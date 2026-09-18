/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	upgradesv1alpha1 "github.com/codebind-luna/ordered-upgrade-operator/api/v1alpha1"
)

// requeueInterval is a backstop poll while a Deployment rolls out. Watches on
// the managed Deployments drive most re-reconciles; this only covers the rare
// case where a readiness transition produces no observable object change.
const requeueInterval = 10 * time.Second

// progressDeadlineExceededReason is the reason the built-in Deployment
// controller sets on its Progressing condition when a rollout exceeds
// spec.progressDeadlineSeconds. It is the authoritative "this rollout is stuck"
// signal, so the operator keys terminal failure off it rather than guessing
// from individual pod states.
const progressDeadlineExceededReason = "ProgressDeadlineExceeded"

// deploymentRefIndexKey indexes ApplicationUpgrades by the namespaced names of
// the Deployments they reference. Without it, mapping a Deployment event back to
// the ApplicationUpgrades that care about it means listing and scanning every
// ApplicationUpgrade in the cluster on every Deployment event; with it the cache
// answers with only the matching objects.
const deploymentRefIndexKey = ".spec.deploymentRefs"

// Condition reasons reported on the ApplicationUpgrade.
const (
	reasonUpgrading   = "Upgrading"
	reasonRollingOut  = "RolloutInProgress"
	reasonReady       = "Ready"
	reasonRolloutFail = "RolloutFailed"
	reasonConfigError = "ConfigurationError"
	reasonUpgradeDone = "UpgradeComplete"
)

// configError marks a failure the user must fix - a missing Deployment, an
// ambiguous container reference, a namespace the operator was never granted.
// Retrying cannot resolve it, so the reconciler transitions to Failed and stops
// rather than backing off forever. Transient errors are returned bare and left
// to controller-runtime's exponential backoff.
type configError struct {
	msg string
}

func (e *configError) Error() string { return e.msg }

// ApplicationUpgradeReconciler reconciles a ApplicationUpgrade object.
type ApplicationUpgradeReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// WatchNamespaces is the set of namespaces the operator may act in. It
	// mirrors the scope of the manager's Deployment cache: a ref pointing outside
	// this set could never be served from that cache, so the reconciler rejects it
	// with an actionable message rather than surfacing an opaque cache miss as a
	// transient error that retries forever. Empty means cluster-wide.
	WatchNamespaces map[string]struct{}
}

// +kubebuilder:rbac:groups=upgrades.lunadas.dev,resources=applicationupgrades,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=upgrades.lunadas.dev,resources=applicationupgrades/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=upgrades.lunadas.dev,resources=applicationupgrades/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch

