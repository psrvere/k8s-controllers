package controllers

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type SecretRotatorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// Label to identify Secrets that should be monitored for rotation
	RotationLabel = "secret-rotator/enabled"

	// Annotation to specify rotation threshold in days
	RotationThresholdAnnotation = "secret-rotator/rotation-threshold-days"

	// Annotation to track last rotation check
	LastRotationCheckAnnotation = "secret-rotator/last-check"

	// Annotation to mark secrets that need rotation
	NeedsRotationAnnotation = "secret-rotator/needs-rotation"

	// Annotation to specify test age in days (test mode only)
	TestAgeAnnotation = "secret-rotator/test-age-days"

	// Default rotation threshold in days
	DefaultRotationThreshold = 90

	// Event reason for rotation alerts
	RotationAlertReason = "SecretRotationAlert"
)

func (r *SecretRotatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Secret
	secret := &corev1.Secret{}
	err := r.Get(ctx, req.NamespacedName, secret)
	if err != nil {
		if errors.IsNotFound(err) {
			// Secret not found, probably deleted
			log.Info("Secret not found. Skipping reconciliation", "secret", req.Name, "namespace", req.Namespace)
			return ctrl.Result{}, nil
		}
		// Error reading the object
		log.Error(err, "Failed to get Secret", "secret", req.Name, "namespace", req.Namespace)
		return ctrl.Result{}, err
	}

	// Check if this Secret should be monitored for rotation
	if !shouldMonitorSecret(secret) {
		log.Info("Secret doesn't have rotation label, skipping", "secret", secret.Name, "namespace", secret.Namespace)
		return ctrl.Result{}, nil
	}

	// Check if secret needs rotation
	needsRotation, age, threshold := r.checkSecretRotation(secret)

	// Batch update secret with all changes in one operation
	updated, err := r.batchUpdateSecret(ctx, secret, needsRotation, age, threshold)
	if err != nil {
		log.Error(err, "Failed to batch update secret", "secret", secret.Name, "namespace", secret.Namespace)
		return ctrl.Result{}, err
	}

	if updated {
		if needsRotation {
			log.Info("Secret marked for rotation",
				"secret", secret.Name,
				"namespace", secret.Namespace,
				"age", age,
				"threshold", threshold)
		} else {
			log.Info("Secret is within rotation threshold",
				"secret", secret.Name,
				"namespace", secret.Namespace,
				"age", age,
				"threshold", threshold)
		}
	} else {
		log.Info("Secret already in correct state, no changes needed",
			"secret", secret.Name,
			"namespace", secret.Namespace,
			"age", age,
			"threshold", threshold)
	}

	// Requeue after 24 hours to check again, with backoff to prevent conflicts
	return ctrl.Result{RequeueAfter: 24 * time.Hour}, nil
}

func shouldMonitorSecret(secret *corev1.Secret) bool {
	if secret.Labels == nil {
		return false
	}
	_, exists := secret.Labels[RotationLabel]
	return exists
}

func (r *SecretRotatorReconciler) checkSecretRotation(secret *corev1.Secret) (bool, time.Duration, time.Duration) {
	// Get rotation threshold
	thresholdDays := getRotationThreshold(secret)
	threshold := time.Duration(thresholdDays) * 24 * time.Hour

	// Calculate secret age
	var age time.Duration
	if os.Getenv("TEST_MODE") == "true" {
		// Test mode: Use simulated time from annotation
		age = r.calculateTestAge(secret)
	} else {
		// Production mode: Use real time since creation
		age = time.Since(secret.CreationTimestamp.Time)
	}

	return age > threshold, age, threshold
}

func (r *SecretRotatorReconciler) batchUpdateSecret(ctx context.Context, secret *corev1.Secret, needsRotation bool, age, threshold time.Duration) (bool, error) {
	// Check if secret is already in desired state (idempotency)
	currentNeedsRotation := secret.Annotations != nil && secret.Annotations[NeedsRotationAnnotation] == "true"

	// If state is already correct, skip update
	if currentNeedsRotation == needsRotation {
		// Only update last check annotation if needed
		if secret.Annotations == nil || secret.Annotations[LastRotationCheckAnnotation] == "" {
			secretCopy := secret.DeepCopy()
			if secretCopy.Annotations == nil {
				secretCopy.Annotations = make(map[string]string)
			}
			secretCopy.Annotations[LastRotationCheckAnnotation] = time.Now().Format(time.RFC3339)
			err := r.Update(ctx, secretCopy)
			return true, err
		}
		return false, nil // No changes needed
	}

	// Create a deep copy to avoid race conditions
	secretCopy := secret.DeepCopy()

	// Initialize annotations if nil
	if secretCopy.Annotations == nil {
		secretCopy.Annotations = make(map[string]string)
	}

	// Always update last check annotation
	secretCopy.Annotations[LastRotationCheckAnnotation] = time.Now().Format(time.RFC3339)

	if needsRotation {
		// Mark secret as needing rotation
		secretCopy.Annotations[NeedsRotationAnnotation] = "true"

		// Update the secret first
		if err := r.Update(ctx, secretCopy); err != nil {
			return false, err
		}

		// Create event to alert about rotation
		err := r.createRotationEvent(ctx, secret, age, threshold)
		return true, err
	} else {
		// Remove rotation annotation if it exists
		if _, exists := secretCopy.Annotations[NeedsRotationAnnotation]; exists {
			delete(secretCopy.Annotations, NeedsRotationAnnotation)
		}

		err := r.Update(ctx, secretCopy)
		return true, err
	}
}

