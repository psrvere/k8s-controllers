# ConfigMap Sync Controller

A Kubernetes controller that watches ConfigMaps in one namespace and synchronizes their data to ConfigMaps in another namespace. Useful in real-world multi-tenant setups where configs need to be mirrored across teams or environments.

## Q&A: Key Learning Points

### Q: What's the difference between Labels and Annotations in our project?
**A:** 
- **Labels**: Used for selection/identification (e.g., `config-syncer/enabled` to mark which ConfigMaps to sync)
- **Annotations**: Used for configuration/metadata (e.g., `config-syncer/target-namespace` to specify where to sync)
- Labels are queryable, annotations are not. Labels follow DNS naming rules, annotations can contain any characters.

**Example:**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: source-ns
  labels:
    config-syncer/enabled: "true"        # LABEL: Selects this ConfigMap for processing
  annotations:
    config-syncer/target-namespace: "team-a,team-b"  # ANNOTATION: Configuration
    config-syncer/target-name: "shared-config"       # ANNOTATION: More config
data:
  database-url: "postgres://..."
```

**Query with labels:**
```bash
kubectl get configmaps -l config-syncer/enabled=true
```

### Q: What's the difference between Data and BinaryData in ConfigMaps?
**A:**
- **Data**: `map[string]string` for human-readable text (JSON, YAML, properties)
- **BinaryData**: `map[string][]byte` for binary files (certificates, keys, images)
- Our controller handles both types seamlessly for complete configuration sync.

**Example:**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  # Human-readable configuration
  config.json: '{"debug": true, "timeout": 30}'
  app.properties: |
    server.port=8080
    logging.level=INFO

binaryData:
  # Binary files (base64 encoded)
  ssl-cert.pem: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCg==
  private-key.pem: |
    LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQo=
```

**In our code:**
```go
// Data comparison (strings)
if target.Data[k] != v {  // Direct string comparison
    return false
}

// BinaryData comparison (convert to string)
if string(target.BinaryData[k]) != string(v) {  // Convert []byte to string
    return false
}
```

### Q: What does req.NamespacedName represent in our Reconcile function?
**A:** It's the name and namespace of the **source ConfigMap** that triggered the reconciliation (the one being watched), not the target ConfigMap we're creating/updating.

**Example:**
```yaml
# This ConfigMap triggers reconciliation
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config           # req.Name = "my-config"
  namespace: source-ns      # req.Namespace = "source-ns"
  labels:
    config-syncer/enabled: "true"
  annotations:
    config-syncer/target-namespace: "target-ns"
data:
  key1: value1
```

**When this ConfigMap changes:**
```go
func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // req.NamespacedName = "source-ns/my-config" (the SOURCE)
    
    // Fetch the source ConfigMap
    configMap := &corev1.ConfigMap{}
    err := r.Get(ctx, req.NamespacedName, configMap)  // Gets source ConfigMap
    
    // Then sync it to target namespace
    targetNamespace := "target-ns"
    r.syncConfigMap(ctx, configMap, targetNamespace, log)
}
```

### Q: How does our controller handle multi-namespace syncing?
**A:** Uses comma-separated values in the `config-syncer/target-namespace` annotation to sync a single ConfigMap to multiple target namespaces simultaneously.

**Example:**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: global-config
  namespace: shared-ns
  labels:
    config-syncer/enabled: "true"
  annotations:
    config-syncer/target-namespace: "team-frontend,team-backend,team-mobile"
data:
  api-endpoint: "https://api.example.com"
  feature-flags: '{"new-ui": true}'
```

**Creates ConfigMaps in:**
- `team-frontend` namespace
- `team-backend` namespace  
- `team-mobile` namespace

**Code handling:**
```go
func getTargetNamespaces(configMap *corev1.ConfigMap) []string {
    targetNamespaceStr := configMap.Annotations[TargetNamespaceAnnotation]
    // "team-frontend,team-backend,team-mobile"
    
    namespaces := strings.Split(targetNamespaceStr, ",")
    // ["team-frontend", "team-backend", "team-mobile"]
    
    return namespaces
}

// Sync to each namespace
for _, targetNamespace := range targetNamespaces {
    r.syncConfigMap(ctx, configMap, targetNamespace, log)
}
```

### Q: What's the difference between search_replace and edit_file for code changes in Cursor?
**A:** 
- **search_replace**: More efficient for small, targeted changes (fewer tokens)
- **edit_file**: Better for substantial changes across multiple sections
- **Best practice**: Use search_replace for simple text replacements, edit_file for complex changes