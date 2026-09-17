package snapshot

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const testNamespace = "kubeflow-user"

func newTestController(t *testing.T, objects ...runtime.Object) (*Controller, *k8sfake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	gvrToListKind := map[schema.GroupVersionResource]string{
		workspaceGVR:                "WorkspaceList",
		workspaceKindGVR:            "WorkspaceKindList",
		podSnapshotStorageConfigGVR: "PodSnapshotStorageConfigList",
		podSnapshotPolicyGVR:        "PodSnapshotPolicyList",
		podSnapshotManualTriggerGVR: "PodSnapshotManualTriggerList",
		podSnapshotGVR:              "PodSnapshotList",
	}
	var coreObjects []runtime.Object
	var dynamicObjects []runtime.Object
	for _, object := range objects {
		if _, ok := object.(*unstructured.Unstructured); ok {
			dynamicObjects = append(dynamicObjects, object)
		} else {
			coreObjects = append(coreObjects, object)
		}
	}
	coreClient := k8sfake.NewSimpleClientset(coreObjects...)
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, dynamicObjects...)
	controller := &Controller{
		core:       coreClient.CoreV1(),
		dynamic:    dynamicClient,
		namespaces: map[string]bool{testNamespace: true},
		bucket:     "test-bucket",
		settle:     SocketSettleGracePeriod,
		resync:     defaultResyncPeriod,
		workers:    1,
		podIndexer: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{podWorkspaceIndex: podWorkspaceIndexFunc}),
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[types.NamespacedName]()),
	}
	return controller, coreClient, dynamicClient
}

func workspace(name string, annotations map[string]any, paused bool) *unstructured.Unstructured {
	if annotations == nil {
		annotations = map[string]any{}
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubeflow.org/v1beta1",
			"kind":       "Workspace",
			"metadata": map[string]any{
				"name":        name,
				"namespace":   testNamespace,
				"uid":         "uid-" + name,
				"annotations": annotations,
			},
			"spec": map[string]any{"paused": paused, "kind": "jupyterlab"},
		},
	}
}

func runningPod(name, workspaceName string, gate, ready corev1.ConditionStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels:    map[string]string{WorkspaceLabel: workspaceName},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: ReadinessGateConditionType, Status: gate},
				{Type: corev1.PodReady, Status: ready},
			},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "main", Ready: true}},
		},
	}
}

func TestMutateWorkspaceInterceptsPauseAndFlipsReadinessGate(t *testing.T) {
	pod := runningPod("ws-ws-test-x2wbv-0", "ws-test", corev1.ConditionTrue, corev1.ConditionTrue)
	controller, coreClient, _ := newTestController(t, pod)

	enabled := map[string]any{AnnotationEnabled: "true"}
	oldRaw, _ := json.Marshal(workspace("ws-test", enabled, false).Object)
	newRaw, _ := json.Marshal(workspace("ws-test", enabled, true).Object)

	response := controller.mutateWorkspace(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		OldObject: runtime.RawExtension{Raw: oldRaw},
		Object:    runtime.RawExtension{Raw: newRaw},
	})
	if !response.Allowed || len(response.Patch) == 0 {
		t.Fatalf("expected a patch intercepting the pause, got allowed=%v patch=%s", response.Allowed, response.Patch)
	}

	var operations []jsonPatchOp
	if err := json.Unmarshal(response.Patch, &operations); err != nil {
		t.Fatalf("failed to decode patch: %v", err)
	}
	heldPaused, markedCheckpointing := false, false
	for _, operation := range operations {
		if operation.Path == "/spec/paused" && operation.Value == false {
			heldPaused = true
		}
		if operation.Path == "/metadata/annotations" {
			annotations, _ := operation.Value.(map[string]any)
			if annotations[AnnotationCheckpointState] == CheckpointStateCheckpointing && annotations[AnnotationCheckpointStartedAt] != "" {
				markedCheckpointing = true
			}
		}
	}
	if !heldPaused || !markedCheckpointing {
		t.Fatalf("expected the patch to hold spec.paused=false and mark Checkpointing, got %+v", operations)
	}

	updated, err := coreClient.CoreV1().Pods(testNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get pod: %v", err)
	}
	if isPodReady(updated) {
		t.Fatal("expected PodReady to be flipped to False while checkpointing")
	}
}

