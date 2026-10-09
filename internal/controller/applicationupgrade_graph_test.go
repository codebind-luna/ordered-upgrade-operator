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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	upgradesv1alpha1 "github.com/codebind-luna/ordered-upgrade-operator/api/v1alpha1"
	upgradesv1beta1 "github.com/codebind-luna/ordered-upgrade-operator/api/v1beta1"
)

// These specs cover what v1beta1 adds: an upgrade over a dependency graph, the
// validation that keeps the graph well formed, and what a v1alpha1 client sees
// of a graph it cannot represent.
//
// The graph throughout is a -> {b, c}, b -> d: a calls b and c, b calls d.
// Upgrade waves are [c, d], [b], [a].

const graphNS = "default"

// graphDeploymentName is the Deployment backing a graph component.
func graphDeploymentName(component string) string { return "app-" + component }

func graphComponent(name string, deps ...string) upgradesv1beta1.Component {
	return upgradesv1beta1.Component{
		Name:          name,
		DeploymentRef: upgradesv1beta1.DeploymentRef{Name: graphDeploymentName(name)},
		Image:         newImage,
		DependsOn:     deps,
	}
}

func graphUpgrade(name string) *upgradesv1beta1.ApplicationUpgrade {
	return &upgradesv1beta1.ApplicationUpgrade{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: graphNS},
		Spec: upgradesv1beta1.ApplicationUpgradeSpec{Components: []upgradesv1beta1.Component{
			graphComponent("a", "b", "c"),
			graphComponent("b", "d"),
			graphComponent("c"),
			graphComponent("d"),
		}},
	}
}

func graphDeployment(component string) *appsv1.Deployment {
	replicas := int32(1)
	labels := map[string]string{"component": component}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: graphDeploymentName(component), Namespace: graphNS},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: oldImage}}},
			},
		},
	}
}

func rollOut(component string) {
	var d appsv1.Deployment
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: graphDeploymentName(component), Namespace: graphNS}, &d)).To(Succeed())
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas = 1
	d.Status.UpdatedReplicas = 1
	d.Status.ReadyReplicas = 1
	d.Status.AvailableReplicas = 1
	d.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable",
	}}
	Expect(k8sClient.Status().Update(ctx, &d)).To(Succeed())
}

func graphImage(component string) string {
	var d appsv1.Deployment
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: graphDeploymentName(component), Namespace: graphNS}, &d)).To(Succeed())
	return d.Spec.Template.Spec.Containers[0].Image
}

