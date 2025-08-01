package controllers

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type NodeBalancerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// Label to identify nodes that should be balanced
	BalancerLabel = "node-balancer/enabled"

	// Annotation to track rebalancing status
	RebalancingStatusAnnotation = "node-balancer/status"

	// Status values
	StatusBalanced    = "balanced"
	StatusRebalancing = "rebalancing"
	StatusFailed      = "failed"

	// Resource thresholds (percentage)
	CPUThresholdHigh    = 60.0 // Node is overloaded if CPU usage > 60%
	CPUThresholdLow     = 40.0 // Node is underutilized if CPU usage < 40%
	MemoryThresholdHigh = 60.0 // Node is overloaded if memory usage > 60%
	MemoryThresholdLow  = 40.0 // Node is underutilized if memory usage < 40%

	// Event reasons
	NodeRebalancingReason = "NodeRebalancing"

	// Requeue interval
	RequeueInterval = 30 * time.Second
)

// NodeResourceUsage represents the resource usage of a node
type NodeResourceUsage struct {
	NodeName        string
	CPUUsage        float64 // Percentage
	MemoryUsage     float64 // Percentage
	IsOverloaded    bool
	IsUnderutilized bool
	Pods            []corev1.Pod
}

// PodResourceRequest represents the resource requests of a pod
type PodResourceRequest struct {
	PodName       string
	CPURequest    int64 // millicores
	MemoryRequest int64 // bytes
	IsEvictable   bool
}

func (r *NodeBalancerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Get all nodes
	nodeList := &corev1.NodeList{}
	err := r.List(ctx, nodeList)
	if err != nil {
		log.Error(err, "Failed to list nodes")
		return ctrl.Result{}, err
	}

	// Filter nodes that should be balanced
	var targetNodes []corev1.Node
	for _, node := range nodeList.Items {
		if shouldBalanceNode(&node) {
			targetNodes = append(targetNodes, node)
		}
	}

	if len(targetNodes) == 0 {
		log.Info("No nodes with balancer label found")
		return ctrl.Result{RequeueAfter: RequeueInterval}, nil
	}

	// Analyze node resource usage
	nodeUsages, err := r.analyzeNodeResourceUsage(ctx, targetNodes)
	if err != nil {
		log.Error(err, "Failed to analyze node resource usage")
		return ctrl.Result{}, err
	}

	// Check if rebalancing is needed
	overloadedNodes := getOverloadedNodes(nodeUsages)
	underutilizedNodes := getUnderutilizedNodes(nodeUsages)

	if len(overloadedNodes) == 0 || len(underutilizedNodes) == 0 {
		log.Info("No rebalancing needed - no overloaded or underutilized nodes")
		return ctrl.Result{RequeueAfter: RequeueInterval}, nil
	}

	// Perform rebalancing
	err = r.performRebalancing(ctx, overloadedNodes, underutilizedNodes)
	if err != nil {
		log.Error(err, "Failed to perform rebalancing")
		return ctrl.Result{}, err
	}

	log.Info("Rebalancing completed",
		"overloadedNodes", len(overloadedNodes),
		"underutilizedNodes", len(underutilizedNodes))

	return ctrl.Result{RequeueAfter: RequeueInterval}, nil
}

func shouldBalanceNode(node *corev1.Node) bool {
	if node.Labels == nil {
		return false
	}
	_, exists := node.Labels[BalancerLabel]
	return exists
}

