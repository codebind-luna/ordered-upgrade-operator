# Design vs. Implementation Reflection

This document is an honest account of the journey from
[`DESIGN.md`](../DESIGN.md) to the code. Where the design still describes the
system accurately, I have not repeated it here - this focuses on where reality
diverged, what I learned building it, and what I knowingly left undone.

`DESIGN.md` was updated in a couple of places to match what I learned during
implementation (most notably the worker-ready criteria). The differences below
describe the design as it stood before implementation began.

---

## What changed from the original design, and why

### 1. The "worker ready" definition was too weak to enforce the invariant

This was the most important change. My original readiness criteria were:
observedGeneration caught up, `updatedReplicas == desired`, and
`availableReplicas == desired`. That looks complete, but it is satisfied
**during the surge window of a rolling update**, while an old-revision worker pod
is still terminating. At that moment the new pods are up and available, yet an
old worker can still receive and serve a request. If I let the API upgrade at
that point, a new-contract API pod could call an old worker - exactly the
failure the whole ordering exists to prevent.

The fix was to add a third clause,
[`d.Status.Replicas == d.Status.UpdatedReplicas`](../internal/controller/applicationupgrade_controller.go)
("no old-revision pods remain"), matching what `kubectl rollout status` actually
waits for. I added a dedicated test for it -
*"does not upgrade the API while an old worker pod is still terminating"* - which
models the surge by setting `status.Replicas = desired + 1`.

The lesson: "the new version is available" and "the old version is gone" are
different conditions, and only the second one makes callee-before-caller
ordering sound.

### 2. Image patching had to be a strategic merge, not a plain merge

The design said "patch the Deployment" without specifying how. My first instinct
was a JSON merge patch (`client.MergeFrom`). Implementing it, I realized a JSON
merge patch is **array-atomic**: it resends the entire `containers` list from my
possibly-stale cached read. If anything else changed the Deployment concurrently
(an autoscaler adjusting replicas, a mutating webhook injecting a sidecar),
patching the image would clobber it.

I switched to
[`client.StrategicMergeFrom`](../internal/controller/applicationupgrade_controller.go),
which merges the containers list on `name`, so the wire payload is just the one
container's new image. This is invisible in the design but material to
correctness in a real cluster.

### 3. RBAC needed `list` and `watch`, not just `get` and `patch`

The design's Security section modeled the grant as `get` and `patch` on
Deployments. That is all the reconcile logic itself needs, but I chose to react
to rollout completion via a **watch** on the referenced Deployments rather than
polling. A watch requires `list` and `watch` verbs for the informer cache, so
the generated role grants `get;list;watch;patch`. A design that only reasoned
about the reconcile body missed the verbs the chosen watch strategy implies.

### 4. Status is written once per pass, behind an equality guard

The design listed "Update Status" as a step but didn't specify the mechanism.
Implementing it naively - writing status wherever it changes - produces churn:
every "still waiting" reconcile would issue an identical status write. I settled
on a **single-writer** pattern: the inner `reconcile` mutates `ap.Status` in
memory, and the outer `Reconcile` persists it exactly once, and only when
`equality.Semantic.DeepEqual` shows it actually changed. This keeps the write
path in one place and avoids no-op API traffic on every requeue.

---

## What I discovered during implementation that I didn't anticipate

- **envtest has no Deployment controller.** envtest runs the API server and etcd
  but not kube-controller-manager, so nothing advances a rollout on its own.
  Rather than a limitation, this turned out to be the right test seam: the specs
  drive Deployment `status` by hand (`markRolledOut`, `markNewUpOldLingering`,
  `markStuck`) to reproduce "in progress", "rolled out", and "stuck"
  deterministically - which is exactly the surface the reconciler reads. It let
  me assert the ordering and failure behavior without flakiness.
- **The "just patched" race is real and subtle.** Immediately after patching, a
  Deployment's `status` still describes the old revision, so replica counts can
  read as ready for pods that are about to be replaced. This is why the
  `observedGeneration` guard has to be evaluated *first*; I understood it in
  principle from the design but only appreciated how easily it produces a
  false "done" once I was writing the readiness function.
