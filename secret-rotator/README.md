Build a controller that monitors Secrets and checks their age; if older than a threshold, it triggers an event or updates an annotation to alert for rotation. This addresses real-world security practices in handling credentials for databases or APIs.

## Things learnt and Improvements made

### Q1: What does `fmt.Sscanf` do in the secret rotator controller?

A: `fmt.Sscanf` is a Go function that parses formatted text from a string and stores the values into variables. It's used to convert string annotations into typed values.

**Example:**
```go
// Parse a string annotation into an integer
thresholdStr := "90"  // From annotation: secret-rotator/rotation-threshold-days: "90"
var threshold int
if _, err := fmt.Sscanf(thresholdStr, "%d", &threshold); err != nil {
    return DefaultRotationThreshold  // Use default if parsing fails
}
// threshold now contains 90 (integer)
```

**Common format specifiers:**
- `%d` - decimal integer
- `%f` - floating point number  
- `%s` - string
- `%t` - boolean

This is useful for parsing configuration values from Kubernetes annotations or other string sources.

### Qe: How does the controller handle race conditions when updating Secrets?

A: The controller uses deep copy and optimistic concurrency control to prevent race conditions when multiple controllers might update the same Secret.

**Example:**
```go
// ❌ WRONG: Direct modification can cause race conditions
secret.Annotations["key"] = "value"
return r.Update(ctx, secret)

// ✅ CORRECT: Deep copy prevents race conditions
secretCopy := secret.DeepCopy()
secretCopy.Annotations["key"] = "value"
return r.Update(ctx, secretCopy)
```

**Why this matters:**
- Kubernetes uses optimistic concurrency control with resource versions
- If another controller updates the object between our read and write, the update fails
- Deep copy ensures we work on a clean copy of the object
- The controller-runtime handles retries automatically on conflicts

### Q3: Why does the controller create Kubernetes Events?

A: Events provide observability, monitoring, and audit trails for controller actions. They're the Kubernetes-native way to signal important state changes.

**Why create events:**
- **Observability**: Clear audit trail of what happened and when
- **Monitoring**: Other tools can watch for these events
- **Debugging**: Events show controller actions during investigations
- **Compliance**: Some organizations require event logging for security audits

**In secret rotator context:**
- Alerts security teams about secrets needing rotation
- Enables DevOps teams to set up monitoring alerts
- Provides audit trails for compliance requirements
- Integrates with monitoring systems that watch Kubernetes events

**Alternative approaches:**
- Just annotations (less discoverable)
- Log to stdout (not Kubernetes-native)
- Custom resources (more complex)

### Q: What is ResourceVersion in Kubernetes?

A: `ResourceVersion` is an optimistic concurrency control mechanism that tracks object modifications and prevents conflicting updates.

**What it does:**
- **Version tracking**: Each object modification increments the ResourceVersion
- **Conflict detection**: Kubernetes uses it to detect concurrent modifications
- **Consistency**: Ensures you're working with the latest version of an object

**How it works:**
```go
// When you GET an object, it includes ResourceVersion
secret := &corev1.Secret{}
err := r.Get(ctx, req.NamespacedName, secret)
// secret.ResourceVersion = "12345"

// When you UPDATE, Kubernetes checks if ResourceVersion matches
// If someone else updated the object, ResourceVersion changed
// Your update fails with a conflict error
```

**In event creation:**
```go
InvolvedObject: corev1.ObjectReference{
    ResourceVersion: secret.ResourceVersion,  // Links event to specific object version
}
```

**Why it matters:**
- **Race conditions**: Prevents conflicting updates
- **Data integrity**: Ensures you're not overwriting someone else's changes
- **Event correlation**: Links events to specific object versions

### Q4: Why do test secrets show as "just created" even with old creationTimestamp?

A: Kubernetes ignores the `creationTimestamp` field in YAML files and always sets the actual creation time to when the object is created in the cluster.

**What happens:**
```yaml
# This creationTimestamp is IGNORED by Kubernetes
metadata:
  creationTimestamp: "2024-01-01T00:00:00Z"  # ❌ Ignored
```

