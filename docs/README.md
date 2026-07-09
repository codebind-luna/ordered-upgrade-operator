# Application Upgrade Operator

A Kubernetes operator that performs **ordered upgrades** of a two-component
job-processing application:

- **Component A - Job API** (externally accessible; calls the worker over HTTP)
- **Component B - Job Worker** (internal service)

The operator enforces one invariant: **Component B is upgraded and becomes
healthy before Component A**. Because A is the caller and B the callee over an
internal HTTP contract, upgrading the callee first keeps the new request
contract on the receiving side before any caller can emit it. The reasoning is
detailed in [`../DESIGN.md`](../DESIGN.md).

Documentation:

- **Getting Started** - build, install, run, and drive a sample upgrade (below)
- **Runtime Expectations** - readiness, the user-facing flow, status, and errors (below)
- **Project Structure** - where the important code lives (below)
- **[Design vs. Implementation](design-vs-implementation.md)** - what changed
  between the design and the code, and why

---

## Getting Started

### Prerequisites

| Requirement | Notes |
|-------------|-------|
| **Go 1.23+** | Hard requirement; the module declares `go 1.23.0`. |
| **A Kubernetes cluster** | [Kind](https://kind.sigs.k8s.io/) is the easiest local option. Any cluster reachable via your kubeconfig works. |
| **kubectl** | Configured to point at the target cluster (`kubectl config current-context`). |
| **make** | Drives all build, test, and deploy targets. |
| **Docker** (optional) | Only needed to build/push the manager image (`make docker-build`) or run e2e tests on Kind. |

The Makefile downloads its own pinned build tools into `./bin` on first use
(`controller-gen`, `kustomize`, `setup-envtest`, `golangci-lint`), so you do not
need to install them separately.

### Build

```sh
make build        # generates manifests + code, vets, and builds bin/manager
```

Other useful targets:

```sh
make test         # runs the envtest-backed controller specs with coverage
make lint         # runs golangci-lint
make docker-build IMG=<registry>/application-upgrade-operator:tag
```

### Install the CRD

Install the `ApplicationUpgrade` CustomResourceDefinition into the cluster your
kubeconfig points at:

```sh
make install
```

This is equivalent to `kustomize build config/crd | kubectl apply -f -`. To
apply the raw CRD directly:

```sh
kubectl apply -f config/crd/bases/upgrades.lunadas.dev_applicationupgrades.yaml
```

Verify:

```sh
kubectl get crd applicationupgrades.upgrades.lunadas.dev
```

### Run the operator locally

Run the controller on your host, against the cluster in `~/.kube/config`:

```sh
make run
```

This runs as your kubeconfig user, so RBAC is not enforced the way it would be
for the in-cluster ServiceAccount. To run it in-cluster instead, build and push
an image and use `make deploy IMG=<your-image>`.

### Create a sample upgrade and observe it

1. **Deploy the two placeholder components.** They are nginx stand-ins in the
   `job-system` namespace, starting at `nginx:1.25.3`:

   ```sh
   kubectl apply -f manifests/namespace.yaml
   kubectl apply -f manifests/
   ```

2. **Apply an ApplicationUpgrade** that bumps both components to `nginx:1.27.0`:

   ```sh
   kubectl apply -f config/samples/upgrades_v1alpha1_applicationupgrade.yaml
   ```

3. **Watch the upgrade progress.** The printer columns surface the phase and the
   observed image of each component:

   ```sh
   kubectl -n job-system get applicationupgrade -w
   ```

   ```
   NAME              PHASE              WORKER         API            MESSAGE
   upgrade-to-1.27   UpgradingWorkers   nginx:1.25.3   <none>         Upgrading workers to nginx:1.27.0
   upgrade-to-1.27   WaitingForWorkers  nginx:1.27.0   <none>         Waiting for worker rollout to complete
   upgrade-to-1.27   UpgradingAPI       nginx:1.27.0   nginx:1.25.3   Upgrading API to nginx:1.27.0
   upgrade-to-1.27   WaitingForAPI      nginx:1.27.0   nginx:1.27.0   Waiting for API rollout to complete
   upgrade-to-1.27   Completed          nginx:1.27.0   nginx:1.27.0   Upgrade complete
   ```

   The API column reads `<none>` until the worker is ready, because the operator
   only records `currentAPIImage` once it reaches Component A - a direct,
   observable consequence of the B-before-A ordering.

4. **Confirm the ordering held** at any point by reading the live Deployment
   images. The API stays at the old image until the worker rollout is complete:

   ```sh
   kubectl -n job-system get deploy job-worker job-api \
     -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.template.spec.containers[0].image}{"\n"}{end}'
   ```

5. **Inspect conditions** for detail:

   ```sh
   kubectl -n job-system get applicationupgrade upgrade-to-1.27 -o yaml | yq '.status'
   ```

---

## Runtime Expectations

### When Component B is considered ready to proceed

The operator patches the worker Deployment's container image and then waits for
the built-in Deployment controller to complete the rolling update. Readiness
uses the same criteria as `kubectl rollout status`
([`deploymentReady`](../internal/controller/applicationupgrade_controller.go)):

1. `status.observedGeneration >= metadata.generation` - the Deployment
   controller has observed the patched spec. **Checked first**, because right
   after a patch the status still describes the previous revision and its
   replica counts can momentarily read as "ready" for the pods being replaced.
2. `updatedReplicas == desired replicas` - every desired replica runs the new
   template.
3. `replicas == updatedReplicas` - **no old-revision pods remain.**
4. `availableReplicas == updatedReplicas` - all updated replicas are available.

Clause 3 is what makes the ordering sound: it holds the API upgrade until every
old worker pod is gone, not merely until the new ones are up, so the API is
never upgraded while an old, terminating worker can still serve a request. Only
once the worker satisfies all four does the operator move on to Component A,
which is gated on the identical criteria.

Readiness is defined against **Deployment status**, not an independent probe of
the running process. "Ready" means "the desired image rolled out and its
replicas are available," which composes with native Kubernetes rollout behavior
without coupling the operator to the application.

### The upgrade flow, from a user's perspective

1. You create (or edit) an `ApplicationUpgrade` naming the two Deployments and
   the desired image for each.
2. The operator patches the **worker** Deployment first and waits for its
   rollout to finish.
3. Once the worker is fully rolled out, the operator patches the **API**
   Deployment and waits for its rollout.
4. When both are at their desired image and ready, the upgrade reaches
   `Completed`.
5. If either rollout gets stuck, or the spec is invalid, the upgrade reaches
   `Failed` with a message and stops until you act.

Each reconcile performs **at most one transition**, and all progress is derived
from live cluster state, so the loop is idempotent and safe to resume after a
controller restart. Editing the spec mid-upgrade bumps the generation and
re-opens a `Completed`/`Failed` upgrade for another pass.

### Status you should expect to see

**Phases** (`.status.phase`):

| Phase | Meaning |
|-------|---------|
| `UpgradingWorkers` | The worker Deployment was just patched. |
| `WaitingForWorkers` | Waiting for the worker rollout to complete. |
| `UpgradingAPI` | The API Deployment was just patched. |
| `WaitingForAPI` | Waiting for the API rollout to complete. |
| `Completed` | Both components are at the desired image and ready. |
| `Failed` | The upgrade cannot proceed without intervention. |

**Conditions** (`.status.conditions`), each stamped with the `observedGeneration`
it was recorded at:

- `WorkersReady` - true once the worker rollout has fully reconciled.
- `APIReady` - true once the API rollout has fully reconciled.
- `Progressing` - true while the operator is actively driving an upgrade.
- `Failed` - true when the upgrade has entered a terminal failure.

**Other fields:** `currentWorkerImage` / `currentAPIImage` report the images the
operator currently observes on each Deployment; `message` is a human-readable
summary; `observedGeneration` is the spec generation the status reflects.

### Behavior during error scenarios

The operator separates **configuration errors** from **transient errors**
([`failOrRequeue`](../internal/controller/applicationupgrade_controller.go)):

- **Configuration errors** cannot be fixed by retrying, so the upgrade moves to
  `Failed` with a descriptive message and stops. These include:
  - a referenced Deployment that does not exist;
  - a namespace the operator was never granted access to (the API returns
    Forbidden - the message directs the cluster admin to bind the operator's
    ClusterRole there);
  - an ambiguous container reference (a multi-container pod template with no
    `containerName`, or a `containerName` that matches nothing) - which prevents
    a sidecar from being patched by accident.
- **Transient errors** (a conflicting write, a temporary API-server outage) are
  identified by their error type and left to controller-runtime's exponential
  backoff to retry.

A **stuck rollout** is detected from the Deployment's own `Progressing`
condition with reason `ProgressDeadlineExceeded`
([`deploymentFailed`](../internal/controller/applicationupgrade_controller.go)) -
the authoritative "this rollout is stuck" signal - rather than by interpreting
individual pod states. A stuck **worker** rollout marks the whole upgrade
`Failed` and the API is never touched.

There is **no automatic rollback** and **no finalizer**: deleting an
`ApplicationUpgrade` stops orchestration and leaves the components at their
current versions.

---

## Project Structure

Kubebuilder (`go.kubebuilder.io/v4`) project layout. The pieces that matter for
this project:

| Path | What's there |
|------|--------------|
| **`internal/controller/applicationupgrade_controller.go`** | **The main reconciliation logic** - the ordered B-before-A state machine, readiness/failure evaluation, image patching, status management, and the Deployment watch. |
| `internal/controller/applicationupgrade_controller_test.go` | envtest specs driving Deployment status by hand to assert ordering, the terminating-old-pod window, idempotency, and failure behavior. |
| `internal/controller/applicationupgrade_validation_test.go` | Tests for the CRD's declarative (CEL) validation rules. |
| `internal/controller/suite_test.go` | envtest suite bootstrap. |
| **`api/v1alpha1/applicationupgrade_types.go`** | **The CRD definition** - `ApplicationUpgrade` spec/status Go types, phases, conditions, and the kubebuilder validation/print-column markers. |
| `config/crd/bases/` | Generated CRD YAML applied by `make install`. |
| **`config/samples/upgrades_v1alpha1_applicationupgrade.yaml`** | **A ready-to-apply sample** upgrade resource. |
| `config/rbac/role.yaml` | Generated operator RBAC (get/list/watch/patch on Deployments, plus the CRD). |
| `manifests/` | Placeholder two-component application (Deployments/Services in `job-system`) used to demo an upgrade end-to-end. |
| `cmd/main.go` | Manager entry point (wires the reconciler, metrics, leader election). |
| `DESIGN.md` | The design document written before the implementation. |

The reconcile entry point is `Reconcile` (get the CR, short-circuit terminal
states, snapshot, run the state machine, persist status once if it changed). The
ordered decision logic is in the unexported `reconcile` method directly below it.