// Reconcile drives an ApplicationUpgrade toward its desired state, upgrading the
// worker (Component B) before the API (Component A). Each call performs at most
// one state transition and derives all progress from observed cluster state, so
// the loop is idempotent and safe to resume after a controller restart.
func (r *ApplicationUpgradeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ap upgradesv1alpha1.ApplicationUpgrade
	if err := r.Get(ctx, req.NamespacedName, &ap); err != nil {
		// NotFound means the CR was deleted mid-flight. There is no finalizer
		// and the operator does not own the Deployments, so there is nothing to
		// unwind - just stop reconciling it.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A terminal upgrade whose spec has not changed needs no further work.
	// Comparing observedGeneration to the current generation lets a spec edit
	// re-open a Completed or Failed upgrade for another pass.
	if isTerminal(ap.Status.Phase) && ap.Status.ObservedGeneration == ap.Generation {
		return ctrl.Result{}, nil
	}

	// Snapshot for a status-only patch at the end. Every branch below mutates
	// ap.Status in place; this is the single point where it is persisted.
	base := ap.DeepCopy()

	// A spec change invalidates conditions recorded for the previous generation.
	// Clear them so the re-plan only reports what it re-verifies this pass; a
	// failure that exits early (e.g. a stuck worker) would otherwise leave a
	// stale APIReady=True from the prior generation. The diff against base then
	// atomically replaces the persisted conditions with the freshly derived set.
	if ap.Status.ObservedGeneration != ap.Generation {
		ap.Status.Conditions = nil
	}

	result, err := r.reconcile(ctx, &ap)

	// Only record this generation as observed when the pass completed without a
	// transient error. Stamping it after an errored pass can combine with a
	// stale terminal phase (e.g. a still-Completed status carried over from the
	// previous generation) to trip the terminal short-circuit above, which would
	// then skip re-planning the new spec entirely.
	if err == nil {
		ap.Status.ObservedGeneration = ap.Generation
	}

	// Persist status only when it actually changed. Repeated "waiting" passes
	// re-derive the same status, so the guard skips a no-op API write on each.
	if !equality.Semantic.DeepEqual(base.Status, ap.Status) {
		// IgnoreNotFound: the CR may have been deleted mid-reconcile, in which
		// case there is nothing left to update and no error to report.
		if statusErr := client.IgnoreNotFound(r.Status().Patch(ctx, &ap, client.MergeFrom(base))); statusErr != nil {
			// Don't mask the primary error; surface the status write only if the
			// reconcile itself succeeded.
			if err == nil {
				err = statusErr
			}
			log.Error(statusErr, "failed to update ApplicationUpgrade status")
		}
	}
	return result, err
}

// reconcile contains the ordered state machine. It mutates ap.Status and returns
// the requeue decision; persistence is handled by the caller.
func (r *ApplicationUpgradeReconciler) reconcile(ctx context.Context, ap *upgradesv1alpha1.ApplicationUpgrade) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	worker, err := r.getComponentDeployment(ctx, ap, ap.Spec.Worker)
	if err != nil {
		return r.failOrRequeue(ap, err)
	}
	api, err := r.getComponentDeployment(ctx, ap, ap.Spec.API)
	if err != nil {
		return r.failOrRequeue(ap, err)
	}

	// ---- Component B: workers. Always upgraded first. ----
	wIdx, err := containerIndex(worker, ap.Spec.Worker.ContainerName)
	if err != nil {
		return r.failOrRequeue(ap, err)
	}
	ap.Status.CurrentWorkerImage = worker.Spec.Template.Spec.Containers[wIdx].Image

	if worker.Spec.Template.Spec.Containers[wIdx].Image != ap.Spec.Worker.Image {
		if err := r.patchImage(ctx, worker, wIdx, ap.Spec.Worker.Image); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("patched worker deployment", "deployment", client.ObjectKeyFromObject(worker), "image", ap.Spec.Worker.Image)
		r.setProgress(ap, upgradesv1alpha1.PhaseUpgradingWorkers, "Upgrading workers to "+ap.Spec.Worker.Image)
		r.setCondition(ap, upgradesv1alpha1.ConditionWorkersReady, metav1.ConditionFalse, reasonUpgrading, "Worker image patched")
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	if failed, msg := deploymentFailed(worker); failed {
		r.markFailed(ap, reasonRolloutFail, "Worker rollout failed: "+msg)
		return ctrl.Result{}, nil
	}
	if !deploymentReady(worker) {
		r.setProgress(ap, upgradesv1alpha1.PhaseWaitingForWorkers, "Waiting for worker rollout to complete")
		r.setCondition(ap, upgradesv1alpha1.ConditionWorkersReady, metav1.ConditionFalse, reasonRollingOut, "Worker rollout in progress")
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}
	r.setCondition(ap, upgradesv1alpha1.ConditionWorkersReady, metav1.ConditionTrue, reasonReady, "Workers at desired image and available")

	// ---- Component A: API. Only reached once workers are ready. ----
	aIdx, err := containerIndex(api, ap.Spec.API.ContainerName)
	if err != nil {
		return r.failOrRequeue(ap, err)
	}
	ap.Status.CurrentAPIImage = api.Spec.Template.Spec.Containers[aIdx].Image

	if api.Spec.Template.Spec.Containers[aIdx].Image != ap.Spec.API.Image {
		if err := r.patchImage(ctx, api, aIdx, ap.Spec.API.Image); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("patched api deployment", "deployment", client.ObjectKeyFromObject(api), "image", ap.Spec.API.Image)
		r.setProgress(ap, upgradesv1alpha1.PhaseUpgradingAPI, "Upgrading API to "+ap.Spec.API.Image)
		r.setCondition(ap, upgradesv1alpha1.ConditionAPIReady, metav1.ConditionFalse, reasonUpgrading, "API image patched")
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	if failed, msg := deploymentFailed(api); failed {
		r.markFailed(ap, reasonRolloutFail, "API rollout failed: "+msg)
		return ctrl.Result{}, nil
	}
	if !deploymentReady(api) {
		r.setProgress(ap, upgradesv1alpha1.PhaseWaitingForAPI, "Waiting for API rollout to complete")
		r.setCondition(ap, upgradesv1alpha1.ConditionAPIReady, metav1.ConditionFalse, reasonRollingOut, "API rollout in progress")
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}
	r.setCondition(ap, upgradesv1alpha1.ConditionAPIReady, metav1.ConditionTrue, reasonReady, "API at desired image and available")

	// Both components are at their desired image and ready.
	ap.Status.Phase = upgradesv1alpha1.PhaseCompleted
	ap.Status.Message = "Upgrade complete"
	r.setCondition(ap, upgradesv1alpha1.ConditionProgressing, metav1.ConditionFalse, reasonUpgradeDone, "Both components upgraded")
	log.Info("upgrade complete", "applicationupgrade", client.ObjectKeyFromObject(ap))
	return ctrl.Result{}, nil
}

// getComponentDeployment fetches the Deployment backing a component, resolving
// the ref namespace against the CR's namespace when omitted. A missing
// Deployment or a Forbidden response are configuration errors the user or
// cluster admin must fix; anything else is treated as transient.
func (r *ApplicationUpgradeReconciler) getComponentDeployment(
	ctx context.Context,
	ap *upgradesv1alpha1.ApplicationUpgrade,
	comp upgradesv1alpha1.ComponentSpec,
) (*appsv1.Deployment, error) {
	ns := comp.DeploymentRef.Namespace
	if ns == "" {
		ns = ap.Namespace
	}

	// Checked before the Get so the operator reports the misconfiguration itself
	// instead of relying on however the cache happens to fail for an unwatched
	// namespace.
	if !r.namespaceWatched(ns) {
		return nil, &configError{fmt.Sprintf(
			"Deployment %s/%s is outside the operator's watch set; add namespace %q to --watch-namespaces "+
				"and bind the operator's ClusterRole there",
			ns, comp.DeploymentRef.Name, ns)}
	}

	var d appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: comp.DeploymentRef.Name, Namespace: ns}, &d); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return nil, &configError{fmt.Sprintf("Deployment %s/%s not found", ns, comp.DeploymentRef.Name)}
		case apierrors.IsForbidden(err):
			return nil, &configError{fmt.Sprintf(
				"operator is not authorized on Deployment %s/%s; bind the operator's ClusterRole in that namespace",
				ns, comp.DeploymentRef.Name)}
		default:
			return nil, err
		}
	}
	return &d, nil
}

