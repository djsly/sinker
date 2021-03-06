package manifest

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/plexsystems/sinker/internal/docker"

	promv1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	kubeyaml "github.com/ghodss/yaml"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GetImagesFromKubernetesManifests returns all images found in Kubernetes manifests
// that are located at the specified path.
func GetImagesFromKubernetesManifests(path string) ([]string, error) {
	files, err := getYamlFiles(path)
	if err != nil {
		return nil, fmt.Errorf("get yaml files: %w", err)
	}

	resources, err := splitResources(files)
	if err != nil {
		return nil, fmt.Errorf("split resources: %w", err)
	}

	var imageList []string
	for _, resource := range resources {
		images, err := getImagesFromResource(resource)
		if err != nil {
			return nil, fmt.Errorf("get images from resource: %w", err)
		}

		imageList = append(imageList, images...)
	}

	imageList = dedupeImages(imageList)
	return imageList, nil
}

func getYamlFiles(path string) ([]string, error) {
	var files []string
	err := filepath.Walk(path, func(currentFilePath string, fileInfo os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("walk path: %w", err)
		}

		if fileInfo.IsDir() && fileInfo.Name() == ".git" {
			return filepath.SkipDir
		}

		if fileInfo.IsDir() {
			return nil
		}

		if filepath.Ext(currentFilePath) != ".yaml" && filepath.Ext(currentFilePath) != ".yml" {
			return nil
		}

		files = append(files, currentFilePath)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return files, nil
}

func splitResources(resources []string) ([][]byte, error) {
	var splitResources [][]byte
	for _, resource := range resources {
		resourceContents, err := ioutil.ReadFile(resource)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}

		if bytes.Contains(resourceContents, []byte("---")) {
			splitResources = append(splitResources, splitResourcesBySeparator(resourceContents)...)
			continue
		}

		splitResources = append(splitResources, resourceContents)
	}

	return splitResources, nil
}

func splitResourcesBySeparator(resources []byte) [][]byte {
	var lineBreak string
	if bytes.Contains(resources, []byte("\r\n")) && runtime.GOOS == "windows" {
		lineBreak = "\r\n"
	} else {
		lineBreak = "\n"
	}

	individualResources := bytes.Split(resources, []byte(lineBreak+"---"+lineBreak))
	return individualResources
}

func getImagesFromResource(resource []byte) ([]string, error) {

	// If the resource does not contain a TypeMeta, it will not be a valid
	// Kubernetes resource and can be assumed to have no images.
	var typeMeta metav1.TypeMeta
	if err := kubeyaml.Unmarshal(resource, &typeMeta); err != nil {
		return []string{}, nil
	}

	if typeMeta.Kind == "Prometheus" {
		prometheusImages, err := getPrometheusImages(resource)
		if err != nil {
			return nil, fmt.Errorf("get prometheus images: %w", err)
		}

		return prometheusImages, nil
	}

	if typeMeta.Kind == "Alertmanager" {
		alertmanagerImages, err := getAlertmanagerImages(resource)
		if err != nil {
			return nil, fmt.Errorf("get alertmanager images: %w", err)
		}

		return alertmanagerImages, nil
	}

	if typeMeta.Kind == "Pod" {
		podImages, err := getPodImages(resource)
		if err != nil {
			return nil, fmt.Errorf("get pod images: %w", err)
		}

		return podImages, nil
	}

	type BaseSpec struct {
		Template corev1.PodTemplateSpec `json:"template" protobuf:"bytes,3,opt,name=template"`
	}

	type BaseType struct {
		Spec BaseSpec `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	}

	var contents BaseType
	if err := kubeyaml.Unmarshal(resource, &contents); err != nil {
		return []string{}, nil
	}

	var images []string
	images = append(images, getImagesFromContainers(contents.Spec.Template.Spec.InitContainers)...)
	images = append(images, getImagesFromContainers(contents.Spec.Template.Spec.Containers)...)

	return images, nil
}

func getPrometheusImages(resource []byte) ([]string, error) {
	var prometheus promv1.Prometheus
	if err := kubeyaml.Unmarshal(resource, &prometheus); err != nil {
		return nil, fmt.Errorf("unmarshal prometheus: %w", err)
	}

	var prometheusImage string
	if prometheus.Spec.BaseImage != "" {
		prometheusImage = prometheus.Spec.BaseImage + ":" + prometheus.Spec.Version
	} else {
		prometheusImage = *prometheus.Spec.Image
	}

	var images []string
	images = append(images, getImagesFromContainers(prometheus.Spec.Containers)...)
	images = append(images, getImagesFromContainers(prometheus.Spec.InitContainers)...)
	images = append(images, prometheusImage)

	return images, nil
}

func getAlertmanagerImages(resource []byte) ([]string, error) {
	var alertmanager promv1.Alertmanager
	if err := kubeyaml.Unmarshal(resource, &alertmanager); err != nil {
		return nil, fmt.Errorf("unmarshal alertmanager: %w", err)
	}

	var alertmanagerImage string
	if alertmanager.Spec.BaseImage != "" {
		alertmanagerImage = alertmanager.Spec.BaseImage + ":" + alertmanager.Spec.Version
	} else {
		alertmanagerImage = *alertmanager.Spec.Image
	}

	var images []string
	images = append(images, getImagesFromContainers(alertmanager.Spec.Containers)...)
	images = append(images, getImagesFromContainers(alertmanager.Spec.InitContainers)...)
	images = append(images, alertmanagerImage)

	return images, nil
}

func getPodImages(resource []byte) ([]string, error) {
	var pod corev1.PodTemplateSpec
	if err := kubeyaml.Unmarshal(resource, &pod); err != nil {
		return nil, fmt.Errorf("unmarshal pod: %w", err)
	}

	var images []string
	images = append(images, getImagesFromContainers(pod.Spec.Containers)...)
	images = append(images, getImagesFromContainers(pod.Spec.InitContainers)...)

	return images, nil
}

func getImagesFromContainers(containers []corev1.Container) []string {
	var images []string
	for _, container := range containers {
		images = append(images, container.Image)

		for _, arg := range container.Args {
			if !strings.Contains(arg, ":") || strings.Contains(arg, "=:") {
				continue
			}

			image := strings.Split(arg, "=")[1]

			registryPath := docker.RegistryPath(image)
			if registryPath.Repository() == "" {
				continue
			}

			if strings.Contains(registryPath.Repository(), ":") {
				continue
			}

			images = append(images, image)
		}
	}

	return images
}

func contains(images []string, image string) bool {
	for _, currentImage := range images {
		if strings.EqualFold(currentImage, image) {
			return true
		}
	}

	return false
}