// A Workspace update that does not toggle spec.paused must not cost any API calls:
// this is what keeps the webhook cheap under the constant status updates the
// upstream controller performs.
func TestMutateWorkspaceIgnoresNonTransitionUpdates(t *testing.T) {
	controller, coreClient, dynamicClient := newTestController(t)
	enabled := map[string]any{AnnotationEnabled: "true"}
	oldRaw, _ := json.Marshal(workspace("ws-test", enabled, false).Object)
	updated := workspace("ws-test", enabled, false)
	_ = unstructured.SetNestedField(updated.Object, "Running", "status", "state")
	newRaw, _ := json.Marshal(updated.Object)

	response := controller.mutateWorkspace(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		OldObject: runtime.RawExtension{Raw: oldRaw},
		Object:    runtime.RawExtension{Raw: newRaw},
	})
	if !response.Allowed || len(response.Patch) != 0 {
		t.Fatalf("expected an unmodified allow, got patch=%s", response.Patch)
	}
	if calls := len(coreClient.Actions()) + len(dynamicClient.Actions()); calls != 0 {
		t.Fatalf("expected no API calls for a non-transition update, got %d", calls)
	}
}

func TestMutatePodInjectsGVisorIPCAndRestoreAnnotation(t *testing.T) {
	controller, _, _ := newTestController(t, workspace("ws-test", map[string]any{
		AnnotationEnabled:            "true",
		AnnotationLastCheckpointName: "snap-12345",
	}, false))

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-test-0",
			Namespace: testNamespace,
			Labels:    map[string]string{WorkspaceLabel: "ws-test"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "jupyter:latest"}}},
	}
	raw, _ := json.Marshal(pod)
	response := controller.mutatePod(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: testNamespace,
		Object:    runtime.RawExtension{Raw: raw},
	})
	if !response.Allowed || len(response.Patch) == 0 {
		t.Fatalf("expected a pod mutation patch, got allowed=%v patch=%s", response.Allowed, response.Patch)
	}

	var operations []jsonPatchOp
	if err := json.Unmarshal(response.Patch, &operations); err != nil {
		t.Fatalf("failed to decode patch: %v", err)
	}
	paths := map[string]bool{}
	for _, operation := range operations {
		paths[operation.Path] = true
	}
	for _, expected := range []string{"/spec/runtimeClassName", "/spec/readinessGates", "/spec/volumes", "/spec/containers/0/volumeMounts", "/metadata/annotations"} {
		if !paths[expected] {
			t.Errorf("expected patch path %s, got %v", expected, paths)
		}
	}
}

func TestMutatePodSkipsTPUWorkspaces(t *testing.T) {
	controller, _, dynamicClient := newTestController(t, workspace("ws-tpu", map[string]any{AnnotationEnabled: "true"}, false))
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-tpu-0",
			Namespace: testNamespace,
			Labels:    map[string]string{WorkspaceLabel: "ws-tpu"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "main",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{"google.com/tpu": resource.MustParse("4")},
			},
		}}},
	}
	raw, _ := json.Marshal(pod)
	response := controller.mutatePod(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: testNamespace,
		Object:    runtime.RawExtension{Raw: raw},
	})
	if len(response.Patch) != 0 {
		t.Fatalf("TPU pods must not be mutated, got %s", response.Patch)
	}
	if len(dynamicClient.Actions()) != 0 {
		t.Fatalf("TPU pods must be rejected before any API lookup, got %v", dynamicClient.Actions())
	}
}