- **CEL validation covered more than I expected.** I was able to push several
  invariants into the CRD as declarative `XValidation` rules (immutable
  `deploymentRef`, worker and API must reference different Deployments) instead
  of writing an admission webhook, which kept the operator webhook-free.

---

## Trade-offs made against a fixed scope

- **Failure detection keys solely off `ProgressDeadlineExceeded`.** The design
  listed ImagePullBackOff and CrashLoopBackOff as failure scenarios. I did not
  write per-pod classification for them; instead I rely on the Deployment's own
  `Progressing`/`ProgressDeadlineExceeded` condition, which those conditions
  eventually trigger. This is simpler and avoids interpreting individual pod
  states, at the cost of being **slower** to surface a failure (it waits out
  `progressDeadlineSeconds`).
- **Watch-plus-backstop instead of pure event-driven.** Reconciliation is driven
  by a Deployment watch, but I kept a `RequeueAfter` backstop (`requeueInterval`)
  for the rare readiness transition that produces no observable object change.
  It is belt-and-suspenders, not strictly necessary.
- **One `ApplicationUpgrade` per application is assumed.** Overlapping CRs
  targeting the same Deployments are treated as user error rather than arbitrated.
- **The e2e suite is the scaffold.** I invested test effort in the envtest specs,
  where the ordering and failure logic actually lives, rather than in a Kind-based
  e2e run.

---

## What I prioritized vs. deferred

**Prioritized** (the parts that make the invariant correct):

- The ordered B-before-A state machine with at-most-one-transition semantics.
- A readiness definition that genuinely waits for old worker pods to drain.
- Config-vs-transient error separation, so an unfixable spec fails fast and a
  blip retries.
- Deterministic tests for ordering, the terminating-pod window, idempotency, and
  stuck/missing-Deployment failures.

**Deferred or simplified:**

- Automatic rollback (explicitly out of scope in the design).
- Per-pod failure classification (ImagePullBackOff/CrashLoopBackOff surfaced
  faster than the progress deadline).
- Application-level version verification over the components' HTTP endpoints.
- A filled-in Kind e2e suite.
- Multi-CR arbitration.

---

## What I would do next with more time

1. **Faster, richer failure detection.** Watch pod status behind the Deployment
   and surface ImagePullBackOff/CrashLoopBackOff in the CR's conditions
   immediately, instead of waiting for `ProgressDeadlineExceeded`.
2. **An explicit restart-recovery test.** Restart-safety is a consequence of
   deriving all state from the cluster, and the idempotency test exercises it
   indirectly, but a test that reconstructs the reconciler mid-upgrade and
   asserts it resumes correctly would state the guarantee directly.
3. **A real Kind e2e** that applies the sample against the placeholder manifests
   and asserts the ordering with a live Deployment controller.
4. **Optional application-level readiness** - verify the served version over the
   component's HTTP endpoint - as an opt-in behind a spec field, since it trades
   coupling for a stronger guarantee.
5. **Rollback / pause semantics** and multi-CR arbitration if the use case grows
   beyond a single upgrade per application.

---

## Known limitations and gaps

- **The `Pending` phase is defined but never set.** The enum includes it, but the
  state machine transitions straight from the initial state into
  `UpgradingWorkers` or a terminal phase. It is dead surface I would either wire
  up or remove.
- **Failure latency.** Because failure is detected via `ProgressDeadlineExceeded`,
  how quickly an upgrade reports `Failed` depends on the target Deployment having
  a sensible `progressDeadlineSeconds`. The placeholder manifests set it low for
  exactly this reason.
- **Readiness is Deployment-status-based, not served-version-verified.** "Ready"
  means the rollout completed and replicas are available - not an independent
  confirmation that the running process serves the new version. This is a
  deliberate decoupling, but it is a real gap for anyone expecting end-to-end
  version verification.
- **No admission webhook.** Validation is declarative (CEL) only. It catches the
  invariants encoded as rules but not anything requiring cross-object lookups.
- **Overlapping CRs are unguarded.** Two `ApplicationUpgrade`s pointing at the
  same Deployments will race; nothing detects or arbitrates that.
- **e2e is scaffold-only.** Confidence in ordering and failure handling comes
  from the envtest specs, not from a live end-to-end run.