func (r *NodeBalancerReconciler) analyzeNodeResourceUsage(ctx context.Context, nodes []corev1.Node) ([]NodeResourceUsage, error) {
	var nodeUsages []NodeResourceUsage

	for _, node := range nodes {
		usage := NodeResourceUsage{
			NodeName: node.Name,
		}

		// Calculate CPU usage
		cpuUsage, err := r.calculateCPUUsage(&node)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate CPU usage for node %s: %w", node.Name, err)
		}
		usage.CPUUsage = cpuUsage

		// Calculate memory usage
		memoryUsage, err := r.calculateMemoryUsage(&node)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate memory usage for node %s: %w", node.Name, err)
		}
		usage.MemoryUsage = memoryUsage

		// Determine if node is overloaded or underutilized
		usage.IsOverloaded = usage.CPUUsage > CPUThresholdHigh || usage.MemoryUsage > MemoryThresholdHigh
		usage.IsUnderutilized = usage.CPUUsage < CPUThresholdLow && usage.MemoryUsage < MemoryThresholdLow

		// Get pods on this node
		pods, err := r.getPodsOnNode(ctx, node.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get pods for node %s: %w", node.Name, err)
		}
		usage.Pods = pods

		nodeUsages = append(nodeUsages, usage)
	}

	return nodeUsages, nil
}

func (r *NodeBalancerReconciler) calculateCPUUsage(node *corev1.Node) (float64, error) {
	// Get node capacity
	cpuCapacity := node.Status.Capacity[corev1.ResourceCPU]
	if cpuCapacity.IsZero() {
		return 0, nil
	}

	// Get node allocatable
	cpuAllocatable := node.Status.Allocatable[corev1.ResourceCPU]
	if cpuAllocatable.IsZero() {
		return 0, nil
	}

	// Get pods on this node
	pods, err := r.getPodsOnNode(context.Background(), node.Name)
	if err != nil {
		return 0, err
	}

	// Calculate total CPU requests
	var totalCPURequests int64
	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			if container.Resources.Requests != nil {
				cpuRequest := container.Resources.Requests[corev1.ResourceCPU]
				totalCPURequests += cpuRequest.MilliValue()
			}
		}
	}

	// Calculate usage percentage
	usagePercentage := float64(totalCPURequests) / float64(cpuAllocatable.MilliValue()) * 100
	return math.Min(usagePercentage, 100.0), nil
}

func (r *NodeBalancerReconciler) calculateMemoryUsage(node *corev1.Node) (float64, error) {
	// Get node capacity
	memoryCapacity := node.Status.Capacity[corev1.ResourceMemory]
	if memoryCapacity.IsZero() {
		return 0, nil
	}

	// Get node allocatable
	memoryAllocatable := node.Status.Allocatable[corev1.ResourceMemory]
	if memoryAllocatable.IsZero() {
		return 0, nil
	}

	// Get pods on this node
	pods, err := r.getPodsOnNode(context.Background(), node.Name)
	if err != nil {
		return 0, err
	}

	// Calculate total memory requests
	var totalMemoryRequests int64
	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			if container.Resources.Requests != nil {
				memoryRequest := container.Resources.Requests[corev1.ResourceMemory]
				totalMemoryRequests += memoryRequest.Value()
			}
		}
	}

	// Calculate usage percentage
	usagePercentage := float64(totalMemoryRequests) / float64(memoryAllocatable.Value()) * 100
	return math.Min(usagePercentage, 100.0), nil
}

func (r *NodeBalancerReconciler) getPodsOnNode(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	err := r.List(ctx, podList, client.MatchingFields{"spec.nodeName": nodeName})
	if err != nil {
		return nil, err
	}

	// Filter out pods that shouldn't be evicted
	var evictablePods []corev1.Pod
	for _, pod := range podList.Items {
		if isPodEvictable(&pod) {
			evictablePods = append(evictablePods, pod)
		}
	}

	return evictablePods, nil
}

func isPodEvictable(pod *corev1.Pod) bool {
	// Don't evict pods that are terminating
	if pod.DeletionTimestamp != nil {
		return false
	}

	// Don't evict pods with specific annotations
	if pod.Annotations != nil {
		if _, exists := pod.Annotations["node-balancer/evictable"]; exists {
			evictable, _ := strconv.ParseBool(pod.Annotations["node-balancer/evictable"])
			return evictable
		}
	}

	// Don't evict system pods
	if pod.Namespace == "kube-system" {
		return false
	}

	// Don't evict pods with node affinity that prevents movement
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
		// Check for required node selectors that would prevent movement
		if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			return false
		}
	}

	return true
}

