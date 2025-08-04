package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
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

type JobHandlerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// Label to identify Jobs that should be handled
	HandlerLabel = "job-handler/enabled"

	// Annotation to track processing status
	ProcessingStatusAnnotation = "job-handler/status"

	// Status values
	StatusPending   = "pending"
	StatusCompleted = "completed"
	StatusFailed    = "failed"

	// Event reason for job processing
	JobProcessingReason = "JobProcessing"

	// Requeue interval
	RequeueInterval = 5 * time.Minute
)

// JobProcessingResult contains the result of job processing
type JobProcessingResult struct {
	IsCompleted   bool
	JobName       string
	Reason        string
	Errors        []string
	Logs          string
	ConfigMapName string
	ShouldDelete  bool // Flag indicating if job should be deleted
}

func (r JobProcessingResult) Error() string {
	if r.IsCompleted {
		return ""
	}
	if len(r.Errors) > 0 {
		return fmt.Sprintf("job %s processing failed: %s - %s",
			r.JobName, r.Reason, strings.Join(r.Errors, "; "))
	}
	return fmt.Sprintf("job %s processing failed: %s", r.JobName, r.Reason)
}

// NewJobProcessingResult creates a new job processing result
func NewJobProcessingResult(isCompleted bool, jobName, reason string, shouldDelete bool, errors ...string) JobProcessingResult {
	return JobProcessingResult{
		IsCompleted:  isCompleted,
		JobName:      jobName,
		Reason:       reason,
		ShouldDelete: shouldDelete,
		Errors:       errors,
	}
}

