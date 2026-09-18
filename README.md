# Ordered Upgrade Operator

A Kubernetes operator that upgrades a **callee before its caller**.

When two components talk to each other over an internal API, a rolling update
briefly runs both versions of each at once. If the caller upgrades first, its
new-contract requests land on a callee that cannot parse them, and every
in-flight request fails for the length of the rollout. Upgrading the callee
first — and waiting until *every* old callee pod is gone — is what makes the
release process's backward-compatibility guarantee hold at runtime.

This operator enforces that ordering for a two-component job-processing
application:

| Component | Role |
|-----------|------|
| **Job API** (Component A) | Externally accessible; calls the worker over HTTP |
| **Job Worker** (Component B) | Internal; **always upgraded first** |

You declare the desired images in one custom resource; the operator drives the
Deployments there in the correct order, and reports where it got to.

```yaml
apiVersion: upgrades.lunadas.dev/v1alpha1
kind: ApplicationUpgrade
metadata:
  name: upgrade-v2
  namespace: job-system
spec:
  worker:
    deploymentRef: { name: job-worker }
    image: registry.example.com/job-worker:v2.0.0
    containerName: worker
  api:
    deploymentRef: { name: job-api }
    image: registry.example.com/job-api:v2.0.0
    containerName: api
```

```console
$ kubectl -n job-system get applicationupgrade
NAME         PHASE               WORKER                     API                     MESSAGE                                  AGE
upgrade-v2   WaitingForWorkers   job-worker:v2.0.0          job-api:v1.0.0          Waiting for worker rollout to complete   12s
```

## How it works

Each reconcile performs **at most one state transition** and derives all
progress from observed cluster state — nothing is held in memory, so the
operator resumes correctly after a restart.

```
Pending -> UpgradingWorkers -> WaitingForWorkers
        -> UpgradingAPI     -> WaitingForAPI     -> Completed
                                                 \-> Failed
```

Three decisions carry most of the correctness:

- **"Ready" means the old version is gone, not just that the new one is up.**
  Readiness requires `replicas == updatedReplicas` alongside the usual
  available-replica checks, matching what `kubectl rollout status` waits for. The
  weaker definition is satisfied *during* the surge window, while an old worker
  pod is still terminating and can still serve a request — which is exactly the
  failure the ordering exists to prevent.
- **Deployment status is only trusted once the Deployment controller has seen
  the patch.** `observedGeneration >= generation` is checked first in both the
  readiness and the failure path; without it the operator reads a stale status
  and either declares a rollout done before it started, or re-fails a recovery it
  just initiated.
- **Configuration errors are separated from transient ones.** A missing
  Deployment or an ambiguous container reference cannot be fixed by retrying, so
  the upgrade goes terminally `Failed` with an actionable message; a conflicting
  write or an API blip is left to controller-runtime's backoff.

The operator does not own the Deployments — it patches images and lets the
native Deployment controller perform the rolling update. There is no finalizer
and no automatic rollback.

Its cost scales with the number of upgrades in flight, not with the size of the
cluster: a field index resolves a Deployment event to the ApplicationUpgrades
that reference it, and `--watch-namespaces` scopes the Deployment cache to the
namespaces the operator is actually bound in.

## Documentation

- **[`DESIGN.md`](DESIGN.md)** — the design: CRD shape, controller
  architecture, ordering rationale, failure handling, RBAC model.
- **[`docs/README.md`](docs/README.md)** — build, install, run, and drive a
  sample upgrade; runtime expectations and project layout.
- **[`docs/design-vs-implementation.md`](docs/design-vs-implementation.md)** —
  what changed between the design and the code, what surprised me, and the known
  gaps.

## Quick start

Requires Go 1.23+, a Kubernetes cluster ([Kind](https://kind.sigs.k8s.io/) is
fine), `kubectl`, and `make`. Build tooling is downloaded into `./bin` on first
use.

```sh
make install                              # install the CRD

kubectl apply -f manifests/namespace.yaml # placeholder two-component app...
kubectl apply -f manifests/               # ...in the `job-system` namespace

make run ARGS="--watch-namespaces=job-system"   # run against your kubeconfig

kubectl apply -f config/samples/upgrades_v1alpha1_applicationupgrade.yaml
kubectl -n job-system get applicationupgrade -w
```

`make test` runs the envtest-backed controller specs, which drive Deployment
status by hand to assert the ordering, the terminating-old-pod window,
idempotency, and stuck/missing-Deployment failures deterministically.

`make test-e2e` runs the suite against a live Kind cluster, where a real
Deployment controller produces the rollout statuses. It asserts the negative the
unit specs cannot — that the API is *never* patched at any point during the
worker rollout — and it is what caught the one bug the envtest specs missed
([write-up](docs/design-vs-implementation.md)).

## Status

Alpha (`v1alpha1`), and built as a focused exercise rather than a production
deployment. [`docs/design-vs-implementation.md`](docs/design-vs-implementation.md)
lists the known limitations honestly — the largest being that failure is
detected via `ProgressDeadlineExceeded` rather than per-pod classification, and
that the e2e proves the ordering on a single replica.

## License

[Apache 2.0](LICENSE).
