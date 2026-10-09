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
make test-e2e     # runs the Kind e2e suite (needs a running Kind cluster)
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

The CRD serves two versions: `v1beta1` (storage) and the deprecated `v1alpha1`,
converted by a webhook inside the operator. `make install` points that webhook
at the in-cluster Service `make deploy` creates, so with only `make install` +
`make run`, use `v1beta1`; `v1alpha1` requests need the full `make deploy`
(which also needs [cert-manager](https://cert-manager.io/)). See
[`api-versioning.md`](api-versioning.md).

### Run the operator locally

Run the controller on your host, against the cluster in `~/.kube/config`:

```sh
make run
```

This runs as your kubeconfig user, so RBAC is not enforced the way it would be
for the in-cluster ServiceAccount. To run it in-cluster instead, build and push
an image and use `make deploy IMG=<your-image>`.

#### Scoping which namespaces the operator watches

By default the operator watches Deployments in **every** namespace, which means
its cache holds every Deployment in the cluster. Scope it to the namespaces you
have actually bound it in:

```sh
make run ARGS="--watch-namespaces=job-system,job-system-staging"
```

An `ApplicationUpgrade` whose `deploymentRef` points outside that set fails
immediately with a message naming the flag, rather than retrying forever against
a namespace the operator cannot see. Keep this list and the namespaces carrying
the operator's RoleBinding in sync - the flag governs what it can *watch*, RBAC
governs what it can *write*.

### Create a sample upgrade and observe it

1. **Deploy the two placeholder components.** They are nginx stand-ins in the
   `job-system` namespace, starting at `nginx:1.25.3`:

   ```sh
   kubectl apply -f manifests/namespace.yaml
   kubectl apply -f manifests/
   ```

2. **Apply an ApplicationUpgrade** that bumps both components to `nginx:1.27.0`.
   The API calls the worker, so the sample declares `dependsOn: [worker]` on
   the API component:

   ```sh
   kubectl apply -f config/samples/upgrades_v1beta1_applicationupgrade.yaml
   ```

   The same upgrade in the deprecated two-field form is
   `config/samples/upgrades_v1alpha1_applicationupgrade.yaml`; applying it
   needs the conversion webhook, i.e. `make deploy` rather than `make run`.

3. **Watch the upgrade progress** (captured from a Kind cluster):

   ```sh
   kubectl -n job-system get applicationupgrade -w
   ```

   ```
   NAME              PHASE         MESSAGE                          AGE
   upgrade-to-1.27                                                  0s
   upgrade-to-1.27   Progressing   Upgrading worker                 13s
   upgrade-to-1.27   Progressing   Waiting for worker to complete   13s
   upgrade-to-1.27   Progressing   Upgrading api                    15s
   upgrade-to-1.27   Progressing   Waiting for api to complete      15s
   upgrade-to-1.27   Completed     Upgrade complete                 16s
   ```

   Per-component progress - which wave each component is in, its phase, and
   the image observed on its Deployment - is in `status.components`:

   ```sh
   kubectl -n job-system get applicationupgrade upgrade-to-1.27 \
     -o jsonpath='{range .status.components[*]}{.name}{"\t"}wave {.wave}{"\t"}{.phase}{"\t"}{.currentImage}{"\n"}{end}'
   ```

   ```
   worker	wave 1	Ready	nginx:1.27.0
   api	wave 2	Ready	nginx:1.27.0
   ```

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

1. You create (or edit) an `ApplicationUpgrade` listing the components, the
   desired image for each, and which components each one calls (`dependsOn`).
2. The operator resolves every component's Deployment and container first; a
   misconfiguration anywhere - including a dependency cycle - fails the upgrade
   before anything is patched.
3. It patches every component whose dependencies are all Ready - initially,
   those with no dependencies - and waits for their rollouts.
4. As each rollout completes, the components that were waiting only on it
   start. For the worker/api sample that is simply worker, then API.
5. When every component is at its desired image and ready, the upgrade reaches
   `Completed`.
6. If any rollout gets stuck, or the spec is invalid, the upgrade reaches
   `Failed` with a message naming the component; no further component starts.

All progress is derived from live cluster state, so the loop is idempotent and
safe to resume after a controller restart. Editing the spec mid-upgrade bumps the generation and
re-opens a `Completed` upgrade for another pass.

A `Failed` upgrade needs no such nudge: only `Completed` stops reconciliation,
so a failed upgrade keeps re-deriving its phase and reports recovery on its own
if the rollout turns out to be healthy. A configuration error stops until you
fix the spec, because retrying cannot help.

### Status you should expect to see

**Phase** (`.status.phase`) - a summary; clients should tolerate values added
later and act on conditions instead:

| Phase | Meaning |
|-------|---------|
| `Progressing` | At least one component is being upgraded. |
| `Completed` | Every component is at its desired image and ready. |
| `Failed` | The upgrade cannot currently proceed; re-evaluated each pass. |

**Per component** (`.status.components[]`): `wave` (its position in the upgrade
order), `currentImage` (observed on the Deployment), and `phase`:

| Phase | Meaning |
|-------|---------|
| `Pending` | Waiting for its dependencies. |
| `Upgrading` | Image patched in the latest pass. |
| `RollingOut` | The new revision is rolling out. |
| `Ready` | At the desired image, no old-revision pod left. |
| `Failed` | Its rollout exceeded the progress deadline. |

**Conditions** (`.status.conditions`), each stamped with the `observedGeneration`
it was recorded at:

- `Ready` - true once every component has fully rolled out.
- `Progressing` - true while the operator is actively driving an upgrade.
- `Failed` - true while the upgrade cannot proceed.
- `WorkersReady` / `APIReady` - kept for upgrades that are exactly a worker/api
  pair, which is every upgrade created through v1alpha1, so existing
  `kubectl wait --for=condition=...` users keep working.

Read through v1alpha1, the same object shows the v1alpha1 status:
`UpgradingWorkers` / `WaitingForWorkers` / `UpgradingAPI` / `WaitingForAPI`
phases and `currentWorkerImage` / `currentAPIImage`, derived by the conversion
webhook from the per-component status above.

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
| **`internal/controller/applicationupgrade_controller.go`** | **The main reconciliation logic** - dependency-ordered upgrades over the v1beta1 graph, readiness/failure evaluation, image patching, status management, and the Deployment watch. |
| `internal/dag/` | Kahn's algorithm: upgrade waves from `dependsOn`, and cycle detection. |
| `internal/controller/applicationupgrade_graph_test.go` | envtest specs for graph ordering, cycles, v1beta1 validation, and v1alpha1 read-modify-write of a graph through the real conversion webhook. |
| `internal/controller/applicationupgrade_controller_test.go` | envtest specs driving Deployment status by hand to assert ordering, the terminating-old-pod window, idempotency, and failure behavior. |
| `internal/controller/applicationupgrade_validation_test.go` | Tests for the CRD's declarative (CEL) validation rules. |
| `internal/controller/suite_test.go` | envtest suite bootstrap. |
| **`api/v1beta1/applicationupgrade_types.go`** | **The CRD definition (storage version, conversion hub)** - components, `dependsOn`, per-component status, CEL validation. |
| `api/v1alpha1/applicationupgrade_types.go` | The deprecated two-component version. |
| **`api/v1alpha1/applicationupgrade_conversion.go`** | **Conversion to and from v1beta1**, including the annotation that keeps a graph intact through v1alpha1; fuzzed round-trip tests alongside. |
| `config/policy/` | ValidatingAdmissionPolicy that stops a v1alpha1 write from truncating a graph. |
| `config/webhook/`, `config/certmanager/` | Conversion webhook Service and its cert-manager serving certificate. |
| `hack/migrate-storage-version.sh` | Rewrites every object at the storage version and shrinks `storedVersions`. |
| `config/crd/bases/` | Generated CRD YAML applied by `make install`. |
| **`config/samples/upgrades_v1beta1_applicationupgrade.yaml`** | **A ready-to-apply sample** upgrade resource (the v1alpha1 form sits next to it). |
| `config/rbac/role.yaml` | Generated operator RBAC (get/list/watch/patch on Deployments, plus the CRD). |
| `manifests/` | Placeholder two-component application (Deployments/Services in `job-system`) used to demo an upgrade end-to-end. |
| `cmd/main.go` | Manager entry point (wires the reconciler, metrics, leader election). |
| `DESIGN.md` | The design document written before the implementation. |

The reconcile entry point is `Reconcile` (get the CR, short-circuit terminal
states, snapshot, run the state machine, persist status once if it changed). The
ordered decision logic is in the unexported `reconcile` method directly below it.
