# API Versioning: v1alpha1 → v1beta1

This document covers how the `ApplicationUpgrade` API moved from a fixed
worker/api pair to a dependency graph without breaking anyone using the old
version: what changed, how the two versions convert into each other, where
that conversion loses information and how the loss is contained, and the
release sequence for rolling it out and eventually retiring v1alpha1.

The operator's runtime job hasn't changed: upgrade a callee before its caller.
The application developers own one compatibility rule: a new callee accepts
requests from the old caller. The operator enforces the ordering that makes
that rule sufficient. The operator's **own** API is held to the same standard.
v1beta1 must accept everything v1alpha1 clients send, and nothing a v1alpha1
client does may damage an object written through v1beta1.

---

## Why a new version

v1alpha1 hard-codes two components:

```yaml
spec:
  worker: { deploymentRef: {name: job-worker}, image: worker:v2 }
  api:    { deploymentRef: {name: job-api},    image: api:v2 }
```

Real applications have more than two components, with dependencies that form
a graph rather than a chain. v1beta1 states the dependencies directly:

```yaml
spec:
  components:
  - { name: a, deploymentRef: {name: app-a}, image: app-a:v2, dependsOn: [b, c] }
  - { name: b, deploymentRef: {name: app-b}, image: app-b:v2, dependsOn: [d] }
  - { name: c, deploymentRef: {name: app-c}, image: app-c:v2 }
  - { name: d, deploymentRef: {name: app-d}, image: app-d:v2 }
```

This changes the **shape** of the API: fields are removed and restructured,
not just added. Within one version you can only add optional fields, so a
shape change requires a new version. Because the shapes differ, the API
server can't convert by rewriting `apiVersion` (`conversion: None`). A
v1alpha1 client would receive an empty spec. That's why there's a conversion
webhook.

## Ordering semantics

`dependsOn` lists the components a component **calls**. The controller derives
the upgrade order from it with Kahn's algorithm ([`internal/dag`](../internal/dag/dag.go)):

| | Edges | Order for the example |
|---|---|---|
| Upgrade | `dependsOn` reversed (callee → caller) | waves `[c, d]`, `[b]`, `[a]` |
| Rollback (manual) | `dependsOn` as written (caller → callee) | waves `[a]`, `[b, c]`, `[d]` |

- **Waves are for display, not execution.** A component starts as soon as
  *its own* dependencies are Ready. b starts when d is done, without waiting
  for c. `status.components[].wave` shows the plan.
- **Nothing is stored that can be derived.** Indegrees and waves are
  recomputed from `dependsOn` plus the cluster state on every reconcile, so
  the controller still resumes correctly after a restart.
- **A failure halts the upgrade.** While any component reports
  `ProgressDeadlineExceeded`, no new component starts.
- **Rollback order is not reversed waves.** Reversing `[[c,d],[b],[a]]` gives
  `[[a],[b],[c,d]]`, which makes c wait for b for no reason. The correct order
  comes from running Kahn's algorithm on the original edges. The operator
  doesn't roll back automatically. This is the order an operator would follow
  by hand.

## Where validation lives

| Check | Where | Why there |
|---|---|---|
| Names unique | `listType=map` on `components` | Schema-level, free |
| `dependsOn` names existing components, no self-edge | CEL on the spec | Looks at one object only; needs no traversal |
| No two components on the same `deploymentRef` | CEL on the spec | Same |
| `deploymentRef` immutable, including removing its namespace | CEL transition rule on the whole struct | A field-level `self == oldSelf` only runs when the field is present on both sides, so it misses an optional field being *removed*. v1alpha1 had this gap; it's now closed in both versions |
| **No cycles** | Controller (config error before any patch) | Needs a graph traversal, which CEL can't express |
| Same Deployment via omitted vs. explicit namespace | Controller | CEL can't see the CR's own namespace |
| No v1alpha1 write may change the graph's shape | ValidatingAdmissionPolicy | Needs `oldObject` and the request version. See [the read-modify-write hole](#the-read-modify-write-hole) |

**CEL rules run against the version of the request, not the storage
version.** A v1alpha1 write is checked only against v1alpha1's rules and then
converted. Every constraint therefore has to be expressed in **each** served
version's own shape. The envtest specs check both.

---

## Conversion

The conversion design follows the Cluster API pattern: hub and spoke, with an
annotation that holds data the older version can't represent.

