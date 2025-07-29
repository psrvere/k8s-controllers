package controllers

import (
	"context"

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
	Scheme *runtime.Scheme
}

func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Pod
	pod := &corev1.Pod{}
	err := r.Get(ctx, req.NamespacedName, pod)
	if err != nil {
		if errors.IsNotFound(err) {
			// Pod not found, probably deleted
			return ctrl.Result{}, err
		}
		// Error reading the object - requeue the request
		return ctrl.Result{}, err
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
	for key, value := range labels {
		podCopy.Labels[key] = value
	}

	// Update the Pod
	return r.Update(ctx, podCopy)
}

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
		labels["image"] = pod.Spec.Containers[0].Image
	}

	// Add custom label to mark this Pod as processed by this controller
	labels["pod-labeller/processed"] = "true"

	return labels
}

func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}
