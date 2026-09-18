# Kubernetes Operator for Ordered Upgrades

## Overview

This operator manages the ordered upgrade of a two-component job
processing application:

- **Component A** - Job API (externally accessible)
- **Component B** - Job Worker (internal service)

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
apiVersion: upgrades.lunadas.dev/v1alpha1
kind: ApplicationUpgrade
metadata:
  name: upgrade-v2
  namespace: job-system
spec:
  worker:
    deploymentRef:
      name: job-worker
      # namespace defaults to the ApplicationUpgrade's namespace
    image: registry.example.com/job-worker:v2.0.0
    containerName: worker
  api:
    deploymentRef:
      name: job-api
    image: registry.example.com/job-api:v2.0.0
    containerName: api
```

Using separate fields for each component keeps the API extensible while
allowing future support for independent component upgrades.

Each component points at its Deployment by name. A name resolves to a
single object; a label selector could match none or several, which would
force the operator to resolve an ambiguity that adds nothing here.

The namespace on a ref is optional and defaults to the namespace of the
ApplicationUpgrade. In the common case both components and the CR live in
one namespace and the field is omitted. Setting it supports a control
namespace that drives upgrades for apps elsewhere, at the cost of wider
RBAC - see Security below.

`containerName` is optional. When set, the operator patches that
container's image. When omitted it patches the sole container in the pod
template and treats a multi-container pod as a configuration error, so a
sidecar such as a service mesh proxy is never upgraded by accident.

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

The operator does not own these Deployments - they pre-exist and are only
mutated - so no `ownerReference` is set and the usual `Owns()` watch does
not apply. Instead the Deployments are watched with a handler that maps a
changed Deployment back to any ApplicationUpgrade whose `worker` or `api`
ref names it (`EnqueueRequestsFromMapFunc`).

That mapping is backed by a **field index** on ApplicationUpgrade, keyed by
the namespaced name of each referenced Deployment. Every Deployment in the
watched namespaces produces events, and all but a handful are irrelevant, so
resolving each event by listing and scanning every ApplicationUpgrade would
make the operator's cost scale with the size of the cluster rather than with
the number of upgrades in flight. The index turns it into a keyed cache
lookup. The index resolves an omitted ref namespace against the CR's own
namespace, exactly as the reconciler does, so the two cannot disagree about
which object a ref denotes.

The manager's cache is scoped to the namespaces the operator is bound in,
via `--watch-namespaces`. A cluster-wide default cache holds every Deployment
in the cluster - a large resident set for an operator that touches very few
of them - and caching is the cost that actually scales, since RBAC constrains
what may be written but not what is watched. ApplicationUpgrades stay
cluster-wide: they are few and small, and a control namespace may drive
upgrades elsewhere.

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

### Why workers upgrade first

Component A calls Component B over an internal HTTP API, so B is the
callee and A the caller. During any rolling update old and new pods of a
component coexist for a window, so the safe order is the one that puts the
new request contract on the receiving side before any caller can emit it.

Upgrading B first satisfies this. While B rolls out, A is still old and
sends old requests, which the new B must accept - a backward-compatible
change the release process is responsible for. Once B is fully upgraded, A
rolls out and its new requests land on a B that already understands them.

The reverse is unsafe. If A upgraded first, its new pods would send
new-contract requests - an added field, a new endpoint - to an old B that
cannot parse them, failing in-flight jobs for the length of the rollout.
The operator never inspects the contract itself; enforcing
callee-before-caller ordering is what makes the release process's
compatibility guarantee hold at runtime.

### Worker Ready Criteria

Workers are considered ready when the Deployment has fully rolled out, using
the same criteria as `kubectl rollout status`:

- observedGeneration >= generation
- updatedReplicas == desired replicas
- replicas == updatedReplicas (no old-revision pods remain)
- availableReplicas == updatedReplicas

The `replicas == updatedReplicas` clause is what makes the ordering sound: it
holds the upgrade until every old worker pod is gone, not merely until the new
ones are up, so the API is never upgraded while an old, terminating worker can
still serve a request.

The `observedGeneration` check is evaluated first and matters most.
Immediately after the operator patches the Deployment, its status still
describes the previous revision, so the replica counts can momentarily
read as "ready" for the pods being replaced. The operator therefore only
trusts the status once the Deployment controller has observed the patched
spec - `status.observedGeneration >= metadata.generation` - which avoids
concluding the rollout is done before it has begun.

The operator relies on Deployment status rather than individual Pod
status to compose with Kubernetes' native rollout behavior. This means
"ready" is defined as "the desired image rolled out and its replicas are
available," not as an independent confirmation that the running process
serves that version. Verifying the actual version over the component's
HTTP endpoint would be stronger but couples the operator to the
application; that coupling is deliberately avoided.

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
- Referenced Deployment does not exist
- Ambiguous container reference (multiple containers, none named)
- API conflicts
- Temporary Kubernetes API failures

The operator separates configuration errors from transient failures. A
missing Deployment or an ambiguous container reference cannot be fixed by
retrying, so the upgrade moves to Failed with a descriptive message and
stops until the user corrects the spec. Transient errors - a conflicting
write, a temporary API server outage - are told apart by their error type
and left to the reconciler's backoff to retry.

A failed rollout is detected from the Deployment's own conditions -
`Progressing` with reason `ProgressDeadlineExceeded`, or `Available` false
- rather than by interpreting individual pod states, and is then surfaced
through the CR's status conditions.

Since reconciliation derives progress entirely from observed cluster
state, the operator safely resumes after a crash without in-memory state.
The same property handles a spec edited mid-upgrade: each pass compares
`spec.generation` against `status.observedGeneration`, re-derives the
desired state, and re-plans from what the cluster currently shows.

Deletion uses no finalizer. The operator does not own the Deployments, so
deleting an ApplicationUpgrade simply stops orchestration and leaves the
components at their current versions - a deliberate choice, not an
oversight.

A single ApplicationUpgrade per application is assumed. Overlapping CRs
targeting the same Deployments are treated as a user error rather than
arbitrated by the operator.

---

## Security and RBAC

The operator only needs to read and patch Deployments. RBAC is granted
ahead of time; the operator cannot widen its own access at runtime.

Permissions are modelled as a single ClusterRole granting `get` and
`patch` on Deployments, bound per namespace with a RoleBinding. Defining
the ClusterRole grants nothing on its own - the operator can act in a
namespace only where a RoleBinding for it exists. The set of managed
namespaces is therefore explicit and auditable: it is exactly the set of
namespaces carrying that RoleBinding.

This matters because the ref's namespace is a user-supplied runtime input
while the grant is an administrator's deploy-time decision, and the two
can diverge. If a CR references a namespace the operator was never bound
to, the API returns Forbidden. That is a configuration error, not a
transient one - retrying cannot help - so the upgrade moves to Failed with
a message directed at the cluster administrator rather than the app owner.

The same divergence exists one layer earlier, at the cache. A ref pointing
outside `--watch-namespaces` could never be served from the scoped cache, so
the reconciler checks the watch set *before* issuing the Get and fails the
upgrade with a message naming the flag to change. Without that check the
read fails somewhere inside the cache with an error that reads as transient,
and the upgrade retries forever against a namespace it is structurally
unable to see. The watch set and the RoleBindings should name the same
namespaces; they are enforced separately because they fail differently.

Cross-namespace references weaken namespace isolation: whoever can create
an ApplicationUpgrade can target any namespace the operator is bound to.
The per-namespace RoleBinding is what keeps that opt-in, mirroring the
ReferenceGrant model in the Gateway API using primitives available today.

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
