In this project, I have build many small controller projects with the aim of learning a wide variety of concepts related to kubernetes and controllers.

I have used Cursor extensively for building out these prjects. My role was to break down problem statement in smaller parts that cursor can implement, review the code, manually test and fix bugs.

I have detailed my learnings and experiences of building each projects in a README file.

Here are my learnings with each of the projects in my own words:

1. **Pod Labeller**
- labels are used for filtering, annotations are used for configurations
- labels have name contraints:
    - length: 63 characters
    - start and end with alphanumeric characters,
    - other valid charcters are limited to "-", "_", and "."
- wait for pods to be ready before updating to avoid race condition
- `healthyz.Ping` is not a good readiness check. Replaced with checking connectinvity to k8s API and access to PodList API

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
