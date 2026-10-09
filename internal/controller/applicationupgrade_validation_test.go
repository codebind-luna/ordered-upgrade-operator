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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	upgradesv1alpha1 "github.com/codebind-luna/ordered-upgrade-operator/api/v1alpha1"
	upgradesv1beta1 "github.com/codebind-luna/ordered-upgrade-operator/api/v1beta1"
)

// These specs assert the CRD's schema validation against a real API server
// (envtest): what the server accepts and, just as importantly, what it rejects
// before the controller ever runs.

// validUpgrade returns a well-formed ApplicationUpgrade the API server accepts.
func validUpgrade(name string) *upgradesv1alpha1.ApplicationUpgrade {
	return &upgradesv1alpha1.ApplicationUpgrade{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: upgradesv1alpha1.ApplicationUpgradeSpec{
			Worker: upgradesv1alpha1.ComponentSpec{
				DeploymentRef: upgradesv1alpha1.DeploymentRef{Name: "job-worker"},
				Image:         "nginx:1.27.0",
				ContainerName: "worker",
			},
			API: upgradesv1alpha1.ComponentSpec{
				DeploymentRef: upgradesv1alpha1.DeploymentRef{Name: "job-api"},
				Image:         "nginx:1.27.0",
				ContainerName: "api",
			},
		},
	}
}

// asV1beta1 converts a v1alpha1 object to the hub the way the conversion
// webhook does.
func asV1beta1(in *upgradesv1alpha1.ApplicationUpgrade) *upgradesv1beta1.ApplicationUpgrade {
	out := &upgradesv1beta1.ApplicationUpgrade{}
	Expect(in.ConvertTo(out)).To(Succeed())
	return out
}

var _ = Describe("ApplicationUpgrade CRD validation", func() {
	Context("accepting well-formed specs", func() {
		It("accepts a fully specified upgrade", func() {
			cr := validUpgrade("valid-full")
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())
		})

		It("accepts an upgrade without containerName (single-container default)", func() {
			cr := validUpgrade("valid-no-container")
			cr.Spec.Worker.ContainerName = ""
			cr.Spec.API.ContainerName = ""
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())
		})
	})

	Context("rejecting malformed specs", func() {
		It("rejects an empty image", func() {
			cr := validUpgrade("bad-empty-image")
			cr.Spec.Worker.Image = ""
			Expect(apierrors.IsInvalid(k8sClient.Create(ctx, cr))).To(BeTrue())
		})

		It("rejects an empty deploymentRef name", func() {
			cr := validUpgrade("bad-empty-name")
			cr.Spec.Worker.DeploymentRef.Name = ""
			Expect(apierrors.IsInvalid(k8sClient.Create(ctx, cr))).To(BeTrue())
		})

		It("rejects a deploymentRef name that is not a DNS-1123 subdomain", func() {
			cr := validUpgrade("bad-name-pattern")
			cr.Spec.Worker.DeploymentRef.Name = "Job_Worker"
			Expect(apierrors.IsInvalid(k8sClient.Create(ctx, cr))).To(BeTrue())
		})

		It("rejects a containerName that is not a DNS-1123 label", func() {
			cr := validUpgrade("bad-container-pattern")
			cr.Spec.Worker.ContainerName = "Worker"
			Expect(apierrors.IsInvalid(k8sClient.Create(ctx, cr))).To(BeTrue())
		})

		It("rejects worker and api referencing the same Deployment", func() {
			cr := validUpgrade("bad-same-deploy")
			cr.Spec.API.DeploymentRef.Name = "job-worker"
			Expect(apierrors.IsInvalid(k8sClient.Create(ctx, cr))).To(BeTrue())
		})
	})

	Context("enforcing immutability", func() {
		It("allows changing the image but not the deploymentRef", func() {
			cr := validUpgrade("immutable-check")
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, cr)).To(Succeed())
			})

			By("accepting an image bump")
			cr.Spec.Worker.Image = "nginx:1.28.0"
			Expect(k8sClient.Update(ctx, cr)).To(Succeed())

			By("rejecting a deploymentRef retarget")
			cr.Spec.Worker.DeploymentRef.Name = "other-worker"
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, cr))).To(BeTrue())
		})

		It("rejects removing an explicit namespace, not just changing it", func() {
			// A field-level self == oldSelf rule only runs when the field exists on
			// both sides, so it cannot see the namespace being removed.
			cr := validUpgrade("immutable-ns-removal")
			cr.Spec.Worker.DeploymentRef.Namespace = "default"
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, cr)).To(Succeed())
			})

			cr.Spec.Worker.DeploymentRef.Namespace = ""
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, cr))).To(BeTrue())
		})
	})
})
