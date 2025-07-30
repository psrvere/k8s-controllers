package controllers

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
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

type ConfigMapReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// Label to identify ConfigMaps that should be synced
	SyncLabel = "config-syncer/enabled"

	// Annotation to specify target namespace(s)
	TargetNamespaceAnnotation = "config-syncer/target-namespace"

	// Annotation to specify target ConfigMap name (optional)
	TargetNameAnnotation = "config-syncer/target-name"

	// Label to mark synced ConfigMaps
	SyncedLabel = "config-syncer/synced"

	// Annotation to track source ConfigMap
	SourceAnnotation = "config-syncer/source"
)

func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the ConfigMap
	configMap := &corev1.ConfigMap{}
	err := r.Get(ctx, req.NamespacedName, configMap)
	if err != nil {
		if errors.IsNotFound(err) {
			// ConfigMap not found, probably deleted
			log.Info("ConfigMap not found. Skipping reconciliation", "configmap", req.Name, "namespace", req.Namespace)
			return ctrl.Result{}, nil
		}
		// Error reading the object
		log.Error(err, "Failed to get ConfigMap", "configmap", req.Name, "namespace", req.Namespace)
		return ctrl.Result{}, err
	}

	// Check if this ConfigMap should be synced
	if !shouldSyncConfigMap(configMap) {
		log.Info("ConfigMap doesn't have sync label, skipping", "configmap", configMap.Name, "namespace", configMap.Namespace)
		return ctrl.Result{}, nil
	}

	// Get target namespace(s)
	targetNamespaces := getTargetNamespaces(configMap)
	if len(targetNamespaces) == 0 {
		log.Info("No target namespaces specified, skipping", "configmap", configMap.Name, "namespace", configMap.Namespace)
		return ctrl.Result{}, nil
	}

	// Sync to each target namespace
	for _, targetNamespace := range targetNamespaces {
		if err := r.syncConfigMap(ctx, configMap, targetNamespace, log); err != nil {
			log.Error(err, "Failed to sync ConfigMap", "configmap", configMap.Name, "target-namespace", targetNamespace)
			return ctrl.Result{}, err
		}
	}

	log.Info("Successfully synced ConfigMap", "configmap", configMap.Name, "namespace", configMap.Namespace, "target-namespaces", targetNamespaces)
	return ctrl.Result{}, nil
}

func shouldSyncConfigMap(configMap *corev1.ConfigMap) bool {
	if configMap.Labels == nil {
		return false
	}
	_, exists := configMap.Labels[SyncLabel]
	return exists
}

func getTargetNamespaces(configMap *corev1.ConfigMap) []string {
	if configMap.Annotations == nil {
		return nil
	}

	targetNamespaceStr, exists := configMap.Annotations[TargetNamespaceAnnotation]
	if !exists {
		return nil
	}

	// Support comma-separated namespaces
	namespaces := strings.Split(targetNamespaceStr, ",")
	for i, ns := range namespaces {
		namespaces[i] = strings.TrimSpace(ns)
	}

	return namespaces
}

func (r *ConfigMapReconciler) syncConfigMap(ctx context.Context, sourceConfigMap *corev1.ConfigMap, targetNamespace string, log logr.Logger) error {
	// Determine target ConfigMap name
	targetName := getTargetConfigMapName(sourceConfigMap)

	// Check if target ConfigMap already exists
	targetConfigMap := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Name: targetName, Namespace: targetNamespace}, targetConfigMap)

	if err != nil && errors.IsNotFound(err) {
		// Create new ConfigMap
		return r.createTargetConfigMap(ctx, sourceConfigMap, targetNamespace, targetName, log)
	} else if err != nil {
		return err
	}

	// Update existing ConfigMap
	return r.updateTargetConfigMap(ctx, sourceConfigMap, targetConfigMap, log)
}

func getTargetConfigMapName(sourceConfigMap *corev1.ConfigMap) string {
	// Check if custom target name is specified
	if sourceConfigMap.Annotations != nil {
		if targetName, exists := sourceConfigMap.Annotations[TargetNameAnnotation]; exists {
			return targetName
		}
	}

	// Use source name as default
	return sourceConfigMap.Name
}

func (r *ConfigMapReconciler) createTargetConfigMap(ctx context.Context, sourceConfigMap *corev1.ConfigMap, targetNamespace, targetName string, log logr.Logger) error {
	targetConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetName,
			Namespace: targetNamespace,
			Labels: map[string]string{
				SyncedLabel: "true",
			},
			Annotations: map[string]string{
				SourceAnnotation: fmt.Sprintf("%s/%s", sourceConfigMap.Namespace, sourceConfigMap.Name),
			},
		},
		Data:       sourceConfigMap.Data,
		BinaryData: sourceConfigMap.BinaryData,
	}

	log.Info("Creating target ConfigMap", "name", targetName, "namespace", targetNamespace, "source", sourceConfigMap.Name)
	return r.Create(ctx, targetConfigMap)
}

