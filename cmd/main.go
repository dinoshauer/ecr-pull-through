package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	v1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func handleRoot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "ECR Pull-through webhook %q", html.EscapeString(r.URL.Path))
}

var config *Config

func handleMutate(w http.ResponseWriter, r *http.Request) {
	// read the body / request
	body, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
	}

	// mutate the request
	mutated, err := actuallyMutate(body)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
	}

	// and write it back
	w.WriteHeader(http.StatusOK)
	w.Write(mutated)
}

func imageContainsHost(image string) bool {
	parts := strings.Split(image, ":")
	return strings.Contains(parts[0], ".")
}

// Helper function to process Docker Hub official images
func isDockerHubOfficialImage(image string) bool {
	// Handle both "nginx" and "docker.io/nginx" format
	if !strings.Contains(image, "/") {
		return true
	}
	// Handle "docker.io/library/nginx" or "docker.io/nginx" format
	parts := strings.Split(image, "/")
	return len(parts) <= 3 && parts[0] == "docker.io" && (len(parts) == 2 || parts[1] == "library")
}

func actuallyMutate(body []byte) ([]byte, error) {
	// unmarshal request into AdmissionReview struct
	admReview := v1beta1.AdmissionReview{}
	if err := json.Unmarshal(body, &admReview); err != nil {
		return nil, fmt.Errorf("unmarshaling request failed with %s", err)
	}

	var err error
	var pod *corev1.Pod

	responseBody := []byte{}
	ar := admReview.Request
	resp := v1beta1.AdmissionResponse{}

	if ar != nil {
		// get the Pod object and unmarshal it into its struct, if we cannot, we might as well stop here
		if err := json.Unmarshal(ar.Object.Raw, &pod); err != nil {
			return nil, fmt.Errorf("unable unmarshal pod json object %v", err)
		}
		log.Printf("Received request to mutate pod %s:%s", pod.Namespace, pod.ObjectMeta.GenerateName)
		// set response options
		resp.Allowed = true
		resp.UID = ar.UID
		pT := v1beta1.PatchTypeJSONPatch
		resp.PatchType = &pT

		ecrUrl := fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com", config.AwsAccountID, config.AwsRegion)

		// the actual mutation is done by a string in JSONPatch style, i.e. we don't _actually_ modify the object, but
		// tell K8S how it should modifiy it
		p := []map[string]string{}
		// Containers
		for i, container := range pod.Spec.Containers {
			imageReplaced := false
			for _, reg := range config.RegistryList() {
				if strings.HasPrefix(container.Image, reg) {
					newImage := fmt.Sprintf("%s/%s", ecrUrl, container.Image)
					patch := map[string]string{
						"op":    "replace",
						"path":  fmt.Sprintf("/spec/containers/%d/image", i),
						"value": newImage,
					}
					p = append(p, patch)
					imageReplaced = true
					log.Printf("Created patch for container image %s on pod %s:%s, with %s", container.Image, pod.Namespace, pod.ObjectMeta.GenerateName, newImage)
					break // Stop checking other registries if a match is found
				}
			}

			// Check if image is a Docker Hub official image
			if !imageReplaced && !strings.HasPrefix(container.Image, ecrUrl) && !imageContainsHost(container.Image) {
				for _, reg := range config.RegistryList() {
					if reg == "docker.io" {
						// https://kubernetes.io/docs/concepts/containers/images/#image-names
						newImage := fmt.Sprintf("%s/docker.io/%s", ecrUrl, container.Image)

						if isDockerHubOfficialImage(container.Image) {
							newImage = fmt.Sprintf("%s/docker.io/library/%s", ecrUrl, container.Image)
						}

						patch := map[string]string{
							"op":    "replace",
							"path":  fmt.Sprintf("/spec/containers/%d/image", i),
							"value": newImage,
						}
						p = append(p, patch)
						log.Printf("Created patch for container image %s on pod %s:%s, with %s", container.Image, pod.Namespace, pod.ObjectMeta.GenerateName, newImage)
						break
					}
				}
			}
		}
		// InitContainers
		for i, initcontainer := range pod.Spec.InitContainers {
			imageReplaced := false
			for _, reg := range config.RegistryList() {
				if strings.HasPrefix(initcontainer.Image, reg) {
					newImage := fmt.Sprintf("%s/%s", ecrUrl, initcontainer.Image)
					patch := map[string]string{
						"op":    "replace",
						"path":  fmt.Sprintf("/spec/initContainers/%d/image", i),
						"value": newImage,
					}
					p = append(p, patch)
					imageReplaced = true
					log.Printf("Created patch for initcontainer image %s on pod %s:%s, with %s", initcontainer.Image, pod.Namespace, pod.ObjectMeta.GenerateName, newImage)
					break // Stop checking other registries if a match is found
				}
			}

			// Check if image is a Docker Hub official image
			if !imageReplaced && !strings.HasPrefix(initcontainer.Image, ecrUrl) && !imageContainsHost(initcontainer.Image) {
				for _, reg := range config.RegistryList() {
					if reg == "docker.io" {
						// https://kubernetes.io/docs/concepts/containers/images/#image-names
						newImage := fmt.Sprintf("%s/docker.io/%s", ecrUrl, initcontainer.Image)

						if isDockerHubOfficialImage(initcontainer.Image) {
							newImage = fmt.Sprintf("%s/docker.io/library/%s", ecrUrl, initcontainer.Image)
						}

						patch := map[string]string{
							"op":    "replace",
							"path":  fmt.Sprintf("/spec/initContainers/%d/image", i),
							"value": newImage,
						}
						p = append(p, patch)
						log.Printf("Created patch for initcontainer image %s on pod %s:%s, with %s", initcontainer.Image, pod.Namespace, pod.ObjectMeta.GenerateName, newImage)
						break
					}
				}
			}
		}
		// EphemeralContainers
		for i, ephemeralcontainer := range pod.Spec.EphemeralContainers {
			imageReplaced := false
			for _, reg := range config.RegistryList() {
				if strings.HasPrefix(ephemeralcontainer.Image, reg) {
					newImage := fmt.Sprintf("%s/%s", ecrUrl, ephemeralcontainer.Image)
					patch := map[string]string{
						"op":    "replace",
						"path":  fmt.Sprintf("/spec/ephemeralContainers/%d/image", i),
						"value": newImage,
					}
					p = append(p, patch)
					imageReplaced = true
					log.Printf("Created patch for ephemeralcontainer image %s on pod %s:%s, with %s", ephemeralcontainer.Image, pod.Namespace, pod.ObjectMeta.GenerateName, newImage)
					break // Stop checking other registries if a match is found
				}
			}

			// Check if image is a Docker Hub official image
			if !imageReplaced && !strings.HasPrefix(ephemeralcontainer.Image, ecrUrl) && !imageContainsHost(ephemeralcontainer.Image) {
				for _, reg := range config.RegistryList() {
					if reg == "docker.io" {
						// https://kubernetes.io/docs/concepts/containers/images/#image-names
						newImage := fmt.Sprintf("%s/docker.io/%s", ecrUrl, ephemeralcontainer.Image)

						if isDockerHubOfficialImage(ephemeralcontainer.Image) {
							newImage = fmt.Sprintf("%s/docker.io/library/%s", ecrUrl, ephemeralcontainer.Image)
						}

						patch := map[string]string{
							"op":    "replace",
							"path":  fmt.Sprintf("/spec/ephemeralContainers/%d/image", i),
							"value": newImage,
						}
						p = append(p, patch)
						log.Printf("Created patch for ephemeralcontainer image %s on pod %s:%s, with %s", ephemeralcontainer.Image, pod.Namespace, pod.ObjectMeta.GenerateName, newImage)
						break
					}
				}
			}
		}

		// parse the []map into JSON
		resp.Patch, _ = json.Marshal(p)
		if len(p) == 0 {
			log.Printf("No patches created for pod %s:%s", pod.Namespace, pod.ObjectMeta.Name)
		}

		// Success, of course ;)
		resp.Result = &metav1.Status{
			Status: "Success",
		}

		admReview.Response = &resp
		// back into JSON so we can return the finished AdmissionReview w/ Response directly
		// w/o needing to convert things in the http handler
		responseBody, err = json.Marshal(admReview)
		if err != nil {
			return nil, err // untested section
		}
		log.Printf("Successfully mutated pod %s:%s", pod.Namespace, pod.ObjectMeta.Name)
	}

	return responseBody, nil
}

func main() {
	var err error
	config, err = ReadConf("/etc/ecr-pull-through/registries.yaml")
	if err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/mutate", handleMutate)

	s := &http.Server{
		Addr:           ":8443",
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1048576
	}

	// Check for TLS certificate and key files
	_, certErr := os.Stat("/etc/webhook/certs/tls.crt")
	_, keyErr := os.Stat("/etc/webhook/certs/tls.key")

	if os.IsNotExist(certErr) || os.IsNotExist(keyErr) {
		log.Println("Starting server without TLS...")
		log.Fatal(s.ListenAndServe())
	} else {
		log.Println("Starting server with TLS...")
		log.Fatal(s.ListenAndServeTLS("/etc/webhook/certs/tls.crt", "/etc/webhook/certs/tls.key"))
	}
}
