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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	upgradesv1alpha1 "github.com/codebind-luna/ordered-upgrade-operator/api/v1alpha1"
)

// These specs exercise the reconciler against a real API server (envtest).
// envtest runs the API server and etcd but not kube-controller-manager, so no
// Deployment controller advances rollouts - the tests drive Deployment status
// by hand to reproduce "rollout in progress", "rolled out", and "stuck". That
// is exactly the seam the reconciler reads, so it lets us assert the ordering
// and failure behavior deterministically.

const (
	oldImage      = "nginx:1.26.0"
	newImage      = "nginx:1.27.0"
	recoveryImage = "nginx:1.28.0"
)

var _ = Describe("ApplicationUpgrade Controller", func() {
	const (
		namespace  = "default"
		workerName = "job-worker"
		apiName    = "job-api"
		crName     = "upgrade-under-test"
	)

	var (
		reconciler *ApplicationUpgradeReconciler
		crKey      = types.NamespacedName{Name: crName, Namespace: namespace}
		workerKey  = types.NamespacedName{Name: workerName, Namespace: namespace}
		apiKey     = types.NamespacedName{Name: apiName, Namespace: namespace}
	)

	// newDeployment builds a single-container Deployment at the old image.
	newDeployment := func(name, container string) *appsv1.Deployment {
		replicas := int32(1)
		labels := map[string]string{"app": name}
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: container, Image: oldImage}},
					},
				},
			},
		}
	}

	// markRolledOut makes a Deployment report a completed rollout of its current
	// spec: observedGeneration caught up and every replica updated and available.
	markRolledOut := func(key types.NamespacedName) {
		var d appsv1.Deployment
		Expect(k8sClient.Get(ctx, key, &d)).To(Succeed())
		replicas := int32(1)
		if d.Spec.Replicas != nil {
			replicas = *d.Spec.Replicas
		}
		d.Status.ObservedGeneration = d.Generation
		d.Status.Replicas = replicas
		d.Status.UpdatedReplicas = replicas
		d.Status.ReadyReplicas = replicas
		d.Status.AvailableReplicas = replicas
		// A completed rollout reports Progressing=True/NewReplicaSetAvailable. Set
		// it explicitly so this also clears any prior ProgressDeadlineExceeded, the
		// way the real Deployment controller resets the condition when a corrected
		// spec rolls out - otherwise a recovered Deployment still looks failed.
		d.Status.Conditions = []appsv1.DeploymentCondition{{
			Type:   appsv1.DeploymentProgressing,
			Status: corev1.ConditionTrue,
			Reason: "NewReplicaSetAvailable",
		}}
		Expect(k8sClient.Status().Update(ctx, &d)).To(Succeed())
	}

	// markNewUpOldLingering models the surge window: the new-revision replicas
	// are all up and available, but an old-revision pod has not finished
	// terminating yet (status.Replicas exceeds updatedReplicas). The rollout is
	// not complete - an old worker can still serve a request.
	markNewUpOldLingering := func(key types.NamespacedName) {
		var d appsv1.Deployment
		Expect(k8sClient.Get(ctx, key, &d)).To(Succeed())
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		d.Status.ObservedGeneration = d.Generation
		d.Status.UpdatedReplicas = desired
		d.Status.AvailableReplicas = desired
		d.Status.ReadyReplicas = desired
		d.Status.Replicas = desired + 1 // one old pod still terminating
		Expect(k8sClient.Status().Update(ctx, &d)).To(Succeed())
	}

	// markStuck makes a Deployment report a rollout that blew its progress
	// deadline - the terminal failure signal the reconciler keys off.
	markStuck := func(key types.NamespacedName) {
		var d appsv1.Deployment
		Expect(k8sClient.Get(ctx, key, &d)).To(Succeed())
		d.Status.ObservedGeneration = d.Generation
		d.Status.Conditions = []appsv1.DeploymentCondition{{
			Type:    appsv1.DeploymentProgressing,
			Status:  corev1.ConditionFalse,
			Reason:  progressDeadlineExceededReason,
			Message: "ReplicaSet has timed out progressing",
		}}
		Expect(k8sClient.Status().Update(ctx, &d)).To(Succeed())
	}

	imageOf := func(key types.NamespacedName) string {
		var d appsv1.Deployment
		Expect(k8sClient.Get(ctx, key, &d)).To(Succeed())
		return d.Spec.Template.Spec.Containers[0].Image
	}

	getCR := func() *upgradesv1alpha1.ApplicationUpgrade {
		cr := &upgradesv1alpha1.ApplicationUpgrade{}
		Expect(k8sClient.Get(ctx, crKey, cr)).To(Succeed())
		return cr
	}

	reconcileOnce := func() reconcile.Result {
		res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: crKey})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	BeforeEach(func() {
		reconciler = &ApplicationUpgradeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		By("creating both component Deployments at the old image, already rolled out")
		Expect(k8sClient.Create(ctx, newDeployment(workerName, "worker"))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDeployment(apiName, "api"))).To(Succeed())
		markRolledOut(workerKey)
		markRolledOut(apiKey)

		By("creating an ApplicationUpgrade targeting the new image")
		cr := validUpgrade(crName)
		cr.Spec.Worker.Image = newImage
		cr.Spec.API.Image = newImage
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
	})

	AfterEach(func() {
		cr := &upgradesv1alpha1.ApplicationUpgrade{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cr))).To(Succeed())
		for _, key := range []types.NamespacedName{workerKey, apiKey} {
			d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, d))).To(Succeed())
		}
	})

	Context("when both components need upgrading", func() {
		It("upgrades the worker before touching the API", func() {
			By("first reconcile patches the worker only")
			reconcileOnce()
			Expect(imageOf(workerKey)).To(Equal(newImage))
			Expect(imageOf(apiKey)).To(Equal(oldImage), "API must not be upgraded before the worker is ready")
			Expect(getCR().Status.Phase).To(Equal(upgradesv1alpha1.PhaseUpgradingWorkers))

			By("while the worker rollout is in progress the API stays untouched")
			reconcileOnce()
			Expect(imageOf(apiKey)).To(Equal(oldImage))
			Expect(getCR().Status.Phase).To(Equal(upgradesv1alpha1.PhaseWaitingForWorkers))

			By("once the worker is ready the API is patched")
			markRolledOut(workerKey)
			reconcileOnce()
			Expect(imageOf(apiKey)).To(Equal(newImage))
			Expect(getCR().Status.Phase).To(Equal(upgradesv1alpha1.PhaseUpgradingAPI))

			By("waiting for the API rollout")
			reconcileOnce()
			Expect(getCR().Status.Phase).To(Equal(upgradesv1alpha1.PhaseWaitingForAPI))

			By("once the API is ready the upgrade completes")
			markRolledOut(apiKey)
			reconcileOnce()

			cr := getCR()
			Expect(cr.Status.Phase).To(Equal(upgradesv1alpha1.PhaseCompleted))
			Expect(cr.Status.CurrentWorkerImage).To(Equal(newImage))
			Expect(cr.Status.CurrentAPIImage).To(Equal(newImage))
			Expect(meta.IsStatusConditionTrue(cr.Status.Conditions, upgradesv1alpha1.ConditionWorkersReady)).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(cr.Status.Conditions, upgradesv1alpha1.ConditionAPIReady)).To(BeTrue())
		})

		It("does not upgrade the API while an old worker pod is still terminating", func() {
			reconcileOnce() // patches worker
			markNewUpOldLingering(workerKey)
			reconcileOnce()

			Expect(imageOf(apiKey)).To(Equal(oldImage),
				"the worker rollout is not complete until old pods are gone")
			Expect(getCR().Status.Phase).To(Equal(upgradesv1alpha1.PhaseWaitingForWorkers))

			By("the API upgrades only once the last old worker pod is gone")
			markRolledOut(workerKey)
			reconcileOnce()
			Expect(imageOf(apiKey)).To(Equal(newImage))
		})

		It("is idempotent: repeated reconciles while the worker rolls out never advance to the API", func() {
			reconcileOnce() // patches worker
			for range 3 {
				reconcileOnce()
			}
			Expect(imageOf(apiKey)).To(Equal(oldImage))
			Expect(getCR().Status.Phase).To(Equal(upgradesv1alpha1.PhaseWaitingForWorkers))
		})
	})

	Context("when a rollout fails", func() {
		It("marks the upgrade Failed and never upgrades the API when the worker is stuck", func() {
			reconcileOnce() // patches worker
			markStuck(workerKey)
			reconcileOnce()

			cr := getCR()
			Expect(cr.Status.Phase).To(Equal(upgradesv1alpha1.PhaseFailed))
			Expect(meta.IsStatusConditionTrue(cr.Status.Conditions, upgradesv1alpha1.ConditionFailed)).To(BeTrue())
			Expect(imageOf(apiKey)).To(Equal(oldImage), "a failed worker rollout must block the API upgrade")
		})

		It("recovers when the worker image is corrected after a failed rollout", func() {
			By("driving the worker rollout to Failed")
			reconcileOnce() // patches worker to newImage
			markStuck(workerKey)
			reconcileOnce()
			Expect(getCR().Status.Phase).To(Equal(upgradesv1alpha1.PhaseFailed))
			Expect(imageOf(apiKey)).To(Equal(oldImage))

			By("correcting the image, which re-opens the upgrade at a new generation")
			cr := getCR()
			cr.Spec.Worker.Image = recoveryImage
			cr.Spec.API.Image = recoveryImage
			Expect(k8sClient.Update(ctx, cr)).To(Succeed())

			By("re-patching the worker with the corrected image")
			reconcileOnce()
			Expect(imageOf(workerKey)).To(Equal(recoveryImage))

			By("not re-failing on the previous rollout's stale ProgressDeadlineExceeded")
			// The worker Deployment still carries the failed condition, and its
			// observedGeneration now lags the just-patched spec. deploymentFailed
			// must ignore the condition until the Deployment controller catches up;
			// without that guard the reconcile would latch Failed forever here.
			reconcileOnce()
			Expect(getCR().Status.Phase).To(Equal(upgradesv1alpha1.PhaseWaitingForWorkers),
				"a corrected rollout must not latch Failed on the previous rollout's condition")

			By("completing the recovery in worker-before-API order")
			markRolledOut(workerKey)
			reconcileOnce()
			Expect(imageOf(apiKey)).To(Equal(recoveryImage))
			markRolledOut(apiKey)
			reconcileOnce()
			Expect(getCR().Status.Phase).To(Equal(upgradesv1alpha1.PhaseCompleted))
		})
	})

	Context("when the spec references a Deployment that does not exist", func() {
		It("marks the upgrade Failed with a configuration message", func() {
			Expect(k8sClient.Delete(ctx, &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: workerName, Namespace: namespace},
			})).To(Succeed())

			reconcileOnce()

			cr := getCR()
			Expect(cr.Status.Phase).To(Equal(upgradesv1alpha1.PhaseFailed))
			Expect(cr.Status.Message).To(ContainSubstring("not found"))
		})
	})

	Context("when the components are already at the desired image", func() {
		It("completes without patching anything", func() {
			By("moving both Deployments to the new image up front")
			for _, key := range []types.NamespacedName{workerKey, apiKey} {
				var d appsv1.Deployment
				Expect(k8sClient.Get(ctx, key, &d)).To(Succeed())
				d.Spec.Template.Spec.Containers[0].Image = newImage
				Expect(k8sClient.Update(ctx, &d)).To(Succeed())
				markRolledOut(key)
			}

			reconcileOnce()
			Expect(getCR().Status.Phase).To(Equal(upgradesv1alpha1.PhaseCompleted))
		})
	})
})