// patchImage sets the target container's image, letting the native Deployment
// controller perform the rolling update. It uses a strategic merge patch so the
// wire payload is just that one container's image (containers merge on `name`):
// a plain JSON merge patch is array-atomic and would resend the whole container
// list from our possibly-stale read, clobbering any concurrent change to the
// Deployment (an autoscaled replica count, an injected sidecar).
func (r *ApplicationUpgradeReconciler) patchImage(ctx context.Context, deploy *appsv1.Deployment, idx int, image string) error {
	patch := client.StrategicMergeFrom(deploy.DeepCopy())
	deploy.Spec.Template.Spec.Containers[idx].Image = image
	return r.Patch(ctx, deploy, patch)
}

// failOrRequeue turns a configuration error into a terminal Failed state and a
// transient error into a backed-off retry.
func (r *ApplicationUpgradeReconciler) failOrRequeue(ap *upgradesv1alpha1.ApplicationUpgrade, err error) (ctrl.Result, error) {
	var cfg *configError
	if errors.As(err, &cfg) {
		r.markFailed(ap, reasonConfigError, cfg.Error())
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, err
}

// markFailed records a terminal failure. Callers stop the upgrade by returning
// an empty result: retrying cannot help until the user changes the spec, which
// bumps the generation and re-opens the upgrade.
func (r *ApplicationUpgradeReconciler) markFailed(ap *upgradesv1alpha1.ApplicationUpgrade, reason, msg string) {
	ap.Status.Phase = upgradesv1alpha1.PhaseFailed
	ap.Status.Message = msg
	r.setCondition(ap, upgradesv1alpha1.ConditionFailed, metav1.ConditionTrue, reason, msg)
	r.setCondition(ap, upgradesv1alpha1.ConditionProgressing, metav1.ConditionFalse, reason, msg)
}

// setProgress records an in-flight phase and keeps Progressing true.
func (r *ApplicationUpgradeReconciler) setProgress(ap *upgradesv1alpha1.ApplicationUpgrade, phase upgradesv1alpha1.UpgradePhase, msg string) {
	ap.Status.Phase = phase
	ap.Status.Message = msg
	r.setCondition(ap, upgradesv1alpha1.ConditionProgressing, metav1.ConditionTrue, reasonUpgrading, msg)
}

// setCondition upserts a status condition, stamping it with the generation it
// was observed at so clients can tell fresh conditions from stale ones.
func (r *ApplicationUpgradeReconciler) setCondition(
	ap *upgradesv1alpha1.ApplicationUpgrade,
	condType string,
	status metav1.ConditionStatus,
	reason, msg string,
) {
	meta.SetStatusCondition(&ap.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ap.Generation,
	})
}