var _ = Describe("ApplicationUpgrade dependency graph", func() {
	const crName = "graph-under-test"
	var (
		reconciler *ApplicationUpgradeReconciler
		crKey      = types.NamespacedName{Name: crName, Namespace: graphNS}
	)

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: crKey})
		Expect(err).NotTo(HaveOccurred())
	}
	getCR := func() *upgradesv1beta1.ApplicationUpgrade {
		cr := &upgradesv1beta1.ApplicationUpgrade{}
		Expect(k8sClient.Get(ctx, crKey, cr)).To(Succeed())
		return cr
	}
	images := func() map[string]string {
		return map[string]string{"a": graphImage("a"), "b": graphImage("b"), "c": graphImage("c"), "d": graphImage("d")}
	}

	BeforeEach(func() {
		reconciler = &ApplicationUpgradeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		for _, c := range []string{"a", "b", "c", "d"} {
			Expect(k8sClient.Create(ctx, graphDeployment(c))).To(Succeed())
			rollOut(c)
		}
	})

	AfterEach(func() {
		cr := &upgradesv1beta1.ApplicationUpgrade{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: graphNS}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cr))).To(Succeed())
		for _, c := range []string{"a", "b", "c", "d"} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, graphDeployment(c)))).To(Succeed())
		}
	})

	It("upgrades callees before callers, starting each as soon as its own dependencies are done", func() {
		Expect(k8sClient.Create(ctx, graphUpgrade(crName))).To(Succeed())

		By("starting both components with no dependencies")
		reconcileOnce()
		Expect(images()).To(Equal(map[string]string{"a": oldImage, "b": oldImage, "c": newImage, "d": newImage}))

		cr := getCR()
		Expect(cr.Status.Phase).To(Equal(upgradesv1beta1.PhaseProgressing))
		waves := map[string]int32{}
		for _, st := range cr.Status.Components {
			waves[st.Name] = st.Wave
		}
		Expect(waves).To(Equal(map[string]int32{"a": 3, "b": 2, "c": 1, "d": 1}))

		By("starting b once d is done, without waiting for c")
		rollOut("d")
		reconcileOnce()
		Expect(graphImage("b")).To(Equal(newImage))
		Expect(graphImage("a")).To(Equal(oldImage), "a still waits on b and c")

		By("holding a until both b and c are done")
		rollOut("b")
		reconcileOnce()
		Expect(graphImage("a")).To(Equal(oldImage), "c has not finished")

		rollOut("c")
		reconcileOnce()
		Expect(graphImage("a")).To(Equal(newImage))

		By("completing once a is done")
		rollOut("a")
		reconcileOnce()
		Expect(getCR().Status.Phase).To(Equal(upgradesv1beta1.PhaseCompleted))
	})

	It("starts nothing new while any component has failed", func() {
		Expect(k8sClient.Create(ctx, graphUpgrade(crName))).To(Succeed())
		reconcileOnce() // starts c and d

		By("c getting stuck while d completes")
		var d appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: graphDeploymentName("c"), Namespace: graphNS}, &d)).To(Succeed())
		d.Status.ObservedGeneration = d.Generation
		d.Status.Conditions = []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: progressDeadlineExceededReason,
		}}
		Expect(k8sClient.Status().Update(ctx, &d)).To(Succeed())
		rollOut("d")
		reconcileOnce()

		Expect(graphImage("b")).To(Equal(oldImage),
			"b's own dependency is done, but a failure elsewhere stops the upgrade from spreading")
		cr := getCR()
		Expect(cr.Status.Phase).To(Equal(upgradesv1beta1.PhaseFailed))
		Expect(cr.Status.Message).To(ContainSubstring("c rollout failed"))
	})

	It("rejects a dependency cycle before patching anything", func() {
		cr := graphUpgrade(crName)
		cr.Spec.Components[3].DependsOn = []string{"a"} // d -> a closes a -> b -> d
		Expect(k8sClient.Create(ctx, cr)).To(Succeed(), "CEL cannot detect cycles; the controller must")

		reconcileOnce()

		got := getCR()
		Expect(got.Status.Phase).To(Equal(upgradesv1beta1.PhaseFailed))
		Expect(got.Status.Message).To(ContainSubstring("dependency cycle"))
		Expect(images()).To(Equal(map[string]string{"a": oldImage, "b": oldImage, "c": oldImage, "d": oldImage}))
	})

	It("rejects two components resolving to the same Deployment", func() {
		cr := graphUpgrade(crName)
		// CEL sees an omitted and an explicit namespace as different refs; both
		// resolve to default/app-c.
		cr.Spec.Components[1].DeploymentRef = upgradesv1beta1.DeploymentRef{Name: graphDeploymentName("c"), Namespace: graphNS}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())

		reconcileOnce()

		got := getCR()
		Expect(got.Status.Phase).To(Equal(upgradesv1beta1.PhaseFailed))
		Expect(got.Status.Message).To(ContainSubstring("both reference Deployment default/app-c"))
		Expect(graphImage("c")).To(Equal(oldImage))
	})
})

var _ = Describe("ApplicationUpgrade v1beta1 validation", func() {
	create := func(mutate func(*upgradesv1beta1.ApplicationUpgrade)) error {
		cr := graphUpgrade("validation")
		mutate(cr)
		err := k8sClient.Create(ctx, cr)
		if err == nil {
			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())
		}
		return err
	}

	It("accepts a well-formed graph", func() {
		Expect(create(func(*upgradesv1beta1.ApplicationUpgrade) {})).To(Succeed())
	})

	It("rejects a dependency on a component that does not exist", func() {
		err := create(func(cr *upgradesv1beta1.ApplicationUpgrade) {
			cr.Spec.Components[0].DependsOn = []string{"ghost"}
		})
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("dependsOn must name other components"))
	})

	It("rejects a component depending on itself", func() {
		err := create(func(cr *upgradesv1beta1.ApplicationUpgrade) {
			cr.Spec.Components[2].DependsOn = []string{"c"}
		})
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
	})

	It("rejects two components with the same name", func() {
		err := create(func(cr *upgradesv1beta1.ApplicationUpgrade) {
			cr.Spec.Components[3].Name = "c"
			cr.Spec.Components[1].DependsOn = []string{"c"}
		})
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
	})

	It("rejects two components referencing the same Deployment", func() {
		err := create(func(cr *upgradesv1beta1.ApplicationUpgrade) {
			cr.Spec.Components[3].DeploymentRef.Name = graphDeploymentName("c")
		})
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("each component must reference a different Deployment"))
	})

	It("keeps each component's deploymentRef immutable, including removing its namespace", func() {
		cr := graphUpgrade("v1beta1-immutable")
		cr.Spec.Components[2].DeploymentRef.Namespace = graphNS
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, cr)).To(Succeed()) })

		cr.Spec.Components[2].Image = recoveryImage
		Expect(k8sClient.Update(ctx, cr)).To(Succeed(), "images stay editable")

		cr.Spec.Components[2].DeploymentRef.Namespace = ""
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, cr))).To(BeTrue())
	})
})