func getOverloadedNodes(nodeUsages []NodeResourceUsage) []NodeResourceUsage {
	var overloaded []NodeResourceUsage
	for _, usage := range nodeUsages {
		if usage.IsOverloaded {
			overloaded = append(overloaded, usage)
		}
	}
	return overloaded
}

func getUnderutilizedNodes(nodeUsages []NodeResourceUsage) []NodeResourceUsage {
	var underutilized []NodeResourceUsage
	for _, usage := range nodeUsages {
		if usage.IsUnderutilized {
			underutilized = append(underutilized, usage)
		}
	}
	return underutilized
}

func (r *NodeBalancerReconciler) performRebalancing(ctx context.Context, overloadedNodes, underutilizedNodes []NodeResourceUsage) error {
	log := log.FromContext(ctx)

	// For each overloaded node, find pods to evict
	for _, overloadedNode := range overloadedNodes {
		log.Info("Processing overloaded node",
			"node", overloadedNode.NodeName,
			"cpuUsage", fmt.Sprintf("%.2f%%", overloadedNode.CPUUsage),
			"memoryUsage", fmt.Sprintf("%.2f%%", overloadedNode.MemoryUsage))

		// Get evictable pods from overloaded node
		evictablePods := getEvictablePods(overloadedNode.Pods)
		if len(evictablePods) == 0 {
			log.Info("No evictable pods found on overloaded node", "node", overloadedNode.NodeName)
			continue
		}

		// Sort pods by resource usage (evict largest first)
		sortPodsByResourceUsage(evictablePods)

		// Try to evict pods to underutilized nodes
		for _, pod := range evictablePods {
			targetNode := r.findBestTargetNode(underutilizedNodes, &pod)
			if targetNode == nil {
				log.Info("No suitable target node found for pod",
					"pod", pod.Name,
					"namespace", pod.Namespace)
				continue
			}

			err := r.evictPod(ctx, &pod, targetNode.NodeName)
			if err != nil {
				log.Error(err, "Failed to evict pod",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"targetNode", targetNode.NodeName)
				continue
			}

			log.Info("Successfully evicted pod",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"fromNode", overloadedNode.NodeName,
				"toNode", targetNode.NodeName)

			// Update target node usage (simplified - in reality would recalculate)
			targetNode.CPUUsage += getPodCPURequest(&pod)
			targetNode.MemoryUsage += getPodMemoryRequest(&pod)

			// Check if target node is no longer underutilized
			if !targetNode.IsUnderutilized {
				break
			}
		}
	}

	return nil
}

func getEvictablePods(pods []corev1.Pod) []corev1.Pod {
	var evictable []corev1.Pod
	for _, pod := range pods {
		if isPodEvictable(&pod) {
			evictable = append(evictable, pod)
		}
	}
	return evictable
}

func sortPodsByResourceUsage(pods []corev1.Pod) {
	// Simple sorting by total resource requests
	// In a real implementation, you might want more sophisticated sorting
	for i := 0; i < len(pods)-1; i++ {
		for j := i + 1; j < len(pods); j++ {
			podI := getPodTotalResources(&pods[i])
			podJ := getPodTotalResources(&pods[j])
			if podI < podJ {
				pods[i], pods[j] = pods[j], pods[i]
			}
		}
	}
}

func getPodTotalResources(pod *corev1.Pod) int64 {
	var total int64
	for _, container := range pod.Spec.Containers {
		if container.Resources.Requests != nil {
			cpu := container.Resources.Requests[corev1.ResourceCPU]
			memory := container.Resources.Requests[corev1.ResourceMemory]
			total += cpu.MilliValue() + memory.Value()/1024/1024 // Convert to comparable units
		}
	}
	return total
}

func getPodCPURequest(pod *corev1.Pod) float64 {
	var total int64
	for _, container := range pod.Spec.Containers {
		if container.Resources.Requests != nil {
			cpu := container.Resources.Requests[corev1.ResourceCPU]
			total += cpu.MilliValue()
		}
	}
	return float64(total)
}