func (r *JobHandlerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Job
	job := &batchv1.Job{}
	err := r.Get(ctx, req.NamespacedName, job)
	if err != nil {
		if errors.IsNotFound(err) {
			// Job not found, probably deleted
			log.Info("Job not found. Skipping reconciliation")
			return ctrl.Result{}, nil
		}
		// Error reading the object
		log.Error(err, "Failed to get Job")
		return ctrl.Result{}, err
	}

	// Check if this Job should be handled
	if !shouldHandleJob(job) {
		log.Info("Job doesn't have handler label, skipping")
		return ctrl.Result{}, nil
	}

	// Check if job is already processed
	if isJobAlreadyProcessed(job) {
		log.Info("Job already processed, skipping")
		return ctrl.Result{}, nil
	}

	// Check if job is completed (either success or failure)
	if !isJobCompleted(job) {
		log.Info("Job not completed yet, requeuing")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Process the completed job (handles both success and failure)
	result := r.processCompletedJob(ctx, job)

	// Update job with processing results BEFORE deleting it
	updated, err := r.updateJobProcessingStatus(ctx, job, result)
	if err != nil {
		log.Error(err, "Failed to update job processing status")
		return ctrl.Result{}, err
	}

	if updated {
		if result.IsCompleted {
			log.Info("Job processing completed successfully", "configMap", result.ConfigMapName)

			// Delete the job after successful processing and status update
			if result.ShouldDelete {
				err = r.deleteJob(ctx, job)
				if err != nil {
					log.Error(err, "Failed to delete job after processing")
					return ctrl.Result{}, err
				}
				log.Info("Job deleted after successful processing")
			}
		} else {
			log.Info("Job processing failed", "error", result.Error())
		}
	}

	// Requeue after configured interval to check for new jobs
	return ctrl.Result{RequeueAfter: RequeueInterval}, nil
}

func shouldHandleJob(job *batchv1.Job) bool {
	if job.Labels == nil {
		return false
	}
	_, exists := job.Labels[HandlerLabel]
	return exists
}

func isJobAlreadyProcessed(job *batchv1.Job) bool {
	if job.Annotations == nil {
		return false
	}
	status, exists := job.Annotations[ProcessingStatusAnnotation]
	return exists && (status == StatusCompleted || status == StatusFailed)
}

func isJobCompleted(job *batchv1.Job) bool {
	// Check if job has completion time (successful completion)
	if job.Status.CompletionTime != nil {
		return true
	}

	// Check if job has failed conditions
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

func (r *JobHandlerReconciler) processCompletedJob(ctx context.Context, job *batchv1.Job) JobProcessingResult {
	var errors []string

	// Determine if job was successful (has CompletionTime) or failed
	isSuccessful := job.Status.CompletionTime != nil

	// Collect job logs (for both successful and failed jobs)
	logs, err := r.collectJobLogs(ctx, job)
	if err != nil {
		errors = append(errors, fmt.Sprintf("failed to collect logs: %v", err))
	}

	if isSuccessful {
		// Handle successful job completion
		configMapName := fmt.Sprintf("%s-results", job.Name)
		err = r.createResultsConfigMap(ctx, job, logs, configMapName)
		if err != nil {
			errors = append(errors, fmt.Sprintf("failed to create configmap: %v", err))
		}

		if len(errors) > 0 {
			return NewJobProcessingResult(false, job.Name, "processing failed", false, errors...)
		}

		// Don't delete job here - let the caller handle it after status update
		result := NewJobProcessingResult(true, job.Name, "processing successful", true)
		result.Logs = logs // Keep logs for debugging and future extensibility
		result.ConfigMapName = configMapName
		return result
	} else {
		// Handle failed job - just collect logs, don't delete job
		if len(errors) > 0 {
			return NewJobProcessingResult(false, job.Name, "log collection failed", false, errors...)
		}

		result := NewJobProcessingResult(false, job.Name, "job failed", false, "job did not complete successfully")
		result.Logs = logs // Keep logs for debugging and future extensibility
		return result
	}
}

func (r *JobHandlerReconciler) collectJobLogs(ctx context.Context, job *batchv1.Job) (string, error) {
	var allLogs strings.Builder

	// Get pods associated with this job
	podList := &corev1.PodList{}
	err := r.List(ctx, podList, client.MatchingLabels{
		"job-name": job.Name,
	}, client.InNamespace(job.Namespace))
	if err != nil {
		return "", fmt.Errorf("failed to list job pods: %w", err)
	}

	if len(podList.Items) == 0 {
		return "No pods found for job", nil
	}

	// Collect logs from each pod
	for _, pod := range podList.Items {
		podLogs, err := r.getPodLogs(ctx, &pod)
		if err != nil {
			allLogs.WriteString(fmt.Sprintf("Failed to get logs for pod %s: %v\n", pod.Name, err))
			continue
		}
		allLogs.WriteString(fmt.Sprintf("=== Pod: %s ===\n", pod.Name))
		allLogs.WriteString(podLogs)
		allLogs.WriteString("\n")
	}

	return allLogs.String(), nil
}

func (r *JobHandlerReconciler) getPodLogs(ctx context.Context, pod *corev1.Pod) (string, error) {
	// For now, we'll use a simplified approach since controller-runtime client
	// doesn't directly support log retrieval. In a production environment,
	// you would typically:
	// 1. Use a separate Kubernetes client for log retrieval
	// 2. Use a sidecar container for log collection
	// 3. Use a logging service like Fluentd or ELK stack

	// Check if pod is still running or recently terminated
	if pod.Status.Phase == corev1.PodRunning ||
		pod.Status.Phase == corev1.PodSucceeded ||
		pod.Status.Phase == corev1.PodFailed {

		// Get container statuses for more detailed information
		var containerLogs []string
		for _, container := range pod.Status.ContainerStatuses {
			if container.Ready || container.State.Terminated != nil {
				containerLogs = append(containerLogs,
					fmt.Sprintf("Container: %s, State: %s",
						container.Name,
						getContainerState(container.State)))
			}
		}

		if len(containerLogs) > 0 {
			return fmt.Sprintf("Pod: %s\nPhase: %s\n%s",
				pod.Name,
				pod.Status.Phase,
				strings.Join(containerLogs, "\n")), nil
		}
	}

	return fmt.Sprintf("Pod: %s\nPhase: %s\nNote: Actual logs would be retrieved via Kubernetes API in production",
		pod.Name, pod.Status.Phase), nil
}

func getContainerState(state corev1.ContainerState) string {
	if state.Running != nil {
		return "Running"
	}
	if state.Waiting != nil {
		return fmt.Sprintf("Waiting: %s", state.Waiting.Reason)
	}
	if state.Terminated != nil {
		return fmt.Sprintf("Terminated: %s (exit code: %d)",
			state.Terminated.Reason, state.Terminated.ExitCode)
	}
	return "Unknown"
}

func (r *JobHandlerReconciler) createResultsConfigMap(ctx context.Context, job *batchv1.Job, logs, configMapName string) error {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: job.Namespace,
			Labels: map[string]string{
				"job-handler/created": "true",
				"job-name":            job.Name,
			},
			Annotations: map[string]string{
				"job-handler/created-at": time.Now().Format(time.RFC3339),
			},
		},
		Data: map[string]string{
			"job-name":        job.Name,
			"completion-time": job.Status.CompletionTime.Format(time.RFC3339),
			"logs":            logs,
			"status":          "completed",
		},
	}

	err := r.Create(ctx, configMap)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// ConfigMap already exists, update it
			err = r.Update(ctx, configMap)
		}
	}
	return err
}

func (r *JobHandlerReconciler) deleteJob(ctx context.Context, job *batchv1.Job) error {
	// Use propagation policy to ensure dependent objects are also deleted
	propagationPolicy := metav1.DeletePropagationBackground
	return r.Delete(ctx, job, &client.DeleteOptions{
		PropagationPolicy: &propagationPolicy,
	})
}

