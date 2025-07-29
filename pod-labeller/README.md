# Pod Labeler Controller

A Kubernetes controller that automatically labels newly created Pods based on their metadata. This controller watches for Pod creation events across all namespaces (excluding system namespaces) and applies labels based on the Pod's metadata (e.g., labeling based on the app name).

## Overview

This controller mimics real-world auto-tagging functionality commonly used in CI/CD pipelines for resource organization and management.

## Architecture

The controller follows the standard Kubernetes controller pattern:
- Watches Pod resources across all namespaces
- Filters out system namespaces (kube-system, local-path-storage, etc.)
- Applies labels based on metadata
- Reconciles state changes

### Errors Encountered & Fixes

#### 1. **Invalid Label Values**
**Error**: `"registry.k8s.io/kube-scheduler:v1.32.2": a valid label must be an empty string or consist of alphanumeric characters`
**Fix**: Implemented `sanitizeLabelValue()` function to convert image names to valid label values:
```go
// Replace invalid chars: / → -, : → -, limit to 63 chars
func sanitizeLabelValue(value string) string {
    // Sanitization logic
}
```

#### 2. **Race Conditions on Pod Updates**
**Error**: `"Operation cannot be fulfilled on pods: the object has been modified"`
**Fix**: Wait for Pod to be ready before adding labels:
```go
func isPodReady(pod *corev1.Pod) bool {
    return pod.Status.Phase == corev1.PodRunning && 
           pod.Status.Conditions[corev1.PodReady] == corev1.ConditionTrue
}
```

#### 3. **Endless Requeuing on Deleted Pods**
**Error**: Controller kept trying to reconcile deleted Pods
**Fix**: Don't return error for not found resources:
```go
if errors.IsNotFound(err) {
    return ctrl.Result{}, nil  // Don't requeue
}
```

#### 4. **Multiple Log Entries**
**Error**: Excessive logging due to simultaneous reconciliations
**Fix**: Implemented log deduplication with time-based caching:
```go
func (r *PodReconciler) shouldLogPodNotReady(podName string) bool {
    // Only log once per 5 seconds per Pod
}
```

#### 5. **System Pod Processing**
**Issue**: Controller processing system Pods (kube-system, local-path-storage)
**Fix**: Added namespace filtering:
```go
func isSystemNamespace(namespace string) bool {
    systemNamespaces := []string{"kube-system", "local-path-storage", ...}
    // Skip system namespaces
}
```