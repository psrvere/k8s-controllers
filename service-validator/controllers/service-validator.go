package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type ServiceValidatorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// Label to identify Services that should be validated
	ValidationLabel = "service-validator/enabled"

	// Annotation to track validation status
	ValidationStatusAnnotation = "service-validator/status"

	// Status values
	StatusValid   = "valid"
	StatusInvalid = "invalid"

	// Event reason for validation alerts
	ValidationAlertReason = "ServiceValidationAlert"
)

// ValidationResult contains the result of service validation
type ValidationResult struct {
	IsValid     bool
	ServiceName string
	Reason      string
	Details     []string
}

func (r ValidationResult) Error() string {
	if r.IsValid {
		return ""
	}
	if len(r.Details) > 0 {
		return fmt.Sprintf("service %s validation failed: %s - %s",
			r.ServiceName, r.Reason, strings.Join(r.Details, "; "))
	}
	return fmt.Sprintf("service %s validation failed: %s", r.ServiceName, r.Reason)
}

// NewValidationResult creates a new validation result
func NewValidationResult(isValid bool, serviceName, reason string, details ...string) ValidationResult {
	return ValidationResult{
		IsValid:     isValid,
		ServiceName: serviceName,
		Reason:      reason,
		Details:     details,
	}
}

func (r *ServiceValidatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Service
	service := &corev1.Service{}
	err := r.Get(ctx, req.NamespacedName, service)
	if err != nil {
		if errors.IsNotFound(err) {
			// Service not found, probably deleted
			log.Info("Service not found. Skipping reconciliation", "service", req.Name, "namespace", req.Namespace)
			return ctrl.Result{}, nil
		}
		// Error reading the object
		log.Error(err, "Failed to get Service", "service", req.Name, "namespace", req.Namespace)
		return ctrl.Result{}, err
	}

	// Check if this Service should be validated
	if !shouldValidateService(service) {
		log.Info("Service doesn't have validation label, skipping", "service", service.Name, "namespace", service.Namespace)
		return ctrl.Result{}, nil
	}

	// Validate service endpoints
	result := r.validateServiceEndpoints(ctx, service)

	// Update service with validation results
	updated, err := r.updateServiceValidationStatus(ctx, service, result)
	if err != nil {
		log.Error(err, "Failed to update service validation status", "service", service.Name, "namespace", service.Namespace)
		return ctrl.Result{}, err
	}

	if updated {
		if result.IsValid {
			log.Info("Service validation passed",
				"service", service.Name,
				"namespace", service.Namespace)
		} else {
			log.Info("Service validation failed",
				"service", service.Name,
				"namespace", service.Namespace,
				"error", result.Error())
		}
	} else {
		log.Info("Service validation status already correct, no changes needed",
			"service", service.Name,
			"namespace", service.Namespace,
			"isValid", result.IsValid)
	}

	// Requeue after 5 minutes to check again
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func shouldValidateService(service *corev1.Service) bool {
	if service.Labels == nil {
		return false
	}
	_, exists := service.Labels[ValidationLabel]
	return exists
}

func (r *ServiceValidatorReconciler) validateServiceEndpoints(ctx context.Context, service *corev1.Service) ValidationResult {
	var details []string

	// Get endpoint slices for this service
	endpointSliceList := &discoveryv1.EndpointSliceList{}
	err := r.List(ctx, endpointSliceList, client.MatchingLabels{
		discoveryv1.LabelServiceName: service.Name,
	}, client.InNamespace(service.Namespace))
	if err != nil {
		return NewValidationResult(false, service.Name, "failed to get endpoint slices", err.Error())
	}

	// Check if endpoint slices exist
	if len(endpointSliceList.Items) == 0 {
		return NewValidationResult(false, service.Name, "no endpoint slices found")
	}

	// Validate each endpoint slice
	for i, endpointSlice := range endpointSliceList.Items {
		sliceResult := r.validateEndpointSlice(ctx, endpointSlice, i)
		if !sliceResult.IsValid {
			details = append(details, sliceResult.Error())
		}
	}

	if len(details) > 0 {
		return NewValidationResult(false, service.Name, "endpoint validation failed", details...)
	}

	return NewValidationResult(true, service.Name, "validation successful")
}

func (r *ServiceValidatorReconciler) validateEndpointSlice(ctx context.Context, endpointSlice discoveryv1.EndpointSlice, sliceIndex int) ValidationResult {
	var details []string

	// Check if endpoint slice has endpoints
	if len(endpointSlice.Endpoints) == 0 {
		return NewValidationResult(false, "", fmt.Sprintf("slice %d has no endpoints", sliceIndex))
	}

	// Validate each endpoint in the slice
	for j, endpoint := range endpointSlice.Endpoints {
		if endpoint.TargetRef == nil {
			details = append(details, fmt.Sprintf("slice %d endpoint %d has no target reference", sliceIndex, j))
			continue
		}

		// Validate the target pod
		podResult := r.validateTargetPod(ctx, endpoint.TargetRef, sliceIndex, j)
		if !podResult.IsValid {
			details = append(details, podResult.Error())
		}
	}

	if len(details) > 0 {
		return NewValidationResult(false, "", fmt.Sprintf("slice %d validation failed: %s", sliceIndex, strings.Join(details, "; ")))
	}

	return NewValidationResult(true, "", "slice validation successful")
}

func (r *ServiceValidatorReconciler) validateTargetPod(ctx context.Context, targetRef *corev1.ObjectReference, sliceIndex, endpointIndex int) ValidationResult {
	var details []string

	// Check if target is a Pod
	if targetRef.Kind != "Pod" {
		return NewValidationResult(false, "", fmt.Sprintf("slice %d endpoint %d target is not a Pod (kind: %s)", sliceIndex, endpointIndex, targetRef.Kind))
	}

	// Get the target pod
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: targetRef.Name, Namespace: targetRef.Namespace}, pod)
	if err != nil {
		if errors.IsNotFound(err) {
			return NewValidationResult(false, "", fmt.Sprintf("slice %d endpoint %d target Pod %s not found", sliceIndex, endpointIndex, targetRef.Name))
		} else {
			return NewValidationResult(false, "", fmt.Sprintf("slice %d endpoint %d failed to get target Pod %s: %v", sliceIndex, endpointIndex, targetRef.Name, err))
		}
	}

	// Check if pod is running
	if pod.Status.Phase != corev1.PodRunning {
		details = append(details, fmt.Sprintf("pod %s is not running (phase: %s)", targetRef.Name, pod.Status.Phase))
	}

	// Check if pod has ready condition
	ready := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			ready = true
			break
		}
	}
	if !ready {
		details = append(details, fmt.Sprintf("pod %s is not ready", targetRef.Name))
	}

	if len(details) > 0 {
		return NewValidationResult(false, "", fmt.Sprintf("slice %d endpoint %d validation failed: %s", sliceIndex, endpointIndex, strings.Join(details, "; ")))
	}

	return NewValidationResult(true, "", "pod validation successful")
}

