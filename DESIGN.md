# Kubernetes Operator for Ordered Upgrades

## Overview

This operator manages the ordered upgrade of a two-component job
processing application:

- **Component A** — Job API (externally accessible)
- **Component B** — Job Worker (internal service)

The operator's responsibility is to orchestrate upgrades safely. It does
not implement the application itself or determine application version
compatibility.

**Upgrade Invariant:** Component B must be upgraded and become healthy
before Component A is upgraded.

---

## Assumptions

- Component A and Component B already exist as Kubernetes Deployments.
- Compatibility between application versions is defined by the release
  process, not inferred by the operator.
- An `ApplicationUpgrade` resource represents the desired images for
  the application components.
- If both components require an upgrade, Component B is always
  upgraded first.

### Non-goals

The operator does not:

- Implement rollback policies.
- Validate application compatibility.
- Manage job execution or business logic.

---

## Custom Resource Definition

### Spec

The spec represents the desired state.

```yaml
apiVersion: upgrades.example.io/v1alpha1
kind: ApplicationUpgrade
metadata:
  name: upgrade-v2
spec:
  worker:
    image: registry.example.com/job-worker:v2.0.0
  api:
    image: registry.example.com/job-api:v2.0.0
```

Using separate fields for each component keeps the API extensible while
allowing future support for independent component upgrades.

### Status

The status represents the observed state.

```yaml
status:
  phase: WaitingForWorkers
  observedGeneration: 2
  currentWorkerImage: registry.example.com/job-worker:v2.0.0
  currentAPIImage: registry.example.com/job-api:v1.0.0
  message: Waiting for worker rollout
  conditions:
  - type: WorkersReady
    status: "False"
```

Status fields:

- phase
- observedGeneration
- currentWorkerImage
- currentAPIImage
- conditions
- message

---

## Controller Architecture

    ApplicationUpgrade
            │
            ▼
    Reconcile()
            │
    Read CR + Deployments
            │
    Compare Desired vs Observed
            │
    Perform one safe transition
            │
    Update Status
            │
    Return

The controller watches:

- ApplicationUpgrade
- Worker Deployment
- API Deployment

Deployments are patched rather than individual Pods, allowing
Kubernetes' native Deployment controller to perform rolling updates.

Each reconciliation performs **at most one state transition**, making
reconciliation idempotent, event-driven, and resilient to controller
restarts.

---

## Upgrade Orchestration

    Pending
       │
    Upgrade Workers
       │
    Wait Until Workers Ready
       │
    Upgrade API
       │
    Wait Until API Ready
       │
    Completed

### Worker Ready Criteria

Workers are considered ready when the Deployment has fully reconciled:

- observedGeneration == generation
- updatedReplicas == replicas
- readyReplicas == replicas
- availableReplicas == replicas

The operator relies on Deployment status rather than individual Pod
status to compose with Kubernetes' native rollout behavior.

If the worker rollout fails, the operator marks the upgrade as
**Failed**, records the failure in status, and waits for user
intervention. Automatic rollback is intentionally out of scope.

---

## Status Management

### Phases

- Pending
- UpgradingWorkers
- WaitingForWorkers
- UpgradingAPI
- WaitingForAPI
- Completed
- Failed

### Conditions

- WorkersReady
- APIReady
- Progressing
- Failed

Users can monitor progress using the status section of the custom
resource.

---

## Error Handling

Failure scenarios considered:

- ImagePullBackOff
- CrashLoopBackOff
- ProgressDeadlineExceeded
- Invalid image references
- API conflicts
- Temporary Kubernetes API failures

Transient errors are retried naturally through the reconciliation loop.

Permanent rollout failures are surfaced through status conditions.

Since reconciliation derives progress entirely from observed cluster
state, the operator safely resumes after crashes without maintaining
in-memory state.

---

## Technology Choices

- Kubebuilder
- controller-runtime
- controller-gen

### Why

- Standard Kubernetes operator framework
- Built-in reconciliation, watches, informer cache, leader election
  and status patching
- Reduces boilerplate compared to raw client-go while remaining
  production proven

Trade-off: controller-runtime abstracts lower-level Kubernetes APIs in
exchange for simpler and more maintainable controller code.

---

## Testing Strategy

### Unit Tests

- Reconciliation decisions
- State transitions
- Ready evaluation
- Status updates
- Error handling

### Integration Tests (envtest)

- Worker upgrades before API
- Status progression
- Failed rollout behavior
- Recovery after operator restart

---

## Design Invariants

- Component B is always upgraded before Component A when both require
  upgrades.
- Component A is never upgraded until Component B reaches the desired
  revision and becomes Ready.
- Reconciliation is idempotent.
- Status always reflects observed cluster state.
- The operator stores no upgrade progress in memory.
- Each reconciliation performs at most one state transition.
