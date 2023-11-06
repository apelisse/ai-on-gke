package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/rs/zerolog"
	ray "github.com/ray-project/kuberay/ray-operator/apis/ray/v1alpha1"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
)

// represents TPU worker pod
// for multi slice need to track node pools with pods
// node pool -> pod slice
type Pod struct {
    nodePoolName string
    podName string
}

// mapping from pods in a slice to unique TPU_WORKER_ID
var podToId map[Pod]int

// map of node pool names to # of workers created in the slice
var sliceToWorkers map[string]int

// unmarshal raycluster from admission request
func extractRayCluster(admissionReview *admissionv1.AdmissionReview) (*ray.RayCluster, error) {
	if admissionReview.Request.Kind.Kind != "RayCluster" {
		return nil, fmt.Errorf("Expected RayCluster but got %s", admissionReview.Request.Kind.Kind)
	}

	rayCluster := ray.RayCluster{}
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &rayCluster); err != nil {
		return nil, err
	}

	return &rayCluster, nil
}

// // add TPU_WORKER_HOSTNAMES to containers in a ray cluster
func mutateRayCluster(
	admissionReview *admissionv1.AdmissionReview,
) (*admissionv1.AdmissionResponse, error) {
	raycluster, _ := extractRayCluster(admissionReview)
	patches := []map[string]interface{}{}

	for i := 0; i < len(raycluster.Spec.WorkerGroupSpecs); i++ {
		template := raycluster.Spec.WorkerGroupSpecs[i]
		numWorkers := template.Replicas
		
		hostNames := make([]string, *numWorkers)
		for j := 0; j < int(*numWorkers); j++ {
			hostNames[i] = fmt.Sprintf("worker-%d", j)
		}
		joinedHostNames := strings.Join(hostNames, ",")

		for j := 0; j < len(raycluster.Spec.WorkerGroupSpecs[i].Template.Spec.Containers); j++ {
			patch := map[string]interface{}{
				"op": "add",
			}
			container := raycluster.Spec.WorkerGroupSpecs[i].Template.Spec.Containers[j]
			path := fmt.Sprintf("/spec/workergroupspecs/%d/template/spec/containers/%d/env", i, j)
			value := corev1.EnvVar{
				Name:  "TPU_WORKER_HOSTNAMES",
				Value: joinedHostNames,
			}

			if len(container.Env) == 0 {
				patch["path"] = path
				patch["value"] = []corev1.EnvVar{value}
			} else {
				patch["path"] = fmt.Sprintf("%s/-", path)
				patch["value"] = value
			}
			patches = append(patches, patch)
		}
	}
	patchBytes, _ := json.Marshal(patches)

 	// Create AdmissionResponse
	admissionResponse := &admissionv1.AdmissionResponse{
		UID: 	 admissionReview.Request.UID,
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *admissionv1.PatchType {
			pt := admissionv1.PatchTypeJSONPatch
			return &pt
		}(),
	}
	return admissionResponse, nil
}

// unmarshal pod from admission request
func extractPod(admissionReview *admissionv1.AdmissionReview) (*corev1.Pod, error) {
	if admissionReview.Request.Kind.Kind != "Pod" {
		return nil, fmt.Errorf("Expected Pod but got %s", admissionReview.Request.Kind.Kind)
	}

	pod := corev1.Pod{}
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &pod); err != nil {
		return nil, err
	}

	return &pod, nil
}

// add TPU_WORKER_ID to pod environment
func mutatePod(
	admissionReview *admissionv1.AdmissionReview,
) (*admissionv1.AdmissionResponse, error) {
	pod, _ := extractPod(admissionReview)
	nodePoolName := pod.Labels["cloud.google.com/gke-nodepool"]
	key := Pod{pod.GenerateName, nodePoolName}	// ray operator only sets GenerateName field

	// assign to the next unique ID in the pod slice
	tpu_worker_id := sliceToWorkers[nodePoolName]
	if(podToId[key] > 0) {
		tpu_worker_id = podToId[key] // if pod has already been assigned - reuse id
	} else {
		sliceToWorkers[nodePoolName] += 1
	}
	podToId[key] = tpu_worker_id

	// create patch to tell pod how to modify environment
	patches := []map[string]interface{}{}

	// inject the TPU_WORKER_ID environment variable into each container
	for i := 0; i < len(pod.Spec.Containers); i++ {
		path := fmt.Sprintf("/spec/containers/%d/env", i)	// this path must match your pod config
		value := corev1.EnvVar{
			Name:  "TPU_WORKER_ID",
			Value: fmt.Sprint(tpu_worker_id),
		}
		patch := map[string]interface{}{
			"op": "add",
		}
		if(len(pod.Spec.Containers[i].Env) == 0) {
			patch["path"] = path
			patch["value"] = []corev1.EnvVar{value}
		} else {
			patch["path"] = fmt.Sprintf("%s/-", path)
			patch["value"] = value
		}
		patches = append(patches, patch)
	}

	patchBytes, _ := json.Marshal(patches)

	admissionResponse := &admissionv1.AdmissionResponse{
		UID: 	 admissionReview.Request.UID,
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *admissionv1.PatchType {
			pt := admissionv1.PatchTypeJSONPatch
			return &pt
		}(),
	}
	return admissionResponse, nil
}

func init() {
	// mapping from pods in a slice to unique TPU_WORKER_ID
	podToId = make(map[Pod]int)
	sliceToWorkers = make(map[string]int)
}

func main() {
	cert := "/etc/kuberay-tpu-webhook/tls/tls.crt"
	key := "/etc/kuberay-tpu-webhook/tls/tls.key"
	log := zerolog.New(os.Stdout).With().Timestamp().Logger()

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "kuberay-tpu-webhook")
	})
	mux.HandleFunc("/inject", func(w http.ResponseWriter, r *http.Request) {
		admissionReview := &admissionv1.AdmissionReview{}
		if err := json.NewDecoder(r.Body).Decode(admissionReview); err != nil {
			http.Error(w, "Error decoding request body", http.StatusBadRequest)
			return
		}

		if admissionReview.Request.Kind.Kind == "RayCluster" {
			log.Debug().Msg("Received review for RayCluster")
			admissionReview.Response, _ = mutateRayCluster(admissionReview)
			responseBytes, _ := json.Marshal(admissionReview)
			fmt.Fprint(w, string(responseBytes))
			return
		}

		if admissionReview.Request.Kind.Kind == "Pod" {
			log.Debug().Msg("Received review for Pod")
			admissionReview.Response, _ = mutatePod(admissionReview)
			responseBytes, _ := json.Marshal(admissionReview)
			fmt.Fprint(w, string(responseBytes))
			return
		}
	})

	srv := &http.Server{
		Addr:    ":443",
		Handler: mux,
	}

	if err := srv.ListenAndServeTLS(cert, key); err != nil {
		if err == http.ErrServerClosed {
			log.Info().Msg("Server closed")
			return
		}
		log.Fatal().Err(err).Msg("Failed to start server")
	}
}