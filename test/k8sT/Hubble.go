// Copyright 2020 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8sTest

import (
	"context"
	"fmt"

	"github.com/cilium/cilium/pkg/annotation"
	. "github.com/cilium/cilium/test/ginkgo-ext"
	"github.com/cilium/cilium/test/helpers"

	. "github.com/onsi/gomega"
)

var _ = Describe("K8sHubbleTest", func() {

	var (
		kubectl        *helpers.Kubectl
		ciliumFilename string
		demoPath       string

		app1Service = "app1-service"
		app1Labels  = "id=app1,zgroup=testapp"
		apps        = []string{helpers.App1, helpers.App2, helpers.App3}
	)

	BeforeAll(func() {
		kubectl = helpers.CreateKubectl(helpers.K8s1VMName(), logger)
		ciliumFilename = helpers.TimestampFilename("cilium.yaml")

		demoPath = helpers.ManifestGet(kubectl.BasePath(), "demo.yaml")

		DeployCiliumOptionsAndDNS(kubectl, ciliumFilename, map[string]string{
			"global.hubble.enabled": "true",
		})
	})

	AfterFailed(func() {
		kubectl.CiliumReport(helpers.CiliumNamespace,
			"cilium endpoint list")
	})

	JustAfterEach(func() {
		kubectl.ValidateNoErrorsInLogs(CurrentGinkgoTestDescription().Duration)
	})

	AfterEach(func() {
		ExpectAllPodsTerminated(kubectl)
	})

	AfterAll(func() {
		kubectl.DeleteCiliumDS()
		ExpectAllPodsTerminated(kubectl)
		kubectl.CloseSSHClient()
	})

	waitForHubble := func(ciliumPod string) {
		hubbleReady := func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), helpers.ShortCommandTimeout)
			defer cancel()

			// FIXME: Ideally, we would use the `hubble status` CLI here. It is not
			// available in the Cilium container right now.
			res := kubectl.CiliumExecContext(ctx, ciliumPod, "cilium observe --since 0")
			return res.WasSuccessful()
		}

		By("Waiting for Hubble to become ready on cilium pod %s", ciliumPod)
		helpers.WithTimeout(hubbleReady,
			fmt.Sprintf("timed out waiting for hubble  to become ready"),
			&helpers.TimeoutConfig{Timeout: helpers.MidCommandTimeout})
	}

	hubbleObserve := func(ctx context.Context, ciliumPod string, args string) *helpers.CmdRes {
		cmd := fmt.Sprintf("cilium observe %s", args)
		By("Executing %q on pod %s/%s", cmd, helpers.CiliumNamespace, ciliumPod)
		return kubectl.ExecPodCmdBackground(ctx, helpers.CiliumNamespace, ciliumPod, cmd)
	}

	Context("Hubble Observe", func() {
		var (
			namespaceForTest string
			appPods          map[string]string
			app1ClusterIP    string
			app1Port         int
			ciliumPodK8s1    string
		)

		BeforeAll(func() {
			namespaceForTest = helpers.GenerateNamespaceForTest("")
			kubectl.NamespaceDelete(namespaceForTest)
			res := kubectl.NamespaceCreate(namespaceForTest)
			res.ExpectSuccess("could not create namespace")

			res = kubectl.Apply(helpers.ApplyOptions{FilePath: demoPath, Namespace: namespaceForTest})
			res.ExpectSuccess("could not create resource")

			err := kubectl.WaitforPods(namespaceForTest, "-l zgroup=testapp", helpers.HelperTimeout)
			Expect(err).Should(BeNil(), "Test pods are not ready after timeout")

			appPods = helpers.GetAppPods(apps, namespaceForTest, kubectl, "id")

			app1ClusterIP, app1Port, err = kubectl.GetServiceHostPort(namespaceForTest, app1Service)
			Expect(err).To(BeNil(), "Cannot get service in %q namespace", namespaceForTest)

			ciliumPodK8s1, err = kubectl.GetCiliumPodOnNodeWithLabel(helpers.CiliumNamespace, helpers.K8s1)
			Expect(err).Should(BeNil(), "Cannot get cilium pod on %s", helpers.K8s1)

			waitForHubble(ciliumPodK8s1)
		})

		AfterAll(func() {
			kubectl.Delete(demoPath)
		})

		It("Test L3/L4 Flow", func() {
			ctx, cancel := context.WithTimeout(context.Background(), helpers.MidCommandTimeout)
			defer cancel()

			observe := hubbleObserve(ctx, ciliumPodK8s1, fmt.Sprintf(
				"--follow --last 1 --json --type trace --from-pod %s/%s --to-label %s --to-namespace %s --to-port %d",
				namespaceForTest, appPods[helpers.App2], app1Labels, app1Port, namespaceForTest))

			res := kubectl.ExecPodCmd(namespaceForTest, appPods[helpers.App2],
				helpers.CurlFail(fmt.Sprintf("http://%s/public", app1ClusterIP)))
			res.ExpectSuccess("%q cannot curl clusterIP %q", appPods[helpers.App2], app1ClusterIP)

			// wait for matching json output to appear
			Expect(observe.WaitUntilMatch(`"Type":"L3_L4"`)).
				To(BeNil(), "hubble observe query timed out")
		})

		It("Test L7 Flow", func() {
			By("Adding visibility annotation <Ingress/80/TCP/HTTP> on pod with labels %s", app1Labels)
			res := kubectl.Exec(fmt.Sprintf("%s annotate pod -n %s -l %s %s=\"<Ingress/80/TCP/HTTP>\"", helpers.KubectlCmd, namespaceForTest, app1Labels, annotation.ProxyVisibility))
			res.ExpectSuccess("adding proxy visibility annotation failed")
			Expect(kubectl.CiliumEndpointWaitReady()).To(BeNil())

			ctx, cancel := context.WithTimeout(context.Background(), helpers.MidCommandTimeout)
			defer cancel()
			observe := hubbleObserve(ctx, ciliumPodK8s1, fmt.Sprintf(
				"--follow --last 1 --json --type l7 --from-pod %s/%s --to-label %s --to-namespace %s --protocol http",
				namespaceForTest, appPods[helpers.App2], app1Labels, namespaceForTest))

			res = kubectl.ExecPodCmd(namespaceForTest, appPods[helpers.App2],
				helpers.CurlFail(fmt.Sprintf("http://%s/public", app1ClusterIP)))
			res.ExpectSuccess("%q cannot curl clusterIP %q", appPods[helpers.App2], app1ClusterIP)

			Expect(observe.WaitUntilMatch(`"Type":"L7"`)).
				To(BeNil(), "hubble observe query timed out")
			Expect(observe.CountLines()).To(BeNumerically("==", 1))

			By("Removing visibility annotation on pod with labels %s", app1Labels)
			res = kubectl.Exec(fmt.Sprintf("%s annotate pod -n %s -l %s %s-", helpers.KubectlCmd, namespaceForTest, app1Labels, annotation.ProxyVisibility))
			res.ExpectSuccess("removing proxy visibility annotation failed")
		})
	})
})
