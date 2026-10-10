# Ordered Upgrade Operator

A Kubernetes operator that upgrades a **callee before its caller**.

When two components talk to each other over an internal API, a rolling update
briefly runs both versions of each at once. If the caller upgrades first, its
new-contract requests land on a callee that cannot parse them, and every
in-flight request fails for the length of the rollout.

Compatibility itself is the application developers' contract, not the
operator's: the new callee must accept requests from the previous caller, while
the new caller is allowed to depend on the new callee (for example, by sending a
field the old callee does not know). That one-directional guarantee is only
sufficient if the callee upgrades first — and *every* old callee pod is gone —
before the caller moves. The operator enforces that ordering; it does not verify
compatibility. (A manual rollback reverses the order: caller first, then
callee.)

You declare the components, their images, and which components each one
calls; the operator upgrades them in dependency order and reports where it got
to:

```yaml
apiVersion: upgrades.lunadas.dev/v1beta1
kind: ApplicationUpgrade
metadata:
  name: upgrade-v2
  namespace: job-system
spec:
  components:
  - name: a
    deploymentRef: { name: app-a }
    image: registry.example.com/app-a:v2.0.0
    dependsOn: [b, c]        # a calls b and c
  - name: b
    deploymentRef: { name: app-b }
    image: registry.example.com/app-b:v2.0.0
    dependsOn: [d]           # b calls d
  - name: c
    deploymentRef: { name: app-c }
    image: registry.example.com/app-c:v2.0.0
  - name: d
    deploymentRef: { name: app-d }
    image: registry.example.com/app-d:v2.0.0
```

```console
$ kubectl -n job-system get applicationupgrade upgrade-v2 \
    -o custom-columns='COMPONENT:.status.components[*].name,WAVE:.status.components[*].wave,PHASE:.status.components[*].phase'
COMPONENT   WAVE      PHASE
a,b,c,d     3,2,1,1   Pending,Upgrading,RollingOut,Ready
```

The original two-component form - `spec.worker` / `spec.api` in
`upgrades.lunadas.dev/v1alpha1` - is still served, deprecated, and converts to
and from the graph form. [`docs/api-versioning.md`](docs/api-versioning.md)
covers how.

## How it works

The upgrade order comes from `dependsOn` via Kahn's algorithm: waves `[c, d]`,
`[b]`, `[a]` above. Each reconcile derives every component's progress from
observed cluster state, nothing is held in memory, so the operator resumes
correctly after a restart. A component starts as soon as **its own**
dependencies are done: b starts when d finishes, without waiting for c. All
components are resolved before any is patched, so a misconfigured component
anywhere fails the upgrade before it starts, and while any rollout has failed
nothing new is started.

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
- **[`docs/api-versioning.md`](docs/api-versioning.md)** — the move from
  v1alpha1 to v1beta1: conversion webhook, the lossy direction and how it is
  contained, the admission guard, and the storage-migration and deprecation
  plan.

## Quick start

Requires Go 1.23+, a Kubernetes cluster ([Kind](https://kind.sigs.k8s.io/) is
fine), `kubectl`, and `make`. Build tooling is downloaded into `./bin` on first
use.

```sh
make install                              # install the CRD

kubectl apply -f manifests/namespace.yaml # placeholder two-component app...
kubectl apply -f manifests/               # ...in the `job-system` namespace

make run ARGS="--watch-namespaces=job-system"   # run against your kubeconfig

kubectl apply -f config/samples/upgrades_v1beta1_applicationupgrade.yaml
kubectl -n job-system get applicationupgrade -w
```

`make run` runs without the conversion webhook, so only v1beta1 requests work
locally. `make deploy` installs the full setup - webhook, cert-manager
certificate, admission policy - and needs
[cert-manager](https://cert-manager.io/) in the cluster.

`make test` runs the envtest-backed controller specs, which drive Deployment
status by hand to assert the ordering, the terminating-old-pod window,
idempotency, and stuck/missing-Deployment failures deterministically. The
original v1alpha1 specs run unchanged through the real conversion webhook, and
fuzzed round-trip tests check that conversion loses nothing in either
direction.

`make test-e2e` runs the suite against a live Kind cluster, where a real
Deployment controller produces the rollout statuses. It asserts the negative the
unit specs cannot — that the API is *never* patched at any point during the
worker rollout — and it is what caught the one bug the envtest specs missed
([write-up](docs/design-vs-implementation.md)).

## Status

Beta (`v1beta1`, with `v1alpha1` deprecated), and built as a focused exercise
rather than a production deployment. [`docs/design-vs-implementation.md`](docs/design-vs-implementation.md)
lists the known limitations honestly — the largest being that failure is
detected via `ProgressDeadlineExceeded` rather than per-pod classification, and
that the e2e proves the ordering on a single replica.

## License

[Apache 2.0](LICENSE).
