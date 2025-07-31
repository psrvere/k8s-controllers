# Auto-Scaler Controller

Deployment Scaler Based on Labels: Develop a controller that watches Deployments with a specific label and scales replicas up or down based on a simple threshold, like CPU usage from metrics (using fake data). This is similar to basic auto-scaling in production web services without relying on HPA.

## Implementation Summary

### Key Features Implemented:
- **Label-based filtering**: Only processes deployments with `auto-scaler/enabled` label
- **CPU-based scaling**: Scales up when CPU > 60%, scales down when CPU < 40%
- **Replica limits**: Min 1, Max 10 replicas
- **Cooldown mechanism**: 20-second cooldown between scaling operations
- **Fake CPU metrics**: Random CPU usage between 10-90% for testing

### Lessons Learned:
- Kubernetes controllers receive multiple events during scaling operations (spec changes, status updates, pod changes)
- Deployment status fields: `replicas` (desired), `readyReplicas` (ready), `availableReplicas` (stable), `updatedReplicas` (updated)
- Cooldown periods prevent rapid scaling oscillations
- Event filtering helps understand what triggers controller reconciliations
- Controller reconciliation happens frequently - need proper state management