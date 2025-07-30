Build a controller that monitors Secrets and checks their age; if older than a threshold, it triggers an event or updates an annotation to alert for rotation. This addresses real-world security practices in handling credentials for databases or APIs.

## FAQ

### Q: What does `fmt.Sscanf` do in the secret rotator controller?

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

### Q: How does the controller handle race conditions when updating Secrets?

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

### Q: Why does the controller create Kubernetes Events?

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
