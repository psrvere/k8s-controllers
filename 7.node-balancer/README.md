## 7. Node Resource Balancer
Implement a controller that monitors Nodes and Pods, then reschedules Pods from overloaded Nodes to underutilized ones using eviction (with safeguards). Mirrors real-world cluster optimization in cloud cost management.

### Q: What is `pod.DeletionTimestamp` and when is it set?
A: `pod.DeletionTimestamp` is a field in the Pod metadata that indicates when a Pod deletion was requested. It's automatically set by Kubernetes when:
- A user or controller calls `kubectl delete pod <name>`
- A controller programmatically deletes a Pod using the Kubernetes API
- The Pod is being terminated as part of a cascading deletion

When `DeletionTimestamp` is set, the Pod enters a "Terminating" state and Kubernetes begins the graceful shutdown process.

### Q: What other important timestamps exist in Pod metadata?
A: Pods have several key timestamps:

1. **`CreationTimestamp`**: When the Pod was first created
2. **`DeletionTimestamp`**: When deletion was requested (null if not being deleted)
3. **`StartTime`**: When the Pod's containers actually started running
4. **Container-specific timestamps** in `Status.ContainerStatuses`:
   - `State.Running.StartedAt`: When container started running
   - `State.Terminated.StartedAt`: When container started before termination
   - `State.Terminated.FinishedAt`: When container finished/terminated

### Q: Why do we check `pod.DeletionTimestamp != nil` in the node balancer?
A: We check this to avoid evicting Pods that are already being terminated. This is important because:
- Pods with `DeletionTimestamp` set are already being shut down
- Evicting them again would be redundant and could cause errors
- It prevents race conditions where we try to evict a Pod that's already being deleted
- It's a safety mechanism to avoid interfering with existing termination processes

### Q: How does the graceful termination process work?
A: When `DeletionTimestamp` is set, Kubernetes follows this process:
1. **PreStop hooks** (if configured) are executed
2. **SIGTERM** signal is sent to containers
3. **Grace period** (default 30s) allows containers to shut down gracefully
4. **SIGKILL** is sent if containers don't terminate within grace period
5. Pod is removed from the cluster

### Q: What's the difference between eviction and deletion?
A: 
- **Deletion**: Direct removal of a Pod (sets `DeletionTimestamp`)
- **Eviction**: Kubernetes API that triggers graceful termination with configurable grace period
- **Node balancer approach**: We use deletion for simplicity, but in production you'd use the Eviction API for better control over the termination process

### Q: What is the Kubernetes Eviction API and how does it differ from deletion?
A: The Eviction API is a more sophisticated way to terminate Pods compared to direct deletion:

**Key differences**:
- **Graceful termination**: Uses the Eviction API which respects Pod Disruption Budgets (PDBs)
- **Configurable grace period**: You can specify how long to wait before force termination
- **Safeguards**: Respects cluster policies and disruption budgets
- **Better control**: More granular control over the termination process

**How it works**:
1. **Pod Disruption Budgets**: Checks if evicting the Pod would violate the budget
2. **Grace period**: Specifies grace period (default 30s) for graceful shutdown
3. **PreStop hooks**: Executes any configured PreStop hooks before termination
4. **SIGTERM**: Sends SIGTERM to containers, allowing graceful shutdown
5. **SIGKILL**: If containers don't terminate within grace period, sends SIGKILL

### Q: What are Pod Disruption Budgets (PDBs)?
A: PDBs limit how many pods can be down simultaneously to maintain application availability:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: my-app-pdb
spec:
  minAvailable: 2  # At least 2 pods must be available
  selector:
    matchLabels:
      app: my-app
```

The Eviction API respects PDBs and only evicts pods if it won't violate the budget, ensuring application availability during node rebalancing operations.

### Q: What is the `node-balancer/evictable` annotation and who sets it?
A: The `node-balancer/evictable` annotation is a custom annotation that allows users to explicitly control whether a Pod can be evicted by the node balancer controller.

**Usage examples**:
```yaml
# This pod CAN be evicted (explicitly allowed)
apiVersion: v1
kind: Pod
metadata:
  name: test-app
  annotations:
    node-balancer/evictable: "true"

---
# This pod CANNOT be evicted (protected)
apiVersion: v1
kind: Pod
metadata:
  name: critical-app
  annotations:
    node-balancer/evictable: "false"