// namespaceWatched reports whether the operator is configured to act in ns. An
// empty WatchNamespaces means cluster-wide, matching an unscoped cache.
func (r *ApplicationUpgradeReconciler) namespaceWatched(ns string) bool {
	if len(r.WatchNamespaces) == 0 {
		return true
	}
	_, ok := r.WatchNamespaces[ns]
	return ok
}

// isTerminal reports whether a phase is an end state.
func isTerminal(p upgradesv1alpha1.UpgradePhase) bool {
	return p == upgradesv1alpha1.PhaseCompleted || p == upgradesv1alpha1.PhaseFailed
}

// containerIndex resolves which container to patch. An unset containerName is
// only valid for a single-container pod template; a name that matches nothing,
// or an unset name on a multi-container template, is a configuration error so a
// sidecar is never patched by accident.
func containerIndex(d *appsv1.Deployment, name string) (int, error) {
	containers := d.Spec.Template.Spec.Containers
	if name == "" {
		if len(containers) != 1 {
			return 0, &configError{fmt.Sprintf(
				"Deployment %s/%s has %d containers; set containerName to disambiguate",
				d.Namespace, d.Name, len(containers))}
		}
		return 0, nil
	}
	for i := range containers {
		if containers[i].Name == name {
			return i, nil
		}
	}
	return 0, &configError{fmt.Sprintf("container %q not found in Deployment %s/%s", name, d.Namespace, d.Name)}
}

// deploymentReady reports whether a Deployment has fully rolled out to its
// current spec, using the same criteria as `kubectl rollout status`:
//   - the Deployment controller has observed the latest spec
//     (observedGeneration >= generation) - checked first, since right after a
//     patch the status still describes the previous revision;
//   - every desired replica runs the updated template (updatedReplicas == desired);
//   - no old-revision replicas remain (replicas == updatedReplicas); and
//   - all updated replicas are available (availableReplicas == updatedReplicas).
//
// The "no old replicas remain" clause is what makes the B-before-A ordering
// sound: it holds the upgrade until every old worker pod is gone - not merely
// until the new ones are up - so the API is never upgraded while an old,
// terminating worker can still serve a request.
func deploymentReady(d *appsv1.Deployment) bool {
	if d.Status.ObservedGeneration < d.Generation {
		return false
	}
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return d.Status.UpdatedReplicas == desired &&
		d.Status.Replicas == d.Status.UpdatedReplicas &&
		d.Status.AvailableReplicas == d.Status.UpdatedReplicas
}