- **Hub = v1beta1**: storage version, and the version the controller
  reconciles ([`api/v1beta1/applicationupgrade_conversion.go`](../api/v1beta1/applicationupgrade_conversion.go)).
- **Spoke = v1alpha1**: implements `ConvertTo`/`ConvertFrom` against the hub
  ([`api/v1alpha1/applicationupgrade_conversion.go`](../api/v1alpha1/applicationupgrade_conversion.go)).
  N versions need N−1 converters, not one per pair.

### Up: v1alpha1 → v1beta1 (lossless)

Every v1alpha1 object is a two-node graph:

```
worker → components[0] {name: worker}
api    → components[1] {name: api, dependsOn: [worker]}
```

The single v1alpha1 phase encodes both components' progress
(`WaitingForWorkers`, `UpgradingAPI`, …). It's split into a top-level
`Progressing` plus per-component phases. The down-conversion reverses that
exactly.

### Down: v1beta1 → v1alpha1 (lossy)

v1alpha1 has two slots and no edges. A four-component graph can't fit.

- An exact worker/api pair maps back field for field.
- Anything else is mapped on a best-effort basis. `worker` shows the
  component upgraded **first**, `api` the one upgraded **last**, so the slots
  still mean something to a v1alpha1 reader.
- The full v1beta1 spec and status are stored in the
  `upgrades.lunadas.dev/conversion-data` annotation.

The stash is written only when a plain round trip would lose something. That's
decided by actually performing the round trip, not by a hand-written
"representable?" check, so the two can't drift apart as fields are added.

### Up again, with a stash

Restore from the stash first, then apply anything the v1alpha1 client
**changed**. That's any field that differs from what down-conversion produced.
A v1alpha1 client bumping `worker.image` on a four-component graph updates
component `c` and leaves `a`, `b` and `d` alone. The stash is always removed on
the way up, so **stored objects never carry it**.

### Conversion never fails

Conversion runs on every cross-version GET, LIST and WATCH. One object that
can't convert breaks `kubectl get` for the whole resource. A corrupt stash is
ignored, and the conversion falls back to the plain mapping instead of
returning an error.

### Condition types are API too

`kubectl wait --for=condition=WorkersReady` is a contract, just like a field.
Conversion can't create these conditions without breaking the round trip, so
the controller keeps setting `WorkersReady`/`APIReady` for any upgrade that is
exactly a worker/api pair. That covers everything created through v1alpha1.
Graphs report per-component progress in `status.components`, plus the new
`Ready` condition.

### Testing

- **Fuzzed round trips** in both directions, 2,000 random objects each
  ([`applicationupgrade_conversion_test.go`](../api/v1alpha1/applicationupgrade_conversion_test.go)).
  A field added later without a conversion update fails CI instead of
  silently losing data. Breaking the conversion on purpose (dropping restored
  component status) fails it within a few iterations.
- **Through a real API server.** envtest wires the CRD's conversion webhook to
  the real handler, so the original v1alpha1 controller specs now run
  unchanged through conversion against a v1beta1 controller. That's the
  strongest evidence that v1alpha1 behavior is preserved.

---

## The read-modify-write hole

The fuzz tests prove the conversion functions are inverses. They can't prove
that **clients keep the stash**, and one common workflow doesn't:

1. Someone exports an object as v1alpha1 (`kubectl get -o yaml`) and
   `kubectl apply`s it. kubectl records the whole object, **stash included**,
   in `last-applied-configuration`.
2. Later they `kubectl apply` a hand-written v1alpha1 manifest. kubectl's
   three-way merge sees the stash annotation in last-applied but not in the new
   manifest, so it **patches the annotation away**.
3. Up-conversion has nothing to restore from and stores a two-component graph.
   `a`, `b`, `d` and every edge are gone.

This was reproduced on a Kind cluster. The conversion webhook can't prevent
it, because it never sees the stored object. **Admission can.** It sees
`oldObject`, and `request.requestKind` says the write arrived as v1alpha1. A
v1alpha1 write can never legitimately rename components or change edges, so
any such change means the stash was lost. The
[ValidatingAdmissionPolicy](../config/policy/v1alpha1_shape_guard.yaml) rejects
it with a pointer to v1beta1. It's CEL evaluated in the API server, so it adds
no webhook to keep available. Image bumps through v1alpha1, and every write to
a plain worker/api pair, are still allowed.