func (r *ServiceValidatorReconciler) updateServiceValidationStatus(ctx context.Context, service *corev1.Service, result ValidationResult) (bool, error) {
	// Check if service is already in desired state (idempotency)
	currentStatus := getValidationStatus(service)

	// Determine if update is needed
	needsUpdate := (result.IsValid && currentStatus != StatusValid) || (!result.IsValid && currentStatus != StatusInvalid)

	// If state is already correct, skip update
	if !needsUpdate {
		return false, nil // No changes needed
	}

	// Create a deep copy to avoid race conditions
	serviceCopy := service.DeepCopy()

	// Initialize annotations if nil
	if serviceCopy.Annotations == nil {
		serviceCopy.Annotations = make(map[string]string)
	}

	if result.IsValid {
		// Mark service as valid
		serviceCopy.Annotations[ValidationStatusAnnotation] = StatusValid
	} else {
		// Mark service as invalid
		serviceCopy.Annotations[ValidationStatusAnnotation] = StatusInvalid

		// Create event to alert about validation failure with full details
		err := r.createValidationEvent(ctx, service, []string{result.Error()})
		if err != nil {
			return false, err
		}
	}

	err := r.Update(ctx, serviceCopy)
	return true, err
}

func getValidationStatus(service *corev1.Service) string {
	if service.Annotations == nil {
		return ""
	}
	return service.Annotations[ValidationStatusAnnotation]
}

func (r *ServiceValidatorReconciler) createValidationEvent(ctx context.Context, service *corev1.Service, errors []string) error {
	log := log.FromContext(ctx)

	// Check if event already exists to prevent duplicates
	eventName := fmt.Sprintf("%s-validation-alert", service.Name)
	existingEvent := &corev1.Event{}
	err := r.Get(ctx, client.ObjectKey{Name: eventName, Namespace: service.Namespace}, existingEvent)
	if err == nil {
		// Event already exists, don't create duplicate
		log.Info("Validation event already exists, skipping creation",
			"service", service.Name,
			"namespace", service.Namespace,
			"eventName", eventName)
		return nil
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eventName,
			Namespace: service.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:            "Service",
			Name:            service.Name,
			Namespace:       service.Namespace,
			UID:             service.UID,
			APIVersion:      service.APIVersion,
			ResourceVersion: service.ResourceVersion,
		},
		Reason:         ValidationAlertReason,
		Message:        fmt.Sprintf("Service %s validation failed: %v", service.Name, errors),
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
		Count:          1,
		Type:           "Warning",
		Source: corev1.EventSource{
			Component: "service-validator",
		},
	}

	err = r.Create(ctx, event)
	if err != nil {
		log.Error(err, "Failed to create validation event",
			"service", service.Name,
			"namespace", service.Namespace,
			"eventName", eventName)
		return err
	}

	log.Info("Created validation event",
		"service", service.Name,
		"namespace", service.Namespace,
		"eventName", eventName,
		"errors", errors)
	return nil
}

func (r *ServiceValidatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				log := log.FromContext(context.Background())
				log.Info("Event: Service created",
					"name", e.Object.GetName(),
					"namespace", e.Object.GetNamespace(),
					"resourceVersion", e.Object.GetResourceVersion())
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				log := log.FromContext(context.Background())

				oldService, ok := e.ObjectOld.(*corev1.Service)
				newService, ok2 := e.ObjectNew.(*corev1.Service)

				if ok && ok2 {
					var changes []string

					// Check for label changes
					if hasValidationLabelChanged(oldService, newService) {
						changes = append(changes, "validation label changed")
					}

					if len(changes) > 0 {
						log.Info("Event: Service updated",
							"name", newService.Name,
							"namespace", newService.Namespace,
							"changes", changes,
							"resourceVersion", newService.GetResourceVersion())
					} else {
						log.Info("Event: Service updated (no significant changes)",
							"name", newService.Name,
							"namespace", newService.Namespace,
							"resourceVersion", newService.GetResourceVersion())
					}
				}

				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				// No action needed on delete - service deletion automatically cleans up
				// annotations and events we created
				return false
			},
		}).
		Complete(r)
}

func hasValidationLabelChanged(old, new *corev1.Service) bool {
	oldHasLabel := hasValidationLabel(old)
	newHasLabel := hasValidationLabel(new)
	return oldHasLabel != newHasLabel
}

func hasValidationLabel(service *corev1.Service) bool {
	if service.Labels == nil {
		return false
	}
	_, exists := service.Labels[ValidationLabel]
	return exists
}