// While the socket settle grace period is running the reconciler must ask for a
// delayed retry instead of creating the trigger, and it must not busy-poll.
func TestReconcileWaitsForSocketSettleGracePeriod(t *testing.T) {
	startedAt := time.Now().UTC().Format(time.RFC3339)
	workspaceObject := workspace("ws-test", map[string]any{
		AnnotationEnabled:             "true",
		AnnotationCheckpointState:     CheckpointStateCheckpointing,
		AnnotationCheckpointStartedAt: startedAt,
	}, false)
	pod := runningPod("ws-test-0", "ws-test", corev1.ConditionFalse, corev1.ConditionFalse)
	controller, _, dynamicClient := newTestController(t, workspaceObject, pod)

	requeueAfter, err := controller.reconcileWorkspace(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "ws-test"})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if requeueAfter <= 0 || requeueAfter > SocketSettleGracePeriod {
		t.Fatalf("expected a requeue within the settle grace period, got %s", requeueAfter)
	}
	for _, action := range dynamicClient.Actions() {
		if action.GetVerb() == "create" && action.GetResource() == podSnapshotManualTriggerGVR {
			t.Fatal("a checkpoint was requested before the sockets settled")
		}
	}
}

// Once the grace period has elapsed the trigger is created exactly once.
func TestReconcileCreatesManualTriggerAfterGracePeriod(t *testing.T) {
	startedAt := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	workspaceObject := workspace("ws-test", map[string]any{
		AnnotationEnabled:             "true",
		AnnotationCheckpointState:     CheckpointStateCheckpointing,
		AnnotationCheckpointStartedAt: startedAt,
	}, false)
	pod := runningPod("ws-test-0", "ws-test", corev1.ConditionFalse, corev1.ConditionFalse)
	controller, _, dynamicClient := newTestController(t, workspaceObject, pod)

	if _, err := controller.reconcileWorkspace(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "ws-test"}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	trigger, err := dynamicClient.Resource(podSnapshotManualTriggerGVR).Namespace(testNamespace).
		Get(context.Background(), triggerNameFor("ws-test"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected a PodSnapshotManualTrigger: %v", err)
	}
	if target, _, _ := unstructured.NestedString(trigger.Object, "spec", "targetPod"); target != pod.Name {
		t.Fatalf("trigger targets %q, want %q", target, pod.Name)
	}
	if owner := workspaceOwner(trigger); owner != "ws-test" {
		t.Fatalf("trigger must be owned by its Workspace for garbage collection, got %q", owner)
	}

	// A second reconcile must be idempotent.
	before := len(dynamicClient.Actions())
	if _, err := controller.reconcileWorkspace(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "ws-test"}); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	for _, action := range dynamicClient.Actions()[before:] {
		if action.GetVerb() == "create" || action.GetVerb() == "delete" {
			t.Fatalf("reconcile is not idempotent, it issued a %s on %s", action.GetVerb(), action.GetResource())
		}
	}
}

// A Ready PodSnapshot completes the pause by letting spec.paused=true through.
func TestReconcileCompletesPauseWhenSnapshotIsReady(t *testing.T) {
	startedAt := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	workspaceObject := workspace("ws-test", map[string]any{
		AnnotationEnabled:             "true",
		AnnotationCheckpointState:     CheckpointStateCheckpointing,
		AnnotationCheckpointStartedAt: startedAt,
	}, false)
	pod := runningPod("ws-test-0", "ws-test", corev1.ConditionFalse, corev1.ConditionFalse)
	trigger := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": podsnapshotGroupVersion,
		"kind":       "PodSnapshotManualTrigger",
		"metadata": map[string]any{
			"name":              triggerNameFor("ws-test"),
			"namespace":         testNamespace,
			"annotations":       map[string]any{AnnotationCheckpointStartedAt: startedAt},
			"creationTimestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"status": map[string]any{"snapshotCreated": map[string]any{"name": "snap-1"}},
	}}
	snapshot := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": podsnapshotGroupVersion,
		"kind":       "PodSnapshot",
		"metadata":   map[string]any{"name": "snap-1", "namespace": testNamespace},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "True"},
		}},
	}}
	controller, _, dynamicClient := newTestController(t, workspaceObject, pod, trigger, snapshot)

	if _, err := controller.reconcileWorkspace(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "ws-test"}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	patched := findPatch(dynamicClient.Actions(), workspaceGVR)
	if patched == nil {
		t.Fatal("expected the Workspace to be patched once the snapshot was Ready")
	}
	if paused, _, _ := unstructured.NestedBool(patched, "spec", "paused"); !paused {
		t.Fatalf("expected spec.paused=true, got %v", patched)
	}
	annotations, _, _ := unstructured.NestedMap(patched, "metadata", "annotations")
	if annotations[AnnotationLastCheckpointName] != "snap-1" || annotations[AnnotationCheckpointState] != CheckpointStateReady {
		t.Fatalf("unexpected annotations %v", annotations)
	}
	assertNoPodSnapshotMutations(t, dynamicClient.Actions())
}

