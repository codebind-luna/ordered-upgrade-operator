# Placeholder Application Manifests

Two-component job processing application the operator manages. These are
stand-ins - no real application logic - used to demonstrate ordered upgrades.

| Component | Deployment | Service | Role |
|-----------|------------|---------|------|
| A - Job API | `job-api` | `job-api` | Externally-facing; calls the worker over HTTP |
| B - Job Worker | `job-worker` | `job-worker` | Internal; upgraded first |

All resources live in the `job-system` namespace.

## Labeling

Resources follow the `app.kubernetes.io/*` recommended labels. The two
components are distinguished by `app.kubernetes.io/component` (`api` vs
`worker`), and grouped by `app.kubernetes.io/part-of: job-processing-system`.

Selectors match only the stable identity labels (`name` + `component`), never
the version label - selectors are immutable and the version changes on every
upgrade.

## Version tracking

The **container image tag** is the version source of truth the operator
modifies during an upgrade (`nginx:1.25.3` -> a newer tag). It is visible
directly in the pod spec, which makes an upgrade observable:

```
kubectl -n job-system get deploy job-worker \
  -o jsonpath='{.spec.template.spec.containers[0].image}'
```

The `app.kubernetes.io/version` label is seeded to match the initial tag as a
human-readable marker. The operator identifies each container to patch by a
fixed container name (`api`, `worker`), which the `ApplicationUpgrade` spec
references via `containerName`.

`progressDeadlineSeconds` is set low so a stuck rollout surfaces
`ProgressDeadlineExceeded` promptly - the Deployment condition the operator
reads to declare an upgrade failed.

## Apply

```
kubectl apply -f manifests/namespace.yaml
kubectl apply -f manifests/
```
