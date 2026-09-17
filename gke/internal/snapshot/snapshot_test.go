package snapshot

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

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
	var coreObjs []runtime.Object
	var dynObjs []runtime.Object
	for _, obj := range objects {
		if _, ok := obj.(*unstructured.Unstructured); ok {
			dynObjs = append(dynObjs, obj)
		} else {
			coreObjs = append(coreObjs, obj)
		}
	}
	coreClient := k8sfake.NewSimpleClientset(coreObjs...)
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, dynObjs...)
	ctrl := &Controller{
		core:           coreClient.CoreV1(),
		dynamic:        dynClient,
		tenants:        []string{"kubeflow-user"},
		snapshotBucket: "test-bucket",
		triggerCh:      make(chan struct{}, 1),
	}
	return ctrl, coreClient, dynClient
}

func TestMutateWorkspaceInterceptsPauseAndFlipsReadinessGate(t *testing.T) {
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-ws-test-x2wbv-0",
			Namespace: "kubeflow-user",
			Labels: map[string]string{
				WorkspaceLabel: "ws-test",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: ReadinessGateConditionType, Status: corev1.ConditionTrue},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true},
			},
		},
	}
	ctrl, coreClient, _ := newTestController(t, runningPod)

	oldRaw, _ := json.Marshal(map[string]any{
		"apiVersion": "kubeflow.org/v1beta1",
		"kind":       "Workspace",
		"metadata": map[string]any{
			"name":      "ws-test",
			"namespace": "kubeflow-user",
			"annotations": map[string]any{
				AnnotationEnabled: "true",
			},
		},
		"spec": map[string]any{
			"paused": false,
			"kind":   "jupyterlab",
		},
	})
	newRaw, _ := json.Marshal(map[string]any{
		"apiVersion": "kubeflow.org/v1beta1",
		"kind":       "Workspace",
		"metadata": map[string]any{
			"name":      "ws-test",
			"namespace": "kubeflow-user",
			"annotations": map[string]any{
				AnnotationEnabled: "true",
			},
		},
		"spec": map[string]any{
			"paused": true,
			"kind":   "jupyterlab",
		},
	})

	resp := ctrl.mutateWorkspace(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		OldObject: runtime.RawExtension{Raw: oldRaw},
		Object:    runtime.RawExtension{Raw: newRaw},
	})
	if !resp.Allowed || len(resp.Patch) == 0 {
		t.Fatalf("expected non-empty patch intercepting pause, got allowed=%v patch=%s", resp.Allowed, string(resp.Patch))
	}

	var ops []jsonPatchOp
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatalf("failed to decode patch: %v", err)
	}
	foundPausedFalse := false
	for _, op := range ops {
		if op.Path == "/spec/paused" && op.Value == false {
			foundPausedFalse = true
		}
	}
	if !foundPausedFalse {
		t.Fatalf("expected patch to hold /spec/paused=false, got ops=%+v", ops)
	}

	updatedPod, err := coreClient.CoreV1().Pods("kubeflow-user").Get(context.Background(), "ws-ws-test-x2wbv-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get pod: %v", err)
	}
	if isPodReady(updatedPod) {
		t.Fatalf("expected PodReady to be flipped to False during Checkpointing")
	}
}

func TestMutatePodInjectsGVisorIPCAndRestoreAnnotation(t *testing.T) {
	ws := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubeflow.org/v1beta1",
			"kind":       "Workspace",
			"metadata": map[string]any{
				"name":      "ws-test",
				"namespace": "kubeflow-user",
				"annotations": map[string]any{
					AnnotationEnabled:            "true",
					AnnotationLastCheckpointName: "snap-12345",
				},
			},
			"spec": map[string]any{
				"paused": false,
				"kind":   "jupyterlab",
			},
		},
	}
	ctrl, _, _ := newTestController(t, ws)

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-test-0",
			Namespace: "kubeflow-user",
			Labels: map[string]string{
				WorkspaceLabel: "ws-test",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "jupyter:latest"},
			},
		},
	}
	podRaw, _ := json.Marshal(pod)
	resp := ctrl.mutatePod(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: "kubeflow-user",
		Object:    runtime.RawExtension{Raw: podRaw},
	})
	if !resp.Allowed || len(resp.Patch) == 0 {
		t.Fatalf("expected pod mutation patch, got allowed=%v patch=%s", resp.Allowed, string(resp.Patch))
	}

	var ops []jsonPatchOp
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatalf("failed to decode patch: %v", err)
	}
	paths := map[string]bool{}
	for _, op := range ops {
		paths[op.Path] = true
	}
	for _, expected := range []string{"/spec/runtimeClassName", "/spec/readinessGates", "/spec/volumes", "/spec/containers/0/volumeMounts", "/metadata/annotations"} {
		if !paths[expected] {
			t.Errorf("expected patch path %s, got paths=%v", expected, paths)
		}
	}
}