// GKE installs a ValidatingAdmissionPolicy that rejects every create/update/patch of
// a PodSnapshot from any principal other than its own snapshot controller. An earlier
// revision tried to attach a Workspace ownerReference to the snapshot and wedged the
// pause in "Checkpointing" forever, retrying against a permanent 403. Nothing in the
// reconcile path may write to a PodSnapshot; deleting one is the only allowed verb.
func assertNoPodSnapshotMutations(t *testing.T, actions []clienttesting.Action) {
	t.Helper()
	for _, action := range actions {
		if action.GetResource() != podSnapshotGVR {
			continue
		}
		switch action.GetVerb() {
		case "get", "list", "watch", "delete", "delete-collection":
		default:
			t.Fatalf("reconcile issued a %q on a PodSnapshot; GKE's ValidatingAdmissionPolicy always rejects this", action.GetVerb())
		}
	}
}

// A deleted Workspace must take its snapshots with it. They carry no ownerReference
// (see above), so the Kubernetes garbage collector cannot do it for us.
//
// Ordering matters: GKE resolves a PodSnapshot's bucket through its policy, so the
// policy has to survive until every snapshot has finished finalizing. Retiring the
// policy first makes GKE drop the snapshot object and orphan its files in GCS.
func TestReconcileDeletesSnapshotsWhenWorkspaceIsGone(t *testing.T) {
	newSnapshot := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshot",
			"metadata": map[string]any{
				"name":      "snap-1",
				"namespace": testNamespace,
				"labels":    map[string]any{LabelSnapshotTriggeredBy: triggerNameFor("ws-test")},
			},
		}}
	}
	policy := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshotPolicy",
			"metadata":   map[string]any{"name": policyNameFor("ws-test"), "namespace": testNamespace},
		}}
	}
	key := types.NamespacedName{Namespace: testNamespace, Name: "ws-test"}

	t.Run("snapshot still finalizing keeps the policy", func(t *testing.T) {
		// The fake client honours the label selector but does not run finalizers, so
		// the snapshot stays listed — exactly like a terminating one on a real cluster.
		controller, _, dynamicClient := newTestController(t, newSnapshot(), policy())

		requeue, err := controller.reconcileWorkspace(context.Background(), key)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		if requeue != snapshotDrainInterval {
			t.Fatalf("requeue = %v, want %v so we come back once GKE has finalized", requeue, snapshotDrainInterval)
		}
		assertDeleted(t, dynamicClient.Actions(), podSnapshotGVR, true)
		assertDeleted(t, dynamicClient.Actions(), podSnapshotPolicyGVR, false)
	})

	t.Run("snapshots drained retires the policy", func(t *testing.T) {
		controller, _, dynamicClient := newTestController(t, policy())

		requeue, err := controller.reconcileWorkspace(context.Background(), key)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		if requeue != 0 {
			t.Fatalf("requeue = %v, want 0 once nothing is left to drain", requeue)
		}
		assertDeleted(t, dynamicClient.Actions(), podSnapshotPolicyGVR, true)
	})
	t.Run("wedged finalizer eventually releases the policy", func(t *testing.T) {
		// GKE removes the GCS objects within seconds and only then drops its
		// finalizer, so a snapshot still terminating this long after its deletion is
		// wedged. Waiting forever would pin the policy forever.
		stuck := newSnapshot()
		deletedAt := metav1.NewTime(time.Now().Add(-2 * snapshotDrainTimeout))
		stuck.SetDeletionTimestamp(&deletedAt)
		stuck.SetFinalizers([]string{"podsnapshot.gke.io/podsnapshot-finalizer"})
		controller, _, dynamicClient := newTestController(t, stuck, policy())

		requeue, err := controller.reconcileWorkspace(context.Background(), key)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		if requeue != 0 {
			t.Fatalf("requeue = %v, want 0 once the drain deadline has passed", requeue)
		}
		assertDeleted(t, dynamicClient.Actions(), podSnapshotPolicyGVR, true)
	})
}

