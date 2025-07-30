package controllers

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type DeploymentReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	mutex         sync.RWMutex
	cooldownCache map[string]time.Time
}

const (
	AutoScaleLabel = "auto-scaler/enabled"

	CPUThresholdHigh = 60.0

	CPUThresholdLow = 40.0

	MinReplicas = 1

	MaxReplicas = 10

	ScalingCooldown = 20 * time.Second
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
		return ctrl.Result{RequeueAfter: ScalingCooldown}, nil
	}

	// Check if we are in cooldown period
	if r.isInCooldown(deployment.Name) {
		log.Info("In cooldown. Skipping Scaling")
		return ctrl.Result{RequeueAfter: ScalingCooldown}, nil
	}

	// get fake CPU usage for the deployment
	cpuUsage := r.getFakeCPUUsage()
	log.Info("Current CPU usage", "deployment", deployment.Name, "cpu", cpuUsage)

	// Check if scaling is needed
	shouldScale, newReplicas := r.shouldScale(deployment, cpuUsage, log)
	if !shouldScale {
		return ctrl.Result{RequeueAfter: ScalingCooldown}, nil
	}

	// Perform scaling
	if err := r.scaleDeployment(ctx, deployment, newReplicas); err != nil {
		log.Error(err, "Failed to scale deployment", "deployment", deployment.Name, "replicas", newReplicas)
		return ctrl.Result{}, err
	}

	log.Info("Successfully scaled deployment", "deployment", deployment.Name, "replicas", newReplicas)
	return ctrl.Result{RequeueAfter: ScalingCooldown}, nil
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

func (r *DeploymentReconciler) shouldScale(deployment *appsv1.Deployment, cpuUsage float64, log logr.Logger) (bool, int32) {
	currentReplicas := *deployment.Spec.Replicas

	// scale up if CPU usage is high
	if cpuUsage > CPUThresholdHigh && currentReplicas < MaxReplicas {
		newReplicas := currentReplicas + 1
		log.Info("Scaling up", "deployment", deployment.Name, "from", currentReplicas, "to", newReplicas)
		r.setCoolDown(deployment.Name)
		return true, newReplicas
	}

	// scale down if CPU usage is low
	if cpuUsage < CPUThresholdLow && currentReplicas > MinReplicas {
		newReplicas := currentReplicas - 1
		log.Info("Scaling down", "deployment", deployment.Name, "from", currentReplicas, "to", newReplicas)
		r.setCoolDown(deployment.Name)
		return true, newReplicas
	}

	log.Info("None conditions matched")
	return false, currentReplicas
}

func (r *DeploymentReconciler) scaleDeployment(ctx context.Context, deployment *appsv1.Deployment, newReplicas int32) error {
	deploymentCopy := deployment.DeepCopy()
	deploymentCopy.Spec.Replicas = &newReplicas

	return r.Update(ctx, deploymentCopy)
}

func (r *DeploymentReconciler) isInCooldown(deploymentName string) bool {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	if r.cooldownCache == nil {
		return false
	}

	lastScale, exists := r.cooldownCache[deploymentName]
	if !exists {
		return false
	}

	return time.Since(lastScale) < ScalingCooldown
}

func (r *DeploymentReconciler) setCoolDown(deploymentName string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.cooldownCache == nil {
		r.cooldownCache = make(map[string]time.Time)
	}

	r.cooldownCache[deploymentName] = time.Now()
}

func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				log := log.FromContext(context.Background())
				log.Info("Event: Deployment created",
					"name", e.Object.GetName(),
					"namespace", e.Object.GetNamespace(),
					"resourceVersion", e.Object.GetResourceVersion())
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				log := log.FromContext(context.Background())

				// Get the old and new deployment objects
				oldDeployment, ok := e.ObjectOld.(*appsv1.Deployment)
				newDeployment, ok2 := e.ObjectNew.(*appsv1.Deployment)

				if ok && ok2 {
					// Check what changed
					var changes []string

					if oldDeployment.Spec.Replicas != nil && newDeployment.Spec.Replicas != nil {
						if *oldDeployment.Spec.Replicas != *newDeployment.Spec.Replicas {
							changes = append(changes, fmt.Sprintf("replicas: %d -> %d",
								*oldDeployment.Spec.Replicas, *newDeployment.Spec.Replicas))
						}
					}

					if oldDeployment.Status.ReadyReplicas != newDeployment.Status.ReadyReplicas {
						changes = append(changes, fmt.Sprintf("readyReplicas: %d -> %d",
							oldDeployment.Status.ReadyReplicas, newDeployment.Status.ReadyReplicas))
					}

					if oldDeployment.Status.AvailableReplicas != newDeployment.Status.AvailableReplicas {
						changes = append(changes, fmt.Sprintf("availableReplicas: %d -> %d",
							oldDeployment.Status.AvailableReplicas, newDeployment.Status.AvailableReplicas))
					}

					if oldDeployment.Status.UpdatedReplicas != newDeployment.Status.UpdatedReplicas {
						changes = append(changes, fmt.Sprintf("updatedReplicas: %d -> %d",
							oldDeployment.Status.UpdatedReplicas, newDeployment.Status.UpdatedReplicas))
					}

					if len(changes) > 0 {
						log.Info("Event: Deployment updated",
							"name", newDeployment.Name,
							"namespace", newDeployment.Namespace,
							"changes", changes,
							"resourceVersion", newDeployment.GetResourceVersion())
					} else {
						log.Info("Event: Deployment updated (no significant changes)",
							"name", newDeployment.Name,
							"namespace", newDeployment.Namespace,
							"resourceVersion", newDeployment.GetResourceVersion())
					}
				}

				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				log := log.FromContext(context.Background())
				log.Info("Event: Deployment deleted",
					"name", e.Object.GetName(),
					"namespace", e.Object.GetNamespace(),
					"resourceVersion", e.Object.GetResourceVersion())
				return true
			},
		}).
		Complete(r)
}
