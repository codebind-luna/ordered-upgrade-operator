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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/codebind-luna/ordered-upgrade-operator/test/utils"
)

// These specs run the operator against a live Deployment controller, which the
// envtest specs in internal/controller deliberately do not have. envtest can
// prove the reconciler reads Deployment status correctly because the specs write
// that status by hand; only a real cluster proves the ordering holds against the
// status a real rollout actually produces, in the order it produces it.
//
// Ordering is asserted with Consistently, not Eventually: the claim is that the
// API is *never* patched during the worker rollout, and only a spec that watches
// the whole window can test a negative like that. The worker Deployment is given
// a minReadySeconds so that window is wide enough to observe deterministically
// rather than depending on how fast nginx happens to start.

const (
	appNamespace = "job-system"
	workerDeploy = "job-worker"
	apiDeploy    = "job-api"

	baseImage    = "nginx:1.25.3"
	upgradeImage = "nginx:1.27.0"
	brokenImage  = "nginx:this-tag-does-not-exist"

	// workerMinReady widens the window in which the worker is rolled out but not
	// yet Available, so the "API stays untouched" assertion has something to
	// observe. It must exceed rolloutObservation below.
	workerMinReady = 40

	// rolloutObservation is how long the API is watched for an early patch while
	// the worker rollout is still in flight.
	rolloutObservation = 20 * time.Second

	// stuckDeadline shortens the worker's progressDeadlineSeconds for the failure
	// spec, so a stuck rollout surfaces ProgressDeadlineExceeded without the spec
	// waiting out the two minutes the shipped manifest allows. The API server
	// enforces progressDeadlineSeconds > minReadySeconds, so that spec clears
	// minReadySeconds in the same patch - it has no rollout window to observe,
	// since its image can never be pulled.
	stuckDeadline = 30

	// healthyDeadline is restored before the recovery spec so a rollout that is
	// genuinely healthy is not failed by the shortened deadline above.
	healthyDeadline = 600
)

// kubectl runs a kubectl command against the app namespace and returns trimmed
// stdout, failing the spec if it errors.
func kubectl(args ...string) string {
	out, err := utils.Run(exec.Command("kubectl", args...))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "kubectl %s", strings.Join(args, " "))
	return strings.TrimSpace(out)
}

// imageOf reports the image a Deployment's sole container currently runs.
func imageOf(deployment string) string {
	return kubectl("get", "deployment", deployment, "-n", appNamespace,
		"-o", "jsonpath={.spec.template.spec.containers[0].image}")
}

// upgradeField reads one field off the ApplicationUpgrade's status.
func upgradeField(name, jsonPath string) string {
	return kubectl("get", "applicationupgrade", name, "-n", appNamespace, "-o", "jsonpath="+jsonPath)
}

// applyUpgrade writes an ApplicationUpgrade manifest to a temp file and applies
// it, so each spec can choose its own images.
func applyUpgrade(name, workerImage, apiImage string) {
	manifest := fmt.Sprintf(`apiVersion: upgrades.lunadas.dev/v1alpha1
kind: ApplicationUpgrade
metadata:
  name: %s
  namespace: %s
spec:
  worker:
    deploymentRef:
      name: %s
    image: %s
    containerName: worker
  api:
    deploymentRef:
      name: %s
    image: %s
    containerName: api
`, name, appNamespace, workerDeploy, workerImage, apiDeploy, apiImage)

	path := filepath.Join(GinkgoT().TempDir(), name+".yaml")
	Expect(os.WriteFile(path, []byte(manifest), 0o600)).To(Succeed())
	kubectl("apply", "-f", path)
}

