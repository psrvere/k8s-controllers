package controllers

import (
	"context"
	"maps"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// PodReconciler reconciles a Pod Object
type PodReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	mutex    sync.RWMutex
	logCache map[string]time.Time
}

func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Skip system namespaces
	if isSystemNamespace(req.Namespace) {
		return ctrl.Result{}, nil
	}

	// Fetch the Pod
	pod := &corev1.Pod{}
	err := r.Get(ctx, req.NamespacedName, pod)
	if err != nil {
		if errors.IsNotFound(err) {
			// Pod not found, probably deleted
			log.Info("Pod not found. Skipping reconciliation", "pod", req.Name, "error", err)
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request
		log.Info("Failed to get Pod. Skipping reconciliation", "pod", req.Name, "error", err)
		return ctrl.Result{}, nil
	}

	// Wait for Pod to be ready before adding labels
	if !isPodReady(pod) {
		// Only log once per 5 seconds for the same Pod
		if r.shouldLogPodNotReady(pod.Name) {
			log.Info("Pod not ready yet, will retry", "pod", pod.Name, "phase", pod.Status.Phase)
		}
		return ctrl.Result{}, nil
	}

	// Check if pod already has our labels
	if hasRequiredLables(pod) {
		log.Info("Pod already has required labels", "pod", pod.Name)
		return ctrl.Result{}, nil
	}

	// Add labels to the Pod
	if err := r.addLabelsToPod(ctx, pod); err != nil {
		log.Error(err, "Failed to add labels to Pod", "pod", pod.Name)
		return ctrl.Result{}, err
	}

	log.Info("Successfullly added labels to Pod", "pod", pod.Name)
	return ctrl.Result{}, nil
}

func hasRequiredLables(pod *corev1.Pod) bool {
	// Check if Pod has app label
	if _, exists := pod.Labels["app"]; exists {
		return true
	}
	return false
}

func (r *PodReconciler) addLabelsToPod(ctx context.Context, pod *corev1.Pod) error {
	// Create a copy of the Pod to modify
	podCopy := pod.DeepCopy()

	// Initialize labels map if it doesn't exist
	if podCopy.Labels == nil {
		podCopy.Labels = make(map[string]string)
	}

	// Add labels based on Pod metadata
	labels := generateLabels(pod)
	maps.Copy(podCopy.Labels, labels)

	// Update the Pod
	return r.Update(ctx, podCopy)
}

// generateLabels creates labels based on Pod Metadata
func generateLabels(pod *corev1.Pod) map[string]string {
	labels := make(map[string]string)

	// Add app label based on Pod name or container name
	if pod.Name != "" {
		labels["app"] = pod.Name
	}

	// Add namespace label
	labels["namesapce"] = pod.Namespace

	// Add image label if container exist
	if len(pod.Spec.Containers) > 0 {
		image := pod.Spec.Containers[0].Image
		// sanitize image name
		sanitizedImage := sanitizeLabelValue(image)
		if sanitizedImage != "" {
			labels["image"] = sanitizedImage
		}
	}

	// Add custom label to mark this Pod as processed by this controller
	labels["pod-labeller/processed"] = "true"

	return labels
}

func isSystemNamespace(namespace string) bool {
	systemNamespaces := []string{
		"kube-system",
		"kube-public",
		"kube-node-lease",
		"local-path-storage",
	}

	for _, sn := range systemNamespaces {
		if namespace == sn {
			return true
		}
	}
	return false
}

// sanitizeLabelValu converts an image name to a valid label value
func sanitizeLabelValue(value string) string {
	// Replace invalid characters with valid ones
	result := ""
	for _, char := range value {
		switch {
		case (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9'):
			result += string(char)
		case char == '/' || char == ':':
			result += "-"
		case char == '_' || char == '.' || char == '-':
			result += string(char)
		default:
			result += "-"
		}
	}

	if len(result) == 0 {
		return "img"
	}

	// Ensure it starts with alphanumric
	if !isAlphanumeric(rune(result[0])) {
		result = "img-" + result
		// Check if limit is exceeded after adding prefix
		if len(result) > 63 {
			result = result[:63]
		}
	}

	// Ensure it ends with alphanumeric
	if !isAlphanumeric(rune(result[len(result)-1])) {
		// Check if limit will be exceeded after adding suffix
		if len(result)+4 > 63 {
			result = result[:59]
		}
		result = result + "-img"
	}

	return result
}

func isAlphanumeric(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// isPodReady checks if the Pod is ready for labelling
func isPodReady(pod *corev1.Pod) bool {
	// Wait for Pod to be in Running phase
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	// Wait for all containers to be ready
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *PodReconciler) shouldLogPodNotReady(podName string) bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.logCache == nil {
		r.logCache = make(map[string]time.Time)
	}

	now := time.Now()
	lastLog, exists := r.logCache[podName]

	if !exists || now.Sub(lastLog) > 5*time.Second {
		r.logCache[podName] = now
		return true
	}
	return false
}

func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}
