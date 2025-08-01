# Service Validator Controller

Create a controller that watches Services and validates their endpoints, ensuring they point to valid Pods; if not, it adds a status condition or logs errors. Relates to real-world service discovery reliability in microservices architectures.

## Features

- **Service Monitoring**: Watches Services with the `service-validator/enabled` label
- **Endpoint Validation**: Validates that service endpoints exist and point to valid Pods
- **Pod Health Checks**: Ensures target Pods are running and ready
- **Status Tracking**: Updates service annotations with validation status
- **Event Generation**: Creates Kubernetes events for validation failures
- **Idempotent Operations**: Prevents unnecessary updates and duplicate events

## Usage

### 1. Label Services for Validation

Add the validation label to Services you want to monitor:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-service
  labels:
    service-validator/enabled: "true"
spec:
  # ... service spec
```

### 2. Check Validation Status

The controller adds annotations to track validation status:

- `service-validator/status`: "valid" or "invalid"

### 3. Monitor Events

Validation failures create Kubernetes events with reason `ServiceValidationAlert`.

## Controller Logic

### Validation Process

1. **Service Discovery**: Only validates Services with the `service-validator/enabled` label
2. **EndpointSlice Retrieval**: Fetches the corresponding EndpointSlices for the service
3. **Endpoint Validation**: Checks each endpoint slice for valid endpoints
4. **Pod Validation**: For each endpoint, validates the target Pod:
   - Pod exists
   - Pod is running (Phase = Running)
   - Pod is ready (Ready condition = True)

### Error Scenarios

- No endpoint slices found for service
- Endpoint slice has no endpoints
- Endpoint has no target reference
- Target is not a Pod
- Target Pod not found
- Target Pod not running
- Target Pod not ready

**Q: Why validate service endpoints?**

A: In microservices architectures, service discovery is critical. If a Service points to non-existent or unhealthy Pods, requests will fail. This controller provides proactive monitoring to detect and alert on such issues.

**Q: How does this relate to real-world service discovery?**

A: Service discovery systems like Kubernetes DNS rely on Services pointing to valid endpoints. This controller ensures the service discovery layer remains reliable by validating endpoint health.

**Q: What's the difference between this and Kubernetes health checks?**

A: Kubernetes health checks (liveness/readiness probes) run inside Pods. This controller validates from the outside - ensuring Services point to healthy Pods, which is a different layer of validation.

**Q: How often does the controller revalidate?**

A: The controller requeues after 5 minutes to recheck validation status, providing regular monitoring without overwhelming the API server.

**Q: Can this controller fix validation issues?**

A: No, this is a monitoring controller. It detects and reports issues but doesn't attempt to fix them. The goal is to provide visibility into service discovery problems so operators can address them.

**Q: Why did we switch from corev1.Endpoints to discoveryv1.EndpointSlice?**

A: The `corev1.Endpoints` API was deprecated in Kubernetes v1.33+ in favor of `discoveryv1.EndpointSlice`. EndpointSlices provide better scalability, performance, and more granular control over endpoint management. The new API supports larger clusters and provides better resource efficiency.

**Q: What's the difference between Endpoints and EndpointSlice?**

A: Endpoints is the legacy API that stores all endpoints for a service in a single resource. EndpointSlice splits endpoints across multiple resources, allowing for better scalability and more efficient updates. Each slice can contain up to 100 endpoints, and multiple slices can exist for a single service.

**Q: How does the controller find the correct EndpointSlices for a service?**

A: The controller uses the `discoveryv1.LabelServiceName` label to filter EndpointSlices. This label is automatically added by Kubernetes to all EndpointSlices associated with a service, making it easy to find all slices for a specific service.

**Q: What's the difference between Pod Status.Phase and Status.Conditions?**

A: Pod Status.Phase is a single overall state (Pending, Running, Succeeded, Failed, Unknown), while Status.Conditions is an array of specific conditions that provide detailed information. Each condition has a Type field like PodReady, PodScheduled, PodInitialized, etc.

**Q: What are PodReady and PodScheduled?**

A: PodReady and PodScheduled are specific condition types within the Status.Conditions array, not separate status fields. PodReady indicates if the pod is ready to serve traffic, while PodScheduled indicates if the pod has been scheduled to a node. They provide granular information about different aspects of the pod's state.

**Q: Can you give real-world examples of Phase and Conditions combinations?**

A: Here are some examples:

**Healthy Pod:**
- Phase: Running
- Conditions: PodReady: True, PodScheduled: True

**Pod Starting Up:**
- Phase: Running  
- Conditions: PodReady: False, PodScheduled: True

**Pod Resource Issues:**
- Phase: Pending
- Conditions: PodScheduled: False, PodReady: False

**Pod Crashed:**
- Phase: Running
- Conditions: PodReady: False, PodScheduled: True

Our controller validates that pods are both Running (phase) AND Ready (condition) to ensure they can serve traffic.

**Q: What specific changes were made to implement production error handling?**

A: We refactored the controller from a simple `[]string` approach to a structured error handling system:

**Before (Simple approach):**
```go
func validateServiceEndpoints() (bool, []string) {
    var errors []string
    if len(endpoints) == 0 {
        errors = append(errors, "No endpoints found")
    }
    return len(errors) == 0, errors
}
```

**After (Production approach):**
```go
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