---

## Rollout plan

Changing a CRD's storage version doesn't rewrite existing objects, and a
version can't be removed while `status.storedVersions` lists it. Each step
must also leave the previous release safe to roll back to.

| Release | v1alpha1 | v1beta1 | Notes |
|---|---|---|---|
| N | served, **storage** | served | Webhook ships; nothing is stored as v1beta1 yet, so rolling back to N−1 is safe |
| N+1 | served, **deprecated** | served, **storage** | New writes are stored as v1beta1; rollback to N works because N already understands v1beta1 |
| — | | | Run [`hack/migrate-storage-version.sh`](../hack/migrate-storage-version.sh), then confirm `storedVersions == [v1beta1]` |
| N+2 | `served: false` | served, storage | Only after `apiserver_requested_deprecated_apis` and audit logs show no v1alpha1 clients |
| N+3 | removed (with its conversion code) | | |

**This repository is at N+1.** It skips N because v1alpha1 had no production
users and alpha APIs carry no compatibility promise. With real users it
would not skip that step.

### Storage migration, observed on Kind

Starting from the previous release's CRD, with an object created through
v1alpha1:

```console
storedVersions: ["v1alpha1"]                       # before upgrading
storedVersions: ["v1alpha1","v1beta1"]             # after upgrading
etcd: "apiVersion":"upgrades.lunadas.dev/v1beta1"  # the controller's status write re-encoded it
$ hack/migrate-storage-version.sh
migrated job-system/old-completed-upgrade
storedVersions after: ["v1beta1"]
```

Note that `storedVersions` didn't shrink on its own, even after every object
had been re-encoded. The API server only adds to it. Objects that are actively
reconciled migrate naturally whenever their status is written. Idle ones (an
old `Completed` upgrade) stay as v1alpha1 bytes until something rewrites
them. They remain readable through the webhook, but they keep v1alpha1 in
`storedVersions`, and the API server will refuse the CRD update that removes
it. cert-manager 1.7 hit exactly this, and shipped
`cmctl upgrade migrate-api-version` for it.

The script refuses to touch `storedVersions` if any object failed to migrate.
Shrinking it while an object might still be stored at v1alpha1 would let a
later release strand that object.

### The preferred version changes underneath unpinned clients

Adding v1beta1 also changes what a **bare resource name** means.
`kubectl get applicationupgrade` resolves to the server's preferred version,
which is now v1beta1. A script reading `.status.phase` without pinning a
version silently starts seeing `Progressing` where it used to see
`WaitingForWorkers`. No request fails and no deprecation warning fires, so
`apiserver_requested_deprecated_apis` never counts it. The project's own Kind
e2e suite was the first client to break this way. Its ordering assertions
passed, but its status reads compared against v1alpha1 phases. Clients that
depend on a version's shape should name it
(`applicationupgrades.v1alpha1.upgrades.lunadas.dev`), and the release notes
for N+1 have to say so.


---

## Operating the webhook

The conversion webhook is on the API server's read path for every v1alpha1
request:

- **Same binary as the controller.** It's up whenever the controller is.
  Every replica serves it; only the leader reconciles.
- **Two replicas and a PodDisruptionBudget**, so node drains and cluster
  upgrades don't take it down.
- **Readiness includes the webhook server**, so the Service only routes
  conversions to a pod that is serving `/convert`.
- **cert-manager** issues the serving certificate and injects the `caBundle`
  into the CRD.
- **No external calls; pure functions.** Conversion latency adds to every
  cross-version request and every watch event.

`make run` sets `ENABLE_WEBHOOKS=false` for local development. Only v1beta1
requests work then, because nothing is serving the webhook the CRD points to.

## Known gaps

- **Clients that rebuild objects** from scratch (`kubectl replace` with a
  hand-written v1alpha1 manifest) drop the stash, just as `kubectl apply`
  does. The admission policy stops them from truncating a graph, but they
  must switch to v1beta1 to change that object.
- **Shared dependencies across upgrades** (the same Deployment in two
  ApplicationUpgrades owned by different teams) aren't arbitrated. This is the
  next real problem, and it needs a lease per Deployment or a cluster-level
  owner.
- **Health gates** stop at Deployment readiness. Gating a wave on error rate
  or SLOs would be the next step.
