/*
Copyright 2024.

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
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/utils/ptr"

	kubefloworgv1beta1 "github.com/kubeflow/notebooks/workspaces/controller/api/v1beta1"
	"github.com/kubeflow/notebooks/workspaces/controller/internal/config"
)

var _ = Describe("Workspace Controller", func() {

	// Define utility constants for object names and testing timeouts/durations and intervals.
	const (
		namespaceName = "default"

		// how long to wait in "Eventually" blocks
		timeout = time.Second * 10

		// how long to wait in "Consistently" blocks
		duration = time.Second * 10 //nolint:unused

		// how frequently to poll for conditions
		interval = time.Millisecond * 250

		// tolerance for timestamp comparisons (in milliseconds)
		tolerance = int64(5000)
	)

	Context("When updating a Workspace", Ordered, func() {

		// Define utility variables for object names.
		// NOTE: to avoid conflicts between parallel tests, resource names are unique to each test
		var (
			workspaceName     string
			workspaceKindName string
			workspaceKey      types.NamespacedName
		)

		BeforeAll(func() {
			uniqueName := "ws-update-test"
			workspaceName = fmt.Sprintf("workspace-%s", uniqueName)
			workspaceKindName = fmt.Sprintf("workspacekind-%s", uniqueName)
			workspaceKey = types.NamespacedName{Name: workspaceName, Namespace: namespaceName}

			By("creating the WorkspaceKind")
			workspaceKind := NewExampleWorkspaceKind1(workspaceKindName)
			Expect(k8sClient.Create(ctx, workspaceKind)).To(Succeed())

			By("creating the Workspace")
			workspace := NewExampleWorkspace1(workspaceName, namespaceName, workspaceKindName)
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
		})

		AfterAll(func() {
			By("deleting the Workspace")
			workspace := &kubefloworgv1beta1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      workspaceName,
					Namespace: namespaceName,
				},
			}
			Expect(k8sClient.Delete(ctx, workspace)).To(Succeed())

			By("deleting the WorkspaceKind")
			workspaceKind := &kubefloworgv1beta1.WorkspaceKind{
				ObjectMeta: metav1.ObjectMeta{
					Name: workspaceKindName,
				},
			}
			Expect(k8sClient.Delete(ctx, workspaceKind)).To(Succeed())
		})

		It("should not allow updating immutable fields", func() {
			By("getting the Workspace")
			workspace := &kubefloworgv1beta1.Workspace{}
			Expect(k8sClient.Get(ctx, workspaceKey, workspace)).To(Succeed())
			patch := client.MergeFrom(workspace.DeepCopy())

			By("failing to update the `spec.kind` field")
			newWorkspace := workspace.DeepCopy()
			newWorkspace.Spec.Kind = "new-kind"
			Expect(k8sClient.Patch(ctx, newWorkspace, patch)).NotTo(Succeed())
		})
	})

	Context("When reconciling a Workspace", Serial, Ordered, func() {

		// Define utility variables for object names.
		// NOTE: to avoid conflicts between parallel tests, resource names are unique to each test
		var (
			workspaceName     string
			workspaceKindName string
		)

		BeforeAll(func() {
			uniqueName := "ws-reconcile-test"
			workspaceName = fmt.Sprintf("workspace-%s", uniqueName)
			workspaceKindName = fmt.Sprintf("workspacekind-%s", uniqueName)
		})

		It("should successfully reconcile the Workspace", func() {

			By("creating a WorkspaceKind")
			workspaceKind := NewExampleWorkspaceKind1(workspaceKindName)
			Expect(k8sClient.Create(ctx, workspaceKind)).To(Succeed())

			By("creating a Workspace")
			workspace := NewExampleWorkspace1(workspaceName, namespaceName, workspaceKindName)
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())

			By("pausing the Workspace")
			patch := client.MergeFrom(workspace.DeepCopy())
			newWorkspace := workspace.DeepCopy()
			newWorkspace.Spec.Paused = new(true)
			Expect(k8sClient.Patch(ctx, newWorkspace, patch)).To(Succeed())

			By("setting the Workspace `status.pauseTime` to the current time")
			currentTime := time.Now().UnixMilli()
			Eventually(func() (int64, error) {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: workspaceName, Namespace: namespaceName}, workspace)
				if err != nil {
					return 0, err
				}
				return workspace.Status.PauseTime, nil
			}, timeout, interval).Should(BeNumerically("~", currentTime, tolerance))

			By("un-pausing the Workspace")
			patch = client.MergeFrom(workspace.DeepCopy())
			newWorkspace = workspace.DeepCopy()
			newWorkspace.Spec.Paused = new(false)
			Expect(k8sClient.Patch(ctx, newWorkspace, patch)).To(Succeed())

			By("setting the Workspace `status.pauseTime` to 0")
			Eventually(func() (int64, error) {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: workspaceName, Namespace: namespaceName}, workspace)
				if err != nil {
					return 0, err
				}
				return workspace.Status.PauseTime, nil
			}, timeout, interval).Should(BeZero())

			By("creating a StatefulSet")
			statefulSetList := &appsv1.StatefulSetList{}
			Eventually(func() ([]appsv1.StatefulSet, error) {
				err := k8sClient.List(ctx, statefulSetList, client.InNamespace(namespaceName), client.MatchingLabels{workspaceNameLabel: workspaceName})
				if err != nil {
					return nil, err
				}
				return statefulSetList.Items, nil
			}, timeout, interval).Should(HaveLen(1))

			statefulSet := statefulSetList.Items[0]

			By("running the Workspace Pods as the hardcoded ServiceAccount")
			Expect(statefulSet.Spec.Template.Spec.ServiceAccountName).To(Equal(workspaceServiceAccountName))

			By("creating a Service")
			serviceList := &corev1.ServiceList{}
			Eventually(func() ([]corev1.Service, error) {
				err := k8sClient.List(ctx, serviceList, client.InNamespace(namespaceName), client.MatchingLabels{workspaceNameLabel: workspaceName})
				if err != nil {
					return nil, err
				}
				return serviceList.Items, nil
			}, timeout, interval).Should(HaveLen(1))

			// TODO: use this to get the Service
			// service := serviceList.Items[0]

			//
			// TODO: populate these tests
			//  - use the CronJob controller tests as a reference
			//    https://github.com/kubernetes-sigs/kubebuilder/blob/master/docs/book/src/cronjob-tutorial/testdata/project/internal/controller/cronjob_controller_test.go
			//  - notes:
			//     - it may make sense to split some of these up into at least separate `It(` specs
			//       or even separate `Context(` scopes so we can run them in parallel
			//  - key things to test:
			//     - core behavior:
			//         - resources like Service/StatefulSet/VirtualService/etc are created when the Workspace is created
			//         - even if the Workspace has a >64 character name, everything still works
			//         - deleting the reconciled resources, and ensuring they are recreated
			//         - updating the reconciled resources, and ensuring they are reverted
			//         - the go templates in WorkspaceKind `spec.podTemplate.extraEnv[].value` should work properly
			//            - succeed for valid portID
			//            - return empty string for invalid portID
			//            - set Workspace to error state for invalid template format (e.g. single quote for portID string)
			//     - workspace update behavior:
			//        - pausing the Workspace results in the StatefulSet being scaled to 0
			//        - updating the selected options results in the correct resources being updated:
			//            - imageConfig - updates the StatefulSet and possibly the Service
			//            - podConfig - updates the StatefulSet
			//     - workspaceKind redirect behavior:
			//        - NO resource changes are made except setting `status.pendingRestart`
			//          and `status.podTemplateOptions` (`desired` along with `redirectChain`)
			//     - error states:
			//        - referencing a missing WorkspaceKind results in error state
			//        - invalid WorkspaceKind (with bad option redirect - circular / missing) results in error state
			//        - multiple owned StatefulSets / Services results in error state
			//
		})
	})

	Context("When culling pauses an inactive Workspace", Serial, Ordered, func() {
		var (
			workspaceName     string
			workspaceKindName string
			reconciler        *WorkspaceReconciler
		)

		BeforeAll(func() {
			uniqueName := fmt.Sprintf("ws-culling-patch-%d", time.Now().UnixNano())
			workspaceName = fmt.Sprintf("workspace-%s", uniqueName)
			workspaceKindName = fmt.Sprintf("workspacekind-%s", uniqueName)

			reconciler = &WorkspaceReconciler{
				Client: k8sManager.GetClient(),
				Scheme: k8sManager.GetScheme(),
				Config: &config.EnvConfig{ClusterDomain: "cluster.local"},
			}

			By("creating a WorkspaceKind with an activity probe and culling rules")
			workspaceKind := NewExampleWorkspaceKind1(workspaceKindName)
			workspaceKind.Spec.PodTemplate.ActivityProbe = &kubefloworgv1beta1.ActivityProbe{
				MinProbeIntervalSeconds: new(int32(1)),
				ProbeIntervalSeconds:    new(int32(10)),
				PodExec: &kubefloworgv1beta1.ActivityProbePodExec{
					TimeoutSeconds: new(int32(30)),
					Script:         "exit 0",
				},
			}
			workspaceKind.Spec.ActivityRules = []kubefloworgv1beta1.ActivityRule{
				{
					Config: kubefloworgv1beta1.ActivityRuleConfig{
						SecondsSinceActive: 16,
						MinRunningSeconds:  new(int32(0)),
					},
					Match:  &kubefloworgv1beta1.ActivityRuleMatch{},
					Effect: kubefloworgv1beta1.ActivityRuleEffect{PauseWorkspace: new(true)},
				},
			}
			Expect(k8sClient.Create(ctx, workspaceKind)).To(Succeed())

			By("creating a Workspace")
			workspace := NewExampleWorkspace1(workspaceName, namespaceName, workspaceKindName)
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
		})

		AfterAll(func() {
			By("deleting the Pod")
			podList := &corev1.PodList{}
			if err := k8sClient.List(ctx, podList, client.InNamespace(namespaceName), client.MatchingLabels{workspaceNameLabel: workspaceName}); err == nil {
				for _, p := range podList.Items {
					_ = k8sClient.Delete(ctx, &p)
				}
			}

			By("deleting the StatefulSet")
			stsList := &appsv1.StatefulSetList{}
			if err := k8sClient.List(ctx, stsList, client.InNamespace(namespaceName), client.MatchingLabels{workspaceNameLabel: workspaceName}); err == nil {
				for _, s := range stsList.Items {
					_ = k8sClient.Delete(ctx, &s)
				}
			}

			By("deleting the Service")
			svcList := &corev1.ServiceList{}
			if err := k8sClient.List(ctx, svcList, client.InNamespace(namespaceName), client.MatchingLabels{workspaceNameLabel: workspaceName}); err == nil {
				for _, s := range svcList.Items {
					_ = k8sClient.Delete(ctx, &s)
				}
			}

			By("deleting the Workspace")
			workspace := &kubefloworgv1beta1.Workspace{
				ObjectMeta: metav1.ObjectMeta{Name: workspaceName, Namespace: namespaceName},
			}
			_ = k8sClient.Delete(ctx, workspace)

			By("deleting the WorkspaceKind")
			workspaceKind := &kubefloworgv1beta1.WorkspaceKind{
				ObjectMeta: metav1.ObjectMeta{Name: workspaceKindName},
			}
			_ = k8sClient.Delete(ctx, workspaceKind)
		})

		It("should successfully persist spec.paused=true to the API server even when Status().Update runs in the same reconcile", func() {
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      workspaceName,
					Namespace: namespaceName,
				},
			}

			By("reconciling until the StatefulSet is created")
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() error {
				statefulSetList := &appsv1.StatefulSetList{}
				if err := k8sClient.List(ctx, statefulSetList, client.InNamespace(namespaceName), client.MatchingLabels{workspaceNameLabel: workspaceName}); err != nil {
					return err
				}
				if len(statefulSetList.Items) != 1 {
					return fmt.Errorf("expected 1 StatefulSet, got %d", len(statefulSetList.Items))
				}
				return nil
			}, timeout, interval).Should(Succeed())

			By("fetching the created StatefulSet and creating a running Pod for it")
			statefulSetList := &appsv1.StatefulSetList{}
			Expect(k8sClient.List(ctx, statefulSetList, client.InNamespace(namespaceName), client.MatchingLabels{workspaceNameLabel: workspaceName})).To(Succeed())
			statefulSetName := statefulSetList.Items[0].Name
			podName := fmt.Sprintf("%s-0", statefulSetName)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: namespaceName,
					Labels: map[string]string{
						workspaceNameLabel: workspaceName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			pod.Status.Phase = corev1.PodRunning
			pod.Status.PodIP = "10.0.0.1"
			pod.Status.Conditions = []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			By("mocking pod execution to report stale activity (inactive)")
			reconciler.PodExecutor = &fakePodExecutor{
				stdout: `{"has_activity": false, "last_activity": "2000-01-01T00:00:00Z"}`,
			}

			By("reconciling until spec.paused=true is persisted to the API server")
			Eventually(func(g Gomega) {
				updatedWS := &kubefloworgv1beta1.Workspace{}
				if err := k8sClient.Get(ctx, req.NamespacedName, updatedWS); err == nil && ptr.Deref(updatedWS.Spec.Paused, false) {
					return
				}

				_, err := reconciler.Reconcile(ctx, req)
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(k8sClient.Get(ctx, req.NamespacedName, updatedWS)).To(Succeed())
				g.Expect(ptr.Deref(updatedWS.Spec.Paused, false)).To(BeTrue(), "spec.paused must be persisted to true in the API server")
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("When generating a VirtualService for a Workspace", func() {

		// Define utility variables for object names.
		// NOTE: to avoid conflicts between parallel tests, resource names are unique to each test
		var (
			workspaceName     string
			workspaceKindName string
		)

		// NOTE: these tests call the generate functions directly and do not create any
		//       resources in the cluster, so no teardown is required.
		// TODO: once Istio CRDs are installed in EnvTest (`UseIstio: true` in suite_test.go),
		//       add specs which ensure the VirtualService is actually created by the controller.
		var (
			reconciler      *WorkspaceReconciler
			workspace       *kubefloworgv1beta1.Workspace
			workspaceKind   *kubefloworgv1beta1.WorkspaceKind
			service         *corev1.Service
			imageConfigSpec kubefloworgv1beta1.ImageConfigSpec
		)

		BeforeEach(func() {
			uniqueName := "ws-virtualservice-test"
			workspaceName = fmt.Sprintf("workspace-%s", uniqueName)
			workspaceKindName = fmt.Sprintf("workspacekind-%s", uniqueName)

			reconciler = &WorkspaceReconciler{
				Config: &config.EnvConfig{
					ClusterDomain: "cluster.local",
				},
			}
			workspaceKind = NewExampleWorkspaceKind1(workspaceKindName)
			workspace = NewExampleWorkspace1(workspaceName, namespaceName, workspaceKindName)
			service = &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("ws-%s", workspaceName),
					Namespace: namespaceName,
				},
			}
			imageConfigSpec = workspaceKind.Spec.PodTemplate.Options.ImageConfig.Values[0].Spec
		})

		It("should not rewrite the URI when `removePathPrefix` is false", func() {
			By("generating the VirtualService")
			workspaceKind.Spec.PodTemplate.Ports[0].HTTPProxy.RemovePathPrefix = new(false)
			virtualService, err := reconciler.generateVirtualService(workspace, workspaceKind, service, imageConfigSpec)
			Expect(err).NotTo(HaveOccurred())

			By("checking the HTTP route has no rewrite")
			Expect(virtualService.Spec.Http).To(HaveLen(1))
			Expect(virtualService.Spec.Http[0].Rewrite).To(BeNil())
		})

		It("should rewrite the URI to '/' when `removePathPrefix` is true", func() {
			By("generating the VirtualService")
			workspaceKind.Spec.PodTemplate.Ports[0].HTTPProxy.RemovePathPrefix = new(true)
			virtualService, err := reconciler.generateVirtualService(workspace, workspaceKind, service, imageConfigSpec)
			Expect(err).NotTo(HaveOccurred())

			By("checking the HTTP route rewrites the URI to '/'")
			Expect(virtualService.Spec.Http).To(HaveLen(1))
			Expect(virtualService.Spec.Http[0].Rewrite).NotTo(BeNil())
			Expect(virtualService.Spec.Http[0].Rewrite.Uri).To(Equal("/"))
		})

		It("should not rewrite the URI when `httpProxy` is not set", func() {
			By("generating the VirtualService")
			workspaceKind.Spec.PodTemplate.Ports[0].HTTPProxy = nil
			virtualService, err := reconciler.generateVirtualService(workspace, workspaceKind, service, imageConfigSpec)
			Expect(err).NotTo(HaveOccurred())

			By("checking the HTTP route has no rewrite")
			Expect(virtualService.Spec.Http).To(HaveLen(1))
			Expect(virtualService.Spec.Http[0].Rewrite).To(BeNil())
		})

		It("should render go templates in `requestHeaders` values", func() {
			By("generating the VirtualService")
			workspaceKind.Spec.PodTemplate.Ports[0].HTTPProxy.RequestHeaders = &kubefloworgv1beta1.IstioHeaderOperations{
				Set: map[string]string{
					"X-RStudio-Root-Path": `{{ httpPathPrefix "jupyterlab" }}`,
				},
			}
			virtualService, err := reconciler.generateVirtualService(workspace, workspaceKind, service, imageConfigSpec)
			Expect(err).NotTo(HaveOccurred())

			By("checking the rendered header value")
			Expect(virtualService.Spec.Http).To(HaveLen(1))
			Expect(virtualService.Spec.Http[0].Headers.Request.Set).To(HaveKeyWithValue(
				"X-RStudio-Root-Path", getWorkspaceConnectPath(workspace.Namespace, workspace.Name, "jupyterlab"),
			))
		})

		It("should fail to generate when a `requestHeaders` value has an invalid go template", func() {
			By("generating the VirtualService")
			workspaceKind.Spec.PodTemplate.Ports[0].HTTPProxy.RequestHeaders = &kubefloworgv1beta1.IstioHeaderOperations{
				Set: map[string]string{
					"X-RStudio-Root-Path": `{{ httpPathPrefix 'jupyterlab' }}`,
				},
			}
			_, err := reconciler.generateVirtualService(workspace, workspaceKind, service, imageConfigSpec)
			Expect(err).To(HaveOccurred())
		})
	})
})