var _ = Describe("Ordered upgrade against a live cluster", Ordered, func() {
	BeforeAll(func() {
		By("deploying the two placeholder components")
		kubectl("apply", "-f", filepath.Join("manifests", "namespace.yaml"))
		kubectl("apply", "-f", "manifests")

		By("slowing the worker rollout so the ordering window is observable")
		// minReadySeconds delays availableReplicas without stopping the rollout,
		// which is exactly the state the operator must refuse to treat as ready.
		kubectl("patch", "deployment", workerDeploy, "-n", appNamespace, "--type", "merge",
			"-p", fmt.Sprintf(`{"spec":{"minReadySeconds":%d}}`, workerMinReady))

		By("waiting for both components to finish their initial rollout")
		for _, d := range []string{workerDeploy, apiDeploy} {
			kubectl("rollout", "status", "deployment/"+d, "-n", appNamespace, "--timeout", "5m")
		}
		Expect(imageOf(workerDeploy)).To(Equal(baseImage))
		Expect(imageOf(apiDeploy)).To(Equal(baseImage))
	})

	AfterAll(func() {
		By("removing the application namespace")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", appNamespace, "--ignore-not-found"))
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		// A failure here is almost always "the operator did not move the upgrade
		// along", so the dump has to show what the operator saw: the CR's full
		// status, the Deployment conditions it reads, and its own logs.
		By("dumping cluster state and controller logs for debugging")
		dump := func(args ...string) {
			out, _ := utils.Run(exec.Command("kubectl", args...))
			_, _ = fmt.Fprintf(GinkgoWriter, "--- kubectl %s ---\n%s\n", strings.Join(args, " "), out)
		}
		for _, args := range [][]string{
			{"applicationupgrade,deployment,pod", "-n", appNamespace, "-o", "wide"},
			{"applicationupgrade", "-n", appNamespace, "-o", "yaml"},
			{"deployment", "-n", appNamespace, "-o",
				"jsonpath={range .items[*]}{.metadata.name}{\" gen=\"}{.metadata.generation}" +
					"{\" observed=\"}{.status.observedGeneration}{\" image=\"}" +
					"{.spec.template.spec.containers[0].image}{\" conditions=\"}" +
					"{range .status.conditions[*]}{.type}/{.status}/{.reason}{\" \"}{end}{\"\\n\"}{end}"},
			{"events", "-n", appNamespace, "--sort-by=.lastTimestamp"},
		} {
			dump(append([]string{"get"}, args...)...)
		}
		dump("logs", "-l", "control-plane=controller-manager", "-n", namespace, "--tail=80")
	})

	It("upgrades the worker first and holds the API until the worker is fully rolled out", func() {
		By("applying an upgrade that bumps both components")
		applyUpgrade("e2e-ordered", upgradeImage, upgradeImage)

		By("the worker is patched promptly")
		Eventually(func() string {
			return imageOf(workerDeploy)
		}, 2*time.Minute, time.Second).Should(Equal(upgradeImage))

		By("the API is never patched while the worker rollout is in flight")
		// The negative is the whole point: a reconciler that checked only
		// "new pods available" would patch the API somewhere inside this window,
		// while an old worker pod is still serving.
		Consistently(func() string {
			return imageOf(apiDeploy)
		}, rolloutObservation, time.Second).Should(Equal(baseImage),
			"the API must not move until the worker rollout has fully drained")

		By("and the upgrade reports itself as still working on the worker")
		Expect(upgradeField("e2e-ordered", "{.status.phase}")).To(BeElementOf(
			"UpgradingWorkers", "WaitingForWorkers"))

		By("once the worker is genuinely ready the API rolls and the upgrade completes")
		Eventually(func() string {
			return upgradeField("e2e-ordered", "{.status.phase}")
		}, 5*time.Minute, 2*time.Second).Should(Equal("Completed"))

		Expect(imageOf(apiDeploy)).To(Equal(upgradeImage))
		Expect(upgradeField("e2e-ordered", "{.status.currentWorkerImage}")).To(Equal(upgradeImage))
		Expect(upgradeField("e2e-ordered", "{.status.currentAPIImage}")).To(Equal(upgradeImage))

		By("both readiness conditions are reported true")
		for _, condition := range []string{"WorkersReady", "APIReady"} {
			status := upgradeField("e2e-ordered",
				fmt.Sprintf(`{.status.conditions[?(@.type=="%s")].status}`, condition))
			Expect(status).To(Equal("True"), "condition %s", condition)
		}
	})

	It("fails the upgrade and never touches the API when the worker rollout is stuck", func() {
		By("clearing the completed upgrade so only one ApplicationUpgrade is in play")
		kubectl("delete", "applicationupgrade", "e2e-ordered", "-n", appNamespace)

		By("shortening the worker's progress deadline so a stuck rollout surfaces quickly")
		// minReadySeconds goes back to zero in the same patch: the API server
		// rejects a progressDeadlineSeconds that does not exceed it, and this spec
		// has no ordering window to hold open - the worker never starts at all.
		kubectl("patch", "deployment", workerDeploy, "-n", appNamespace, "--type", "merge",
			"-p", fmt.Sprintf(`{"spec":{"minReadySeconds":0,"progressDeadlineSeconds":%d}}`, stuckDeadline))

		By("applying an upgrade whose worker image cannot be pulled")
		applyUpgrade("e2e-stuck", brokenImage, "nginx:1.28.0")

		By("the upgrade is marked Failed once the rollout misses its deadline")
		Eventually(func() string {
			return upgradeField("e2e-stuck", "{.status.phase}")
		}, 5*time.Minute, 2*time.Second).Should(Equal("Failed"))

		Expect(upgradeField("e2e-stuck", "{.status.message}")).To(ContainSubstring("Worker rollout failed"))

		By("the API was never patched")
		Expect(imageOf(apiDeploy)).To(Equal(upgradeImage),
			"a failed worker upgrade must leave the API where it was")

		By("and it stays Failed rather than flapping back to progressing")
		Consistently(func() string {
			return upgradeField("e2e-stuck", "{.status.phase}")
		}, 15*time.Second, 3*time.Second).Should(Equal("Failed"))
	})

	It("recovers when the worker image is corrected", func() {
		By("restoring a progress deadline that a healthy rollout can meet")
		// The failure spec shortened it to catch a stuck rollout quickly; left that
		// way, a slow-but-healthy rollout could trip ProgressDeadlineExceeded too.
		kubectl("patch", "deployment", workerDeploy, "-n", appNamespace, "--type", "merge",
			"-p", fmt.Sprintf(`{"spec":{"progressDeadlineSeconds":%d}}`, healthyDeadline))

		By("correcting the worker image on the failed upgrade")
		applyUpgrade("e2e-stuck", upgradeImage, "nginx:1.28.0")

		By("the upgrade re-plans and runs to completion")
		// The spec edit bumps the generation, which re-opens the terminal Failed
		// state; the stale ProgressDeadlineExceeded on the Deployment must not
		// re-fail the recovery the operator just started.
		Eventually(func() string {
			return upgradeField("e2e-stuck", "{.status.phase}")
		}, 5*time.Minute, 2*time.Second).Should(Equal("Completed"))

		Expect(imageOf(workerDeploy)).To(Equal(upgradeImage))
		Expect(imageOf(apiDeploy)).To(Equal("nginx:1.28.0"))
	})
})