func (r *ConfigMapReconciler) updateTargetConfigMap(ctx context.Context, sourceConfigMap *corev1.ConfigMap, targetConfigMap *corev1.ConfigMap, log logr.Logger) error {
	// Check if update is needed
	if configMapsEqual(sourceConfigMap, targetConfigMap) {
		log.Info("Target ConfigMap is up to date, skipping update", "name", targetConfigMap.Name, "namespace", targetConfigMap.Namespace)
		return nil
	}

	// Update the target ConfigMap
	targetConfigMap.Data = sourceConfigMap.Data
	targetConfigMap.BinaryData = sourceConfigMap.BinaryData

	// Update source annotation
	if targetConfigMap.Annotations == nil {
		targetConfigMap.Annotations = make(map[string]string)
	}
	targetConfigMap.Annotations[SourceAnnotation] = fmt.Sprintf("%s/%s", sourceConfigMap.Namespace, sourceConfigMap.Name)

	log.Info("Updating target ConfigMap", "name", targetConfigMap.Name, "namespace", targetConfigMap.Namespace, "source", sourceConfigMap.Name)
	return r.Update(ctx, targetConfigMap)
}

func configMapsEqual(source, target *corev1.ConfigMap) bool {
	// Compare Data
	if len(source.Data) != len(target.Data) {
		return false
	}
	for k, v := range source.Data {
		if target.Data[k] != v {
			return false
		}
	}

	// Compare BinaryData
	if len(source.BinaryData) != len(target.BinaryData) {
		return false
	}
	for k, v := range source.BinaryData {
		if string(target.BinaryData[k]) != string(v) {
			return false
		}
	}

	return true
}

func (r *ConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				log := log.FromContext(context.Background())
				log.Info("Event: ConfigMap created",
					"name", e.Object.GetName(),
					"namespace", e.Object.GetNamespace(),
					"resourceVersion", e.Object.GetResourceVersion())
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				log := log.FromContext(context.Background())

				oldConfigMap, ok := e.ObjectOld.(*corev1.ConfigMap)
				newConfigMap, ok2 := e.ObjectNew.(*corev1.ConfigMap)

				if ok && ok2 {
					var changes []string

					// Check for data changes
					if !configMapsEqual(oldConfigMap, newConfigMap) {
						changes = append(changes, "data updated")
					}

					// Check for label changes
					if hasSyncLabelChanged(oldConfigMap, newConfigMap) {
						changes = append(changes, "sync label changed")
					}

					// Check for annotation changes
					if hasTargetNamespaceChanged(oldConfigMap, newConfigMap) {
						changes = append(changes, "target namespace annotation changed")
					}

					if len(changes) > 0 {
						log.Info("Event: ConfigMap updated",
							"name", newConfigMap.Name,
							"namespace", newConfigMap.Namespace,
							"changes", changes,
							"resourceVersion", newConfigMap.GetResourceVersion())
					} else {
						log.Info("Event: ConfigMap updated (no significant changes)",
							"name", newConfigMap.Name,
							"namespace", newConfigMap.Namespace,
							"resourceVersion", newConfigMap.GetResourceVersion())
					}
				}

				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				log := log.FromContext(context.Background())
				log.Info("Event: ConfigMap deleted",
					"name", e.Object.GetName(),
					"namespace", e.Object.GetNamespace(),
					"resourceVersion", e.Object.GetResourceVersion())
				return true
			},
		}).
		Complete(r)
}

func hasSyncLabelChanged(old, new *corev1.ConfigMap) bool {
	oldHasLabel := hasSyncLabel(old)
	newHasLabel := hasSyncLabel(new)
	return oldHasLabel != newHasLabel
}

func hasSyncLabel(configMap *corev1.ConfigMap) bool {
	if configMap.Labels == nil {
		return false
	}
	_, exists := configMap.Labels[SyncLabel]
	return exists
}

func hasTargetNamespaceChanged(old, new *corev1.ConfigMap) bool {
	oldTarget := getTargetNamespaces(old)
	newTarget := getTargetNamespaces(new)

	if len(oldTarget) != len(newTarget) {
		return true
	}

	for i, ns := range oldTarget {
		if i >= len(newTarget) || ns != newTarget[i] {
			return true
		}
	}

	return false
}
