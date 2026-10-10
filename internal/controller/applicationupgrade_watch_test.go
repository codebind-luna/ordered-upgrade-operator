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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	upgradesv1alpha1 "github.com/codebind-luna/ordered-upgrade-operator/api/v1alpha1"
	upgradesv1beta1 "github.com/codebind-luna/ordered-upgrade-operator/api/v1beta1"
)

// These specs cover how the operator finds work and how far it is allowed to
// reach: the field index that maps a Deployment event back to the
// ApplicationUpgrades referencing it, and the watch-set guard that refuses a ref
// pointing outside the namespaces the Deployment cache covers.
//
// The mapping specs use a fake client because the index has to be registered on
// the client under test; the envtest client in suite_test.go is a direct
// (uncached) client with no indexes, and a field selector on a custom resource
// is not something the API server can answer on its own.

var _ = Describe("Deployment-to-ApplicationUpgrade mapping", func() {
	const (
		appNS     = "job-system"
		controlNS = "upgrade-control"
	)

	// upgradeRefs builds a CR in crNS whose worker and api refs carry the given
	// namespaces ("" meaning "default to the CR's own namespace"). It is written
	// as v1alpha1 for brevity and converted, since the controller indexes the
	// v1beta1 hub and a fake client performs no conversion.
	upgradeRefs := func(name, crNS, workerNS, apiNS string) *upgradesv1beta1.ApplicationUpgrade {
		cr := validUpgrade(name)
		cr.Namespace = crNS
		cr.Spec.Worker.DeploymentRef.Namespace = workerNS
		cr.Spec.API.DeploymentRef.Namespace = apiNS
		return asV1beta1(cr)
	}

	// newReconciler wires a fake client carrying the same index the manager
	// registers in SetupWithManager, so the lookup path under test is the real one.
	newReconciler := func(objs ...client.Object) *ApplicationUpgradeReconciler {
		builder := fake.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithIndex(&upgradesv1beta1.ApplicationUpgrade{}, deploymentRefIndexKey, deploymentRefKeys)
		for _, o := range objs {
			builder = builder.WithObjects(o)
		}
		c := builder.Build()
		return &ApplicationUpgradeReconciler{Client: c, Scheme: c.Scheme()}
	}

	deployment := func(namespace, name string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	}

	Describe("the index function", func() {
		It("keys both refs, defaulting an omitted namespace to the CR's own", func() {
			cr := upgradeRefs("defaulted", appNS, "", "")
			Expect(deploymentRefKeys(cr)).To(ConsistOf(
				appNS+"/job-worker",
				appNS+"/job-api",
			))
		})

		It("keys an explicit ref namespace rather than the CR's", func() {
			cr := upgradeRefs("cross-ns", controlNS, appNS, appNS)
			Expect(deploymentRefKeys(cr)).To(ConsistOf(
				appNS+"/job-worker",
				appNS+"/job-api",
			), "a control-namespace CR must be found by the namespace it targets")
		})

		It("returns nothing for an object that is not an ApplicationUpgrade", func() {
			Expect(deploymentRefKeys(deployment(appNS, "job-worker"))).To(BeEmpty())
		})
	})

	Describe("the watch handler", func() {
		It("enqueues only the ApplicationUpgrades referencing the changed Deployment", func() {
			matching := upgradeRefs("matching", appNS, "", "")
			unrelated := upgradeRefs("unrelated", appNS, "", "")
			unrelated.Spec.Components[0].DeploymentRef.Name = "other-worker"
			unrelated.Spec.Components[1].DeploymentRef.Name = "other-api"

			r := newReconciler(matching, unrelated)

			Expect(r.upgradesForDeployment(ctx, deployment(appNS, "job-worker"))).To(ConsistOf(
				reconcile.Request{NamespacedName: types.NamespacedName{Name: "matching", Namespace: appNS}},
			))
		})

		It("enqueues nothing for a Deployment no ApplicationUpgrade references", func() {
			r := newReconciler(upgradeRefs("matching", appNS, "", ""))

			Expect(r.upgradesForDeployment(ctx, deployment(appNS, "some-other-app"))).To(BeEmpty(),
				"the cluster is full of Deployments the operator does not manage")
		})

		It("does not match a same-named Deployment in a different namespace", func() {
			r := newReconciler(upgradeRefs("matching", appNS, "", ""))

			Expect(r.upgradesForDeployment(ctx, deployment("somewhere-else", "job-worker"))).To(BeEmpty())
		})

		It("enqueues a control-namespace CR for a Deployment in the namespace it targets", func() {
			r := newReconciler(upgradeRefs("cross-ns", controlNS, appNS, appNS))

			Expect(r.upgradesForDeployment(ctx, deployment(appNS, "job-worker"))).To(ConsistOf(
				reconcile.Request{NamespacedName: types.NamespacedName{Name: "cross-ns", Namespace: controlNS}},
			))
		})
	})
})

var _ = Describe("ApplicationUpgrade watch-set enforcement", func() {
	const (
		namespace  = "default"
		workerName = "job-worker"
		apiName    = "job-api"
		crName     = "outside-watch-set"
	)

	var crKey = types.NamespacedName{Name: crName, Namespace: namespace}

	AfterEach(func() {
		cr := &upgradesv1alpha1.ApplicationUpgrade{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cr))).To(Succeed())
	})

	It("fails an upgrade whose refs fall outside the watched namespaces", func() {
		By("creating an upgrade in `default` while the operator watches only `job-system`")
		Expect(k8sClient.Create(ctx, validUpgrade(crName))).To(Succeed())

		reconciler := &ApplicationUpgradeReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			WatchNamespaces: map[string]struct{}{"job-system": {}},
		}

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: crKey})
		Expect(err).NotTo(HaveOccurred(), "an unwatched namespace is a config error, not a retryable one")

		cr := &upgradesv1alpha1.ApplicationUpgrade{}
		Expect(k8sClient.Get(ctx, crKey, cr)).To(Succeed())
		Expect(cr.Status.Phase).To(Equal(upgradesv1alpha1.PhaseFailed))
		Expect(cr.Status.Message).To(ContainSubstring("outside the operator's watch set"))
		Expect(cr.Status.Message).To(ContainSubstring("--watch-namespaces"),
			"the message must tell the admin which knob to turn")
	})

	It("proceeds normally when the ref namespace is watched", func() {
		Expect(k8sClient.Create(ctx, validUpgrade(crName))).To(Succeed())

		reconciler := &ApplicationUpgradeReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			WatchNamespaces: map[string]struct{}{namespace: {}},
		}

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: crKey})
		Expect(err).NotTo(HaveOccurred())

		cr := &upgradesv1alpha1.ApplicationUpgrade{}
		Expect(k8sClient.Get(ctx, crKey, cr)).To(Succeed())
		Expect(cr.Status.Phase).To(Equal(upgradesv1alpha1.PhaseFailed))
		Expect(cr.Status.Message).To(ContainSubstring("not found"),
			"the Deployments do not exist in this spec, but the watch set did not stand in the way")
	})

	It("treats an empty watch set as cluster-wide", func() {
		r := &ApplicationUpgradeReconciler{}
		Expect(r.namespaceWatched("anything")).To(BeTrue())
	})
})