func (r *SecretRotatorReconciler) calculateTestAge(secret *corev1.Secret) time.Duration {
	// Use annotation to specify test age in days
	if secret.Annotations != nil {
		if testAgeStr, exists := secret.Annotations[TestAgeAnnotation]; exists {
			if days, err := strconv.Atoi(testAgeStr); err == nil {
				return time.Duration(days) * 24 * time.Hour
			}
		}
	}

	// Default test age: 1 day if no annotation specified
	return 24 * time.Hour
}

func getRotationThreshold(secret *corev1.Secret) int {
	if secret.Annotations == nil {
		return DefaultRotationThreshold
	}

	thresholdStr, exists := secret.Annotations[RotationThresholdAnnotation]
	if !exists {
		return DefaultRotationThreshold
	}

	// Parse threshold (for simplicity, we'll use default if parsing fails)
	threshold, err := strconv.Atoi(thresholdStr)
	if err != nil {
		return DefaultRotationThreshold
	}

	return threshold
}

func (r *SecretRotatorReconciler) createRotationEvent(ctx context.Context, secret *corev1.Secret, age, threshold time.Duration) error {
	// Check if event already exists to prevent duplicates
	eventName := fmt.Sprintf("%s-rotation-alert", secret.Name)
	existingEvent := &corev1.Event{}
	err := r.Get(ctx, client.ObjectKey{Name: eventName, Namespace: secret.Namespace}, existingEvent)
	if err == nil {
		// Event already exists, don't create duplicate
		return nil
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eventName,
			Namespace: secret.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:            "Secret",
			Name:            secret.Name,
			Namespace:       secret.Namespace,
			UID:             secret.UID,
			APIVersion:      secret.APIVersion,
			ResourceVersion: secret.ResourceVersion,
		},
		Reason:         RotationAlertReason,
		Message:        fmt.Sprintf("Secret %s is %v old and exceeds rotation threshold of %v", secret.Name, age, threshold),
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
		Count:          1,
		Type:           "Warning",
		Source: corev1.EventSource{
			Component: "secret-rotator",
		},
	}

	return r.Create(ctx, event)
}

func (r *SecretRotatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				log := log.FromContext(context.Background())
				log.Info("Event: Secret created",
					"name", e.Object.GetName(),
					"namespace", e.Object.GetNamespace(),
					"resourceVersion", e.Object.GetResourceVersion())
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				log := log.FromContext(context.Background())

				oldSecret, ok := e.ObjectOld.(*corev1.Secret)
				newSecret, ok2 := e.ObjectNew.(*corev1.Secret)

				if ok && ok2 {
					var changes []string

					// Check for label changes
					if hasRotationLabelChanged(oldSecret, newSecret) {
						changes = append(changes, "rotation label changed")
					}

					// Check for annotation changes
					if hasRotationThresholdChanged(oldSecret, newSecret) {
						changes = append(changes, "rotation threshold annotation changed")
					}

					if len(changes) > 0 {
						log.Info("Event: Secret updated",
							"name", newSecret.Name,
							"namespace", newSecret.Namespace,
							"changes", changes,
							"resourceVersion", newSecret.GetResourceVersion())
					} else {
						log.Info("Event: Secret updated (no significant changes)",
							"name", newSecret.Name,
							"namespace", newSecret.Namespace,
							"resourceVersion", newSecret.GetResourceVersion())
					}
				}

				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				log := log.FromContext(context.Background())
				log.Info("Event: Secret deleted",
					"name", e.Object.GetName(),
					"namespace", e.Object.GetNamespace(),
					"resourceVersion", e.Object.GetResourceVersion())
				return true
			},
		}).
		Complete(r)
}

func hasRotationLabelChanged(old, new *corev1.Secret) bool {
	oldHasLabel := hasRotationLabel(old)
	newHasLabel := hasRotationLabel(new)
	return oldHasLabel != newHasLabel
}

func hasRotationLabel(secret *corev1.Secret) bool {
	if secret.Labels == nil {
		return false
	}
	_, exists := secret.Labels[RotationLabel]
	return exists
}

func hasRotationThresholdChanged(old, new *corev1.Secret) bool {
	oldThreshold := getRotationThreshold(old)
	newThreshold := getRotationThreshold(new)
	return oldThreshold != newThreshold
}
