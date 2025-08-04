In this project, I have built many small controller projects with the aim of learning a wide variety of concepts related to kubernetes and controllers.

I have used Cursor extensively for building out these projects. My role was to break down problem statements in smaller parts that cursor can implement, review the code, manually test and fix bugs.

I have detailed my learnings and experiences of building each project in a README file.

Here are my learnings with each of the projects in my own words:

1. **Pod Labeller**
- labels have name constraints:
    - length: 63 characters
    - start and end with alphanumeric characters,
    - other valid characters are limited to "-", "_", and "."
- wait for pods to be ready before updating to avoid race condition
- `healthyz.Ping` is not a good readiness check. Replaced with checking connectivity to k8s API and access to PodList API

2. **Auto Scaler**
- Deployment is ready if `deployment.Status.ReadyReplicas` > 0
- Various deployment status fields
  - Replicas - how many are desired
  - ReadyReplicas - how many are ready
  - AvailableReplicas - how many are stable
  - UpdatedReplicas - how many are updated to latest version of the deployment
- Controller updates replicas in deployment spec to desired number and then k8s scales up/down pods accordingly
- Cool down period is required to allow k8s to finish last scaling operations and to be not too sensitive to metrics changes
- Implemented event filtering in `SetupWithManager` to control what triggers reconciliation

3. **ConfigMap Syncer**
- Labels are used for selection/identification. Annotations are used for configuration/metadata.
- Labels are queryable, annotations are not.
    - label query example: `kubectl get pods -l app=web`
    - labels are indexed for faster query. No indexing in annotations
- Another example: should the config map be synced is flagged by labels and to what all namespaces it should be synced to, is flagged by annotations
- Data vs Binary Data in ConfigMaps
    - Data is `map[string]string` for human readable text like JSON, YAML, etc
    - BinaryData is `map[string][]byte` for binary files like certificates, keys, images, etc
- Reconcile function is called by the `controller-runtime` and it passes the `name` and `namespace` of the source ConfigMap which triggered reconciliation