```

**Who sets this annotation**:
- **Users/Operators**: Manually add it to their Pod YAML files
- **Other controllers**: Different controllers might set this annotation
- **Admission controllers**: Could be set by webhooks or admission controllers
- **CI/CD pipelines**: Automated deployment scripts could add it

**Default behavior**: If the annotation is not present, the Pod is considered evictable (returns `true`).

### Q: Why was this annotation added to the controller?
A: The annotation was added as a **safety mechanism** and **best practice** for several reasons:

1. **Safety first**: Common pattern in Kubernetes controllers to have explicit opt-in/opt-out mechanisms
2. **Production readiness**: In real-world scenarios, users need control over which Pods can be evicted
3. **Testing flexibility**: Makes testing easier - you can mark specific Pods as evictable for testing
4. **Gradual rollout**: Users can start with a few Pods marked as evictable, then expand.

### Q: What are Node Affinities and how do they affect Pod eviction?
A: Node Affinities are Kubernetes rules that control which nodes a Pod can be scheduled on. They can prevent Pods from being moved between nodes, which affects the node balancer's ability to evict them.

**Types of Node Affinities**:

1. **RequiredDuringSchedulingIgnoredDuringExecution** (Hard requirement):
   ```yaml
   apiVersion: v1
   kind: Pod
   spec:
     affinity:
       nodeAffinity:
         requiredDuringSchedulingIgnoredDuringExecution:
           nodeSelectorTerms:
           - matchExpressions:
             - key: kubernetes.io/e2e-az-name
               operator: In
               values:
               - e2e-az1
               - e2e-az2
   ```
   - **Effect**: Pod MUST be scheduled on nodes matching these criteria
   - **Eviction impact**: Pods with this affinity are NOT evictable (controller returns `false`)
   - **Reason**: Moving these Pods could violate their scheduling requirements

2. **PreferredDuringSchedulingIgnoredDuringExecution** (Soft preference):
   ```yaml
   apiVersion: v1
   kind: Pod
   spec:
     affinity:
       nodeAffinity:
         preferredDuringSchedulingIgnoredDuringExecution:
         - weight: 1
           preference:
             matchExpressions:
             - key: kubernetes.io/e2e-az-name
               operator: In
               values:
               - e2e-az1
   ```
   - **Effect**: Pod PREFERS to be scheduled on nodes matching these criteria
   - **Eviction impact**: Pods with only this affinity ARE evictable (controller returns `true`)
   - **Reason**: These are preferences, not hard requirements

**How the controller handles them**:
```go
// Don't evict pods with node affinity that prevents movement
if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
    // Check for required node selectors that would prevent movement
    if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
        return false  // NOT evictable
    }
}
```

**Why this matters for node balancing**:
- **Hard affinities**: Protect critical Pods that must stay on specific nodes
- **Soft affinities**: Allow Pods to be moved but prefer certain nodes
- **Safety mechanism**: Prevents breaking application requirements during rebalancing
- **Real-world scenarios**: GPU workloads, storage locality, compliance requirements

### Q: What does "IgnoredDuringExecution" mean in Node Affinities?
A: The "IgnoredDuringExecution" suffix in Node Affinities can be confusing. Let me break down what "Scheduling" and "Execution" mean:

**Scheduling vs Execution**:
- **Scheduling**: When the Pod is first placed on a node (initial placement)
- **Execution**: When the Pod is already running on a node (ongoing phase)

**What "IgnoredDuringExecution" means**:
- **During Scheduling**: The affinity rule is enforced (Pod must/prefers to be on nodes matching criteria)
- **During Execution**: The affinity rule is **ignored** (Pod can stay where it is even if node conditions change)

**Why "IgnoredDuringExecution" exists**:
This is a **safety feature** to prevent unnecessary Pod movements:

**Without "IgnoredDuringExecution"**:
- If a node's labels change after a Pod is scheduled, Kubernetes might try to move the Pod
- This could cause unnecessary Pod restarts and disruption

**With "IgnoredDuringExecution"**:
- Once a Pod is scheduled and running, it stays put
- Even if the node's labels change, the Pod continues running
- This prevents unnecessary Pod movements

**Example**:
```yaml
spec:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: zone
            operator: In
            values:
            - us-west-2a
```

**What happens**:
1. **Scheduling**: Pod MUST be placed on a node in us-west-2a
2. **Execution**: If someone changes the node's zone label later, the Pod keeps running (ignored)

### Q: Do Node Affinities prevent Pod eviction by controllers?
A: **No!** Node Affinities are **scheduling constraints**, not **eviction constraints**. They only affect the Kubernetes scheduler's initial placement decisions.

**Pods CAN still be evicted** even with Node Affinities because:

1. **Node Affinities only affect scheduling** - where Pods are initially placed
2. **Controllers can still evict** - our node balancer can delete Pods regardless of affinities
3. **Manual actions bypass affinities** - users and controllers can still evict Pods

**Example scenario**:
```yaml
# Pod with hard affinity
spec:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: zone
            operator: In
            values:
            - us-west-2a