func validateServiceEndpoints() ValidationResult {
    if len(endpoints) == 0 {
        return NewValidationResult(false, serviceName, "no endpoints found")
    }
    return NewValidationResult(true, serviceName, "validation successful")
}
```

**Q: How does the controller handle different event types?**

A: The controller uses event filtering to optimize performance and reduce unnecessary processing:

**Event Filtering Strategy:**
- **Create Events**: Processed to validate new services
- **Update Events**: Processed to revalidate when services change (especially validation label changes)
- **Delete Events**: Skipped entirely - no cleanup needed since service deletion automatically removes our annotations and events

**Why skip delete events?**
- Service deletion automatically cleans up all associated resources
- Our annotations (`service-validator/status`) are deleted with the service
- Any events we created are garbage collected by Kubernetes
- No manual cleanup is required, reducing controller complexity and improving performance

**Q: Why did we improve the readiness probe from a simple ping to actual resource checks?**

A: The original `healthz.Ping` check only verified the HTTP server was running, but didn't ensure the controller could actually perform its validation work. We implemented three specific checks that verify the controller's core functionality.

**Q: What are the three readiness checks and why are they important?**

A: We implemented three readiness checks that verify the controller can access the resources it needs:

1. **Services Check** - Basic connectivity & permissions:
   ```go
   serviceList := &corev1.ServiceList{}
   if err := mgr.GetClient().List(context.Background(), serviceList, &client.ListOptions{Limit: 1}); err != nil {
       return fmt.Errorf("failed to list services: %w", err)
   }
   ```

2. **EndpointSlices Check** - Core functionality dependency:
   ```go
   endpointSliceList := &discoveryv1.EndpointSliceList{}
   if err := mgr.GetClient().List(context.Background(), endpointSliceList, &client.ListOptions{Limit: 1}); err != nil {
       return fmt.Errorf("failed to list endpoint slices: %w", err)
   }
   ```

3. **Pods Check** - Validation logic dependency:
   ```go
   podList := &corev1.PodList{}
   if err := mgr.GetClient().List(context.Background(), podList, &client.ListOptions{Limit: 1}); err != nil {
       return fmt.Errorf("failed to list pods: %w", err)
   }
   ```

**Q: What real-world scenarios would cause these readiness checks to fail?**

A: Here are common failure scenarios engineers face:

**Services Check Failures:**
- **RBAC Issues**: Service account missing `services.list` permission
- **Network Connectivity**: Controller can't reach Kubernetes API server
- **API Server Issues**: API server overloaded or temporarily unavailable
- **Authentication Problems**: Service account token expired or invalid

**EndpointSlices Check Failures:**
- **Missing RBAC**: Service account lacks `endpointslices.discovery.k8s.io` permissions
- **API Version Issues**: EndpointSlices API not available in older clusters
- **Discovery API Problems**: The discovery.k8s.io API group having issues
- **Resource Quotas**: Cluster exhausted API request quotas

**Pods Check Failures:**
- **RBAC Permissions**: Missing `pods.list` permission in service account
- **Namespace Access**: Controller can't access pods in target namespaces
- **Resource Pressure**: API server throttling pod list requests
- **Network Segmentation**: Controller blocked by network policies

**Q: Why is this better than a simple ping check?**

A: The improved readiness check provides several engineering benefits:

1. **Early Detection**: Instead of controller appearing "ready" but failing silently, you immediately know there's a problem
2. **Proper Load Balancer Behavior**: Kubernetes won't send traffic to a pod that's not truly ready
3. **Debugging**: Specific error messages tell you exactly what's wrong (RBAC, network, API server, etc.)
4. **Operational Excellence**: Prevents "works on my machine" issues where controller seems fine but can't actually do its job

**Q: Can you give examples of real-world RBAC misconfigurations that would be caught?**

A: Here are common RBAC scenarios:

**Missing EndpointSlices Permission:**
```yaml
# Incomplete RBAC - will cause readiness failure
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: service-validator-role
rules:
- apiGroups: [""]
  resources: ["services"]
  verbs: ["get", "list", "watch"]
# Missing: endpointslices.discovery.k8s.io and pods permissions
```

**Network Policy Blocking Access:**
```yaml
# Network policy that blocks pod access
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: restrict-pod-access
spec:
  podSelector: {}
  policyTypes:
  - Ingress
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          name: trusted-namespace
    # Controller namespace not in trusted-namespace
```

**Result**: Controller can list services but fails pod validation, readiness probe fails until network policy is fixed.