**Real behavior:**
- Kubernetes sets `creationTimestamp` to **current time** when object is created
- All secrets created via YAML show as "just created" regardless of YAML timestamp
- Controller sees age as milliseconds/seconds, not the intended days

**Solutions for testing:**
1. **Wait for natural aging**: Let secrets age over time
2. **Manual timestamp patching**: Use kubectl to patch creation timestamp
3. **Programmatic creation**: Create secrets with specific timestamps via API

**Example manual patching:**
```bash
# Create secret normally
kubectl apply -f test_secrets.yaml

# Manually patch to make it old
kubectl patch secret old-database-secret -p '{"metadata":{"creationTimestamp":"2024-01-01T00:00:00Z"}}'
```

This is important for testing time-based controllers like secret rotators.

### Q5: We saw multiple events created, what was the problem and how did we fix it?

A: These errors occur due to **race conditions** when multiple reconciliation loops try to update the same object simultaneously.

**Error Messages:**
```
ERROR Failed to mark secret for rotation
"Operation cannot be fulfilled on secrets: the object has been modified; please apply your changes to the latest version and try again"

ERROR Failed to create rotation event  
"events already exists"
```

**Root Cause:**
- Multiple reconciliation loops running simultaneously for the same secret
- Each reconciliation triggers another reconciliation (update → trigger → update)
- ResourceVersion conflicts when multiple updates happen at the same time

**What We Fixed:**
1. **Event Duplication**: Changed event naming from timestamp-based to fixed name
   ```go
   // ❌ Before: Generated unique names each time
   Name: fmt.Sprintf("%s-rotation-alert-%d", secret.Name, time.Now().Unix())
   
   // ✅ After: Fixed name with duplicate check
   Name: fmt.Sprintf("%s-rotation-alert", secret.Name)
   ```

2. **Duplicate Event Prevention**: Added check before creating events
   ```go
   // Check if event already exists
   err := r.Get(ctx, client.ObjectKey{Name: eventName, Namespace: secret.Namespace}, existingEvent)
   if err == nil {
       return nil // Event exists, don't create duplicate
   }
   ```

**Industry Best Practices:**
- Use **reconciliation backoff** to prevent rapid re-reconciliations
- Implement **duplicate prevention** for events and annotations
- Use **deep copy pattern** (already implemented) to prevent race conditions
- Consider **reconciliation filtering** to reduce unnecessary updates

### Q6: After fixing above, we still had multiple reconciliation loops. What was happening?

A: We implemented **batch updates** to reduce multiple API calls that were causing race conditions.

**Old Error Messages:**
```
ERROR Failed to mark secret for rotation
"Operation cannot be fulfilled on secrets: the object has been modified"

ERROR Failed to create rotation event  
"events already exists"
```

**New Error Messages:**
```
INFO Secret marked for rotation  # Clean, single reconciliation
```

**What We Changed:**
1. **Before**: Multiple separate update functions
   ```go
   // ❌ Multiple API calls causing conflicts
   r.updateLastCheckAnnotation(ctx, secret)     // Update 1
   r.markSecretForRotation(ctx, secret)        // Update 2  
   r.createRotationEvent(ctx, secret, age, threshold)  // Update 3
   ```

2. **After**: Single batch update function
   ```go
   // ✅ Single API call, no conflicts
   func (r *SecretRotatorReconciler) batchUpdateSecret(ctx context.Context, secret *corev1.Secret, needsRotation bool, age, threshold time.Duration) error {
       secretCopy := secret.DeepCopy()
       
       // All changes in one update
       secretCopy.Annotations[LastRotationCheckAnnotation] = time.Now().Format(time.RFC3339)
       if needsRotation {
           secretCopy.Annotations[NeedsRotationAnnotation] = "true"
       }
       
       return r.Update(ctx, secretCopy)  // Single update
   }
   ```

**Benefits:**
- **Fewer API calls** = fewer race conditions
- **Single ResourceVersion change** = no conflicts
- **Atomic updates** = consistent state
- **Simpler logic** = easier to debug