func assertDeleted(t *testing.T, actions []clienttesting.Action, gvr schema.GroupVersionResource, want bool) {
	t.Helper()
	got := false
	for _, action := range actions {
		if action.GetResource() != gvr {
			continue
		}
		if verb := action.GetVerb(); verb == "delete" || verb == "delete-collection" {
			got = true
		}
	}
	if got != want {
		t.Fatalf("deleted %s = %v, want %v", gvr.Resource, got, want)
	}
}

// The steady state (a healthy, running Workspace) must not write anything.
func TestReconcileActiveWorkspaceIsWriteFree(t *testing.T) {
	workspaceObject := workspace("ws-test", map[string]any{AnnotationEnabled: "true"}, false)
	pod := runningPod("ws-test-0", "ws-test", corev1.ConditionTrue, corev1.ConditionTrue)
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: IPCConfigMapName, Namespace: testNamespace},
		Data:       map[string]string{IPCConfigMapKey: jupyterIPCServerConfig},
	}
	storageConfig := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": podsnapshotGroupVersion,
		"kind":       "PodSnapshotStorageConfig",
		"metadata":   map[string]any{"name": DefaultStorageConfigName},
	}}
	policy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": podsnapshotGroupVersion,
		"kind":       "PodSnapshotPolicy",
		"metadata":   map[string]any{"name": policyNameFor("ws-test"), "namespace": testNamespace},
	}}
	controller, coreClient, dynamicClient := newTestController(t, workspaceObject, pod, configMap, storageConfig, policy)

	if _, err := controller.reconcileWorkspace(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "ws-test"}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	for _, action := range append(toActions(coreClient.Actions()), toActions(dynamicClient.Actions())...) {
		switch action.GetVerb() {
		case "get", "list", "watch":
		default:
			t.Fatalf("steady state reconcile issued a %s on %s", action.GetVerb(), action.GetResource())
		}
	}
}

// Workspaces outside the managed tenant namespaces never enter the queue.
func TestEnqueueIgnoresUnmanagedNamespaces(t *testing.T) {
	controller, _, _ := newTestController(t)
	controller.enqueueWorkspace("other-namespace", "ws-test")
	if controller.queue.Len() != 0 {
		t.Fatal("a Workspace outside the managed namespaces was enqueued")
	}
	controller.enqueueWorkspace(testNamespace, "ws-test")
	if controller.queue.Len() != 1 {
		t.Fatal("a managed Workspace was not enqueued")
	}
}

// Pod events must map back to their Workspace through the shared index.
func TestPodIndexResolvesWorkspacePod(t *testing.T) {
	controller, _, _ := newTestController(t)
	controller.cachesReady.Store(true)
	pod := runningPod("ws-test-0", "ws-test", corev1.ConditionTrue, corev1.ConditionTrue)
	if err := controller.podIndexer.Add(pod); err != nil {
		t.Fatal(err)
	}
	found := controller.findWorkspacePod(context.Background(), testNamespace, "ws-test")
	if found == nil || found.Name != pod.Name {
		t.Fatalf("expected the indexed Pod, got %v", found)
	}
	if controller.findWorkspacePod(context.Background(), testNamespace, "other") != nil {
		t.Fatal("the index returned a Pod for the wrong Workspace")
	}
}

func toActions(actions []clienttesting.Action) []clienttesting.Action { return actions }

func findPatch(actions []clienttesting.Action, gvr schema.GroupVersionResource) map[string]any {
	for _, action := range actions {
		patch, ok := action.(clienttesting.PatchAction)
		if !ok || patch.GetResource() != gvr {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal(patch.GetPatch(), &decoded); err != nil {
			continue
		}
		return decoded
	}
	return nil
}
