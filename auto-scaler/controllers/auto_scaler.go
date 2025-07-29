package controllers

import (
	"context"
	"math/rand"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type DeploymentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	mutex    sync.RWMutex
	logCache map[string]time.Time
}

const (
	AutoScaleLabel = "auto-scaler/enabled"

	CPUThresholdHigh = 80.0

	CPUThresholdLow = 20.0

	MinReplicas = 1

	MaxReplicas = 10

	ScalingCooldown = 30 * time.Second
)

func (r *DeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	if isSystemNamespace(req.Namespace) {
		return ctrl.Result{}, nil
	}

	// Fetch the deployment
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, req.NamespacedName, deployment)
	if err != nil {
		if errors.IsNotFound(err) {
			// Deployment not found, probably deleted
			log.Info("Deployment not found. Skipping reconciliation", "deployment", req.Name, "error", err)
			return ctrl.Result{}, nil
		}
		// Error reading the object
		log.Error(err, "Failed to get Deployment", "deployment", req.Name)
		return ctrl.Result{}, err
	}

	// Check if deployment has the auto-scaler label
	if !hasAutoScaleLabel(deployment) {
		log.Info("Deployment doesn't have auto-scaler label, skipping", "deployment", deployment.Name)
		return ctrl.Result{}, nil
	}

	// Check if deployment is ready
	if !isDeploymentReady(deployment) {
		log.Info("Deployment not ready yet, will retry", "deployment", deployment.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// get fake CPU usage for the deployment
	cpuUsage := r.getFakeCPUUsage()
	log.Info("Current CPU usage", "deployment", deployment.Name, "cpu", cpuUsage)

	// Check if scaling is needed
	shouldScale, newReplicas := r.shouldScale(deployment, cpuUsage)
	if !shouldScale {
		log.Info("No scaling needed", "deployment", deployment.Name, "cpu", cpuUsage)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Perform scaling
	if err := r.scaleDeployment(ctx, deployment, newReplicas); err != nil {
		log.Error(err, "Failed to scale deployment", "deployment", deployment.Name, "replicas", newReplicas)
		return ctrl.Result{}, err
	}

	log.Info("Successfully scaled deployment", "deployment", deployment.Name, "replicas", newReplicas)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func hasAutoScaleLabel(deployment *appsv1.Deployment) bool {
	if deployment.Labels == nil {
		return false
	}
	_, exists := deployment.Labels[AutoScaleLabel]
	return exists
}

func isDeploymentReady(deployment *appsv1.Deployment) bool {
	if deployment.Status.ReadyReplicas == 0 {
		return false
	}

	if deployment.Status.ReadyReplicas != *deployment.Spec.Replicas {
		return false
	}
	return true
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

func (r *DeploymentReconciler) getFakeCPUUsage() float64 {
	return rand.Float64()*80 + 10 // CPU usafe between 10-90%
}

func (r *DeploymentReconciler) shouldScale(deployment *appsv1.Deployment, cpuUsage float64) (bool, int32) {
	currentReplicas := *deployment.Spec.Replicas

	// Check if we are in cooldown period
	if r.isInCooldown(deployment.Name) {
		return false, currentReplicas
	}

	// scale up if CPU usage is high
	if cpuUsage > CPUThresholdHigh && currentReplicas < MaxReplicas {
		newReplicas := currentReplicas + 1
		r.setCoolDown(deployment.Name)
		return true, newReplicas
	}

	// scale down if CPU usage is low
	if cpuUsage < CPUThresholdLow && currentReplicas > MinReplicas {
		newReplicas := currentReplicas - 1
		r.setCoolDown(deployment.Name)
		return true, newReplicas
	}

	return false, currentReplicas
}

func (r *DeploymentReconciler) scaleDeployment(ctx context.Context, deployment *appsv1.Deployment, newReplicas int32) error {
	deploymentCopy := deployment.DeepCopy()
	deployment.Spec.Replicas = &newReplicas

	return r.Update(ctx, deploymentCopy)
}

func (r *DeploymentReconciler) isInCooldown(deploymentName string) bool {
	r.mutex.RLock()
	defer r.mutex.Unlock()

	if r.logCache == nil {
		return false
	}

	lastScale, exists := r.logCache[deploymentName]
	if !exists {
		return false
	}

	return time.Since(lastScale) < ScalingCooldown
}

func (r *DeploymentReconciler) setCoolDown(deploymentName string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.logCache == nil {
		r.logCache = make(map[string]time.Time)
	}

	r.logCache[deploymentName] = time.Now()
}

func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		Complete(r)
}