func getPodMemoryRequest(pod *corev1.Pod) float64 {
	var total int64
	for _, container := range pod.Spec.Containers {
		if container.Resources.Requests != nil {
			memory := container.Resources.Requests[corev1.ResourceMemory]
			total += memory.Value()
		}
	}
	return float64(total)
}

func (r *NodeBalancerReconciler) findBestTargetNode(underutilizedNodes []NodeResourceUsage, pod *corev1.Pod) *NodeResourceUsage {
	var bestNode *NodeResourceUsage
	var bestScore float64

	for i := range underutilizedNodes {
		node := &underutilizedNodes[i]

		// Calculate how much this pod would increase the node's usage
		podCPU := getPodCPURequest(pod)
		podMemory := getPodMemoryRequest(pod)

		// Simple scoring: prefer nodes that will remain underutilized after placement
		newCPUUsage := node.CPUUsage + podCPU
		newMemoryUsage := node.MemoryUsage + podMemory

		// Score based on how well the pod fits (lower score is better)
		score := newCPUUsage + newMemoryUsage

		if bestNode == nil || score < bestScore {
			bestNode = node
			bestScore = score
		}
	}

	return bestNode
}

func (r *NodeBalancerReconciler) evictPod(ctx context.Context, pod *corev1.Pod, targetNodeName string) error {
	log := log.FromContext(ctx)

	// Create a deep copy to avoid race conditions
	podCopy := pod.DeepCopy()

	// Add annotation to mark this pod for rebalancing
	if podCopy.Annotations == nil {
		podCopy.Annotations = make(map[string]string)
	}
	podCopy.Annotations[RebalancingStatusAnnotation] = StatusRebalancing
	podCopy.Annotations["node-balancer/target-node"] = targetNodeName
	podCopy.Annotations["node-balancer/evicted-at"] = time.Now().Format(time.RFC3339)

	// Update the pod
	err := r.Update(ctx, podCopy)
	if err != nil {
		return fmt.Errorf("failed to update pod for eviction: %w", err)
	}

	// Create an event to track the eviction
	err = r.createEvictionEvent(ctx, pod, targetNodeName)
	if err != nil {
		log.Error(err, "Failed to create eviction event")
		// Don't fail the eviction for event creation failure
	}

	// In a real implementation, you would use the Kubernetes eviction API
	// For this demo, we'll just delete the pod and let it be rescheduled
	err = r.Delete(ctx, podCopy)
	if err != nil {
		return fmt.Errorf("failed to delete pod for eviction: %w", err)
	}

	log.Info("Pod evicted for rebalancing",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"targetNode", targetNodeName)

	return nil
}

func (r *NodeBalancerReconciler) createEvictionEvent(ctx context.Context, pod *corev1.Pod, targetNodeName string) error {
	eventName := fmt.Sprintf("%s-rebalancing-event", pod.Name)

	// Check if event already exists
	existingEvent := &corev1.Event{}
	err := r.Get(ctx, types.NamespacedName{Name: eventName, Namespace: pod.Namespace}, existingEvent)
	if err == nil {
		// Event already exists, don't create duplicate
		return nil
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eventName,
			Namespace: pod.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:            "Pod",
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			UID:             pod.UID,
			APIVersion:      pod.APIVersion,
			ResourceVersion: pod.ResourceVersion,
		},
		Reason:         NodeRebalancingReason,
		Message:        fmt.Sprintf("Pod evicted for rebalancing to node %s", targetNodeName),
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
		Count:          1,
		Type:           "Normal",
		Source: corev1.EventSource{
			Component: "node-balancer",
		},
	}

	return r.Create(ctx, event)
}

func (r *NodeBalancerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				log := log.FromContext(context.Background())
				log.Info("Event: Node created", "node", e.Object.GetName())
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				log := log.FromContext(context.Background())
				log.Info("Event: Node updated", "node", e.Object.GetName())
				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				log := log.FromContext(context.Background())
				log.Info("Event: Node deleted", "node", e.Object.GetName())
				return true
			},
		}).
		Complete(r)
}