```

**What happens**:
1. **Scheduling**: Pod must be placed in us-west-2a
2. **Running**: Pod stays on its node even if zone label changes
3. **Our controller**: Can still evict this Pod if the node is overloaded
4. **Rescheduling**: When the Pod is recreated, it must go back to us-west-2a

**Key point**: Node Affinities tell Kubernetes where to place Pods initially, but don't prevent manual eviction by controllers or users. Our node balancer can evict any Pod (except those we explicitly protect), regardless of their Node Affinities.

### Q: Why does the node balancer calculate "requests" instead of actual resource usage?
A: The node balancer calculates **resource requests** (what Pods have reserved) rather than actual resource usage for several important reasons:

**What are Resource Requests?**
```yaml
spec:
  containers:
  - name: app
    resources:
      requests:
        cpu: "500m"      # Container declares it needs 500 millicores
        memory: "512Mi"   # Container declares it needs 512 MiB
      limits:
        cpu: "1000m"      # Container can use up to 1000 millicores
        memory: "1Gi"     # Container can use up to 1 GiB
```

**Requests vs Actual Usage**:
- **Requests**: What the container **reserves/claims** it needs (stable, predictable)
- **Actual Usage**: What the container **actually consumes** (fluctuates, requires metrics)

**Why the controller uses requests**:
1. **Scheduling-based approach**: We're looking at what Pods have **reserved** on nodes
2. **Predictable**: Requests are stable, actual usage fluctuates
3. **Scheduler logic**: This matches how Kubernetes scheduler thinks about resource allocation
4. **Node capacity**: We compare against node's **allocatable** resources (what's available for Pods)

**For real CPU usage**, you'd need:
- Metrics from Prometheus/Heapster
- Node-level monitoring
- Container runtime metrics

But for node balancing based on **scheduling decisions**, using requests makes sense because that's what the scheduler considers when placing Pods.

### Q: What are millicores and how are CPU cores divided?
A: **Millicores** are a way to express fractional CPU resources in Kubernetes. They represent 1/1000th of a CPU core.

**CPU Core Division**:
**1 CPU Core = 1000 millicores**

| Expression | Millicores | Meaning |
|------------|------------|---------|
| `1` | 1000m | 1 full CPU core |
| `0.5` | 500m | Half a CPU core |
| `0.1` | 100m | 1/10th of a CPU core |
| `0.001` | 1m | 1/1000th of a CPU core |

**Examples**:
```yaml
spec:
  containers:
  - name: app
    resources:
      requests:
        cpu: "1000m"    # 1 full CPU core
        cpu: "500m"      # Half a CPU core
        cpu: "100m"      # 1/10th of a CPU core
        cpu: "250m"      # Quarter of a CPU core
```

### Q: How are CPU cores actually divided into millicores on a node?
A: **Physical CPU cores** on a node are **time-shared** among containers, not physically divided.

**The Reality**:
1. **No Physical Division**: You can't physically split a CPU core into smaller pieces
2. **Time Slicing**: The operating system (Linux) uses **time slicing** to share CPU cores
3. **Time Slices**: Each container gets a **time slice** to run on the CPU, and the scheduler switches between containers rapidly

**Example with 1 CPU Core**:
```
Time: 0ms    100ms   200ms   300ms   400ms
      |       |       |       |       |
      Container A     Container B     Container A
      (500m)          (300m)          (200m)
```

**What happens**:
- Container A (500m) gets ~50% of the CPU time
- Container B (300m) gets ~30% of the CPU time  
- Container C (200m) gets ~20% of the CPU time
- They all share the same physical core through time slicing

**How Linux Scheduler Works**:
- **CFS (Completely Fair Scheduler)**: Linux uses CFS to ensure fair CPU time allocation
- **Proportional Allocation**: Each container gets CPU time proportional to its millicore request
- **Time Quantum**: Scheduler gives each container a small time slice (typically 1-10ms)

**Example Node with 4 Cores**:
```
Node: 4 physical CPU cores = 4000 millicores

Pods running:
- Pod A: 1000m (1 core) - gets 25% of total CPU time
- Pod B: 500m (0.5 core) - gets 12.5% of total CPU time  
- Pod C: 1500m (1.5 cores) - gets 37.5% of total CPU time
- Pod D: 1000m (1 core) - gets 25% of total CPU time

Total requested: 4000m (100% of node capacity)
```

**Why This Matters for Node Balancing**:
- Our controller calculates how much CPU time has been **reserved** (millicore requests)
- This tells us if a node is "overcommitted" from a scheduling perspective
- Millicores represent **scheduled CPU time allocation**, not physical CPU division