func (r *JobHandlerReconciler) updateJobProcessingStatus(ctx context.Context, job *batchv1.Job, result JobProcessingResult) (bool, error) {
	// Check if job is already in desired state (idempotency)
	currentStatus := getProcessingStatus(job)

	// Determine if update is needed
	needsUpdate := (result.IsCompleted && currentStatus != StatusCompleted) || (!result.IsCompleted && currentStatus != StatusFailed)

	// If state is already correct, skip update
	if !needsUpdate {
		return false, nil // No changes needed
	}

	// Create a deep copy to avoid race conditions
	jobCopy := job.DeepCopy()

	// Initialize annotations if nil
	if jobCopy.Annotations == nil {
		jobCopy.Annotations = make(map[string]string)
	}

	if result.IsCompleted {
		// Mark job as completed
		jobCopy.Annotations[ProcessingStatusAnnotation] = StatusCompleted

		// Create event to notify about successful processing
		err := r.createProcessingEvent(ctx, job, "Job processing completed successfully", "Normal")
		if err != nil {
			return false, err
		}
	} else {
		// Mark job as failed
		jobCopy.Annotations[ProcessingStatusAnnotation] = StatusFailed

		// Create event to alert about processing failure
		err := r.createProcessingEvent(ctx, job, result.Error(), "Warning")
		if err != nil {
			return false, err
		}
	}

	err := r.Update(ctx, jobCopy)
	return true, err
}

func getProcessingStatus(job *batchv1.Job) string {
	if job.Annotations == nil {
		return ""
	}
	return job.Annotations[ProcessingStatusAnnotation]
}

func (r *JobHandlerReconciler) createProcessingEvent(ctx context.Context, job *batchv1.Job, message, eventType string) error {
	log := log.FromContext(ctx)

	// Check if event already exists to prevent duplicates
	eventName := fmt.Sprintf("%s-processing-event", job.Name)
	existingEvent := &corev1.Event{}
	err := r.Get(ctx, client.ObjectKey{Name: eventName, Namespace: job.Namespace}, existingEvent)
	if err == nil {
		// Event already exists, don't create duplicate
		log.Info("Processing event already exists, skipping creation",
			"job", job.Name,
			"namespace", job.Namespace,
			"eventName", eventName)
		return nil
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eventName,
			Namespace: job.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:            "Job",
			Name:            job.Name,
			Namespace:       job.Namespace,
			UID:             job.UID,
			APIVersion:      job.APIVersion,
			ResourceVersion: job.ResourceVersion,
		},
		Reason:         JobProcessingReason,
		Message:        message,
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
		Count:          1,
		Type:           eventType,
		Source: corev1.EventSource{
			Component: "job-handler",
		},
	}

	err = r.Create(ctx, event)
	if err != nil {
		log.Error(err, "Failed to create processing event", "eventName", eventName)
		return err
	}

	log.Info("Created processing event", "eventName", eventName, "message", message)
	return nil
}

func (r *JobHandlerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&batchv1.Job{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				log := log.FromContext(context.Background())
				log.Info("Event: Job created", "resourceVersion", e.Object.GetResourceVersion())
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				log := log.FromContext(context.Background())

				oldJob, ok := e.ObjectOld.(*batchv1.Job)
				newJob, ok2 := e.ObjectNew.(*batchv1.Job)

				if ok && ok2 {
					var changes []string

					// Check for label changes
					if hasHandlerLabelChanged(oldJob, newJob) {
						changes = append(changes, "handler label changed")
					}

					// Check for completion status changes
					if hasCompletionStatusChanged(oldJob, newJob) {
						changes = append(changes, "completion status changed")
					}

					if len(changes) > 0 {
						log.Info("Event: Job updated", "changes", changes, "resourceVersion", newJob.GetResourceVersion())
					} else {
						log.Info("Event: Job updated (no significant changes)", "resourceVersion", newJob.GetResourceVersion())
					}
				}

				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				// No action needed on delete - job deletion automatically cleans up
				// annotations and events we created
				return false
			},
		}).
		Complete(r)
}

func hasHandlerLabelChanged(old, new *batchv1.Job) bool {
	oldHasLabel := hasHandlerLabel(old)
	newHasLabel := hasHandlerLabel(new)
	return oldHasLabel != newHasLabel
}

func hasHandlerLabel(job *batchv1.Job) bool {
	if job.Labels == nil {
		return false
	}
	_, exists := job.Labels[HandlerLabel]
	return exists
}

func hasCompletionStatusChanged(old, new *batchv1.Job) bool {
	oldCompleted := isJobCompleted(old)
	newCompleted := isJobCompleted(new)
	return oldCompleted != newCompleted
}