// deploymentFailed reports a stuck rollout via the Deployment's own Progressing
// condition. ProgressDeadlineExceeded is the terminal signal; a merely
// Available=False Deployment is normal mid-rollout and is not treated as failed.
//
// Like deploymentReady, the failure signal is only trusted once the Deployment
// controller has observed the current spec (observedGeneration >= generation).
// Right after the operator re-patches a Deployment that previously failed - the
// recovery path, where the user corrects a bad image - its status still carries
// the old ProgressDeadlineExceeded from the prior rollout. Without this guard the
// operator would re-fail the recovery it just initiated and latch it terminally.
func deploymentFailed(d *appsv1.Deployment) (bool, string) {
	if d.Status.ObservedGeneration < d.Generation {
		return false, ""
	}
	for i := range d.Status.Conditions {
		c := d.Status.Conditions[i]
		if c.Type == appsv1.DeploymentProgressing &&
			c.Status == corev1.ConditionFalse &&
			c.Reason == progressDeadlineExceededReason {
			msg := c.Message
			if msg == "" {
				msg = "progress deadline exceeded"
			}
			return true, msg
		}
	}
	return false, ""
}

// SetupWithManager wires the controller. The managed Deployments are not owned
// by the operator (they pre-exist and are only mutated), so instead of Owns()
// they are watched with a handler that maps a changed Deployment back to any
// ApplicationUpgrade whose worker or api ref names it. That mapping is served by
// a field index registered here, so it costs a keyed cache lookup rather than a
// full list per Deployment event.
func (r *ApplicationUpgradeReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		ctx, &upgradesv1alpha1.ApplicationUpgrade{}, deploymentRefIndexKey, deploymentRefKeys,
	); err != nil {
		return fmt.Errorf("indexing ApplicationUpgrade by %s: %w", deploymentRefIndexKey, err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&upgradesv1alpha1.ApplicationUpgrade{}).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.upgradesForDeployment),
		).
		Named("applicationupgrade").
		Complete(r)
}

// upgradesForDeployment maps a Deployment event to the ApplicationUpgrades that
// reference it, so a rollout completing (or failing) re-triggers reconciliation
// without polling. The lookup is served by deploymentRefIndexKey: every
// Deployment in the watched namespaces produces events, and only a vanishing
// fraction of them are referenced by an ApplicationUpgrade, so scanning every CR
// per event would make the operator's cost scale with cluster size rather than
// with the number of upgrades in flight.
func (r *ApplicationUpgradeReconciler) upgradesForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	var list upgradesv1alpha1.ApplicationUpgradeList
	if err := r.List(ctx, &list, client.MatchingFields{
		deploymentRefIndexKey: deploymentKey(obj.GetNamespace(), obj.GetName()),
	}); err != nil {
		logf.FromContext(ctx).Error(err, "listing ApplicationUpgrades for Deployment watch")
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
		})
	}
	return requests
}

// deploymentRefKeys extracts the index keys for an ApplicationUpgrade: the
// namespaced name of every Deployment it references. A ref that omits the
// namespace is resolved against the CR's own namespace - the same defaulting
// getComponentDeployment applies, so the index and the reconciler always agree
// on which object a ref denotes.
func deploymentRefKeys(obj client.Object) []string {
	ap, ok := obj.(*upgradesv1alpha1.ApplicationUpgrade)
	if !ok {
		return nil
	}

	refs := []upgradesv1alpha1.DeploymentRef{ap.Spec.Worker.DeploymentRef, ap.Spec.API.DeploymentRef}
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		ns := ref.Namespace
		if ns == "" {
			ns = ap.Namespace
		}
		keys = append(keys, deploymentKey(ns, ref.Name))
	}
	return keys
}

// deploymentKey is the single spelling of an index key, used by both the
// indexer and the lookup so the two cannot drift.
func deploymentKey(namespace, name string) string {
	return namespace + "/" + name
}