var _ = Describe("Reading and writing a v1beta1 graph through v1alpha1", func() {
	const crName = "graph-through-v1alpha1"
	key := types.NamespacedName{Name: crName, Namespace: graphNS}

	AfterEach(func() {
		cr := &upgradesv1beta1.ApplicationUpgrade{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: graphNS}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cr))).To(Succeed())
	})

	It("survives a read-modify-write by a v1alpha1 client", func() {
		Expect(k8sClient.Create(ctx, graphUpgrade(crName))).To(Succeed())

		By("reading it through v1alpha1: two slots and a stash of the rest")
		old := &upgradesv1alpha1.ApplicationUpgrade{}
		Expect(k8sClient.Get(ctx, key, old)).To(Succeed())
		Expect(old.Spec.Worker.DeploymentRef.Name).To(Equal(graphDeploymentName("c")), "first in upgrade order")
		Expect(old.Spec.API.DeploymentRef.Name).To(Equal(graphDeploymentName("a")), "last in upgrade order")
		Expect(old.Annotations).To(HaveKey(upgradesv1alpha1.ConversionDataAnnotation))

		By("updating the one image it can see")
		old.Spec.Worker.Image = recoveryImage
		Expect(k8sClient.Update(ctx, old)).To(Succeed())

		By("reading it back through v1beta1: the edit landed and nothing was lost")
		got := &upgradesv1beta1.ApplicationUpgrade{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		want := graphUpgrade(crName).Spec
		want.Components[2].Image = recoveryImage
		Expect(got.Spec).To(Equal(want))
		Expect(got.Annotations).NotTo(HaveKey(upgradesv1alpha1.ConversionDataAnnotation),
			"the stash exists only in the v1alpha1 view, never in storage")
	})

	It("refuses a v1alpha1 write that dropped the stash, instead of truncating the graph", func() {
		// Reproduces what kubectl apply's three-way merge does once an exported
		// v1alpha1 view has been recorded in last-applied: a later v1alpha1
		// manifest without the annotation patches it away. Up-conversion then
		// has nothing to restore from and would store a two-component graph.
		// Conversion cannot see the stored object; the admission policy can.
		Expect(k8sClient.Create(ctx, graphUpgrade(crName))).To(Succeed())

		stripped := func() *upgradesv1alpha1.ApplicationUpgrade {
			old := &upgradesv1alpha1.ApplicationUpgrade{}
			Expect(k8sClient.Get(ctx, key, old)).To(Succeed())
			delete(old.Annotations, upgradesv1alpha1.ConversionDataAnnotation)
			old.Spec.Worker.Image = recoveryImage
			return old
		}

		// A newly created policy takes a moment to become active. Probe with a
		// dry run - admission still runs - so an early attempt cannot truncate
		// the graph before the policy is enforcing.
		Eventually(func() error {
			return k8sClient.Update(ctx, stripped(), client.DryRunAll)
		}).Should(Satisfy(apierrors.IsInvalid), "the policy should become active")

		err := k8sClient.Update(ctx, stripped())
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("update it through upgrades.lunadas.dev/v1beta1"))

		got := &upgradesv1beta1.ApplicationUpgrade{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Spec).To(Equal(graphUpgrade(crName).Spec), "the graph is untouched")
	})

	It("still accepts v1alpha1 writes to an upgrade that is a plain worker/api pair", func() {
		Expect(k8sClient.Create(ctx, validUpgrade(crName))).To(Succeed())

		old := &upgradesv1alpha1.ApplicationUpgrade{}
		Expect(k8sClient.Get(ctx, key, old)).To(Succeed())
		Expect(old.Annotations).NotTo(HaveKey(upgradesv1alpha1.ConversionDataAnnotation),
			"a pair v1alpha1 can represent needs no stash")
		old.Spec.Worker.Image = recoveryImage
		Expect(k8sClient.Update(ctx, old)).To(Succeed())
	})
})
