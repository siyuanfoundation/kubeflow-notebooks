package snapshot

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

const (
	AnnotationEnabled             = "podsnapshot.gke.kubeflow.org/enabled"
	AnnotationStorageConfig       = "podsnapshot.gke.kubeflow.org/storage-config"
	AnnotationCheckpointState     = "podsnapshot.gke.kubeflow.org/checkpoint-state"
	AnnotationCheckpointStartedAt = "podsnapshot.gke.kubeflow.org/checkpoint-started-at"
	AnnotationLastCheckpointName  = "podsnapshot.gke.kubeflow.org/last-checkpoint-name"

	CheckpointStateCheckpointing = "Checkpointing"
	CheckpointStateReady         = "Ready"
	CheckpointStateRestoring     = "Restoring"

	GKERestoreAnnotation       = "podsnapshot.gke.io/ps-name"
	DefaultGKERuntimeClass     = "gvisor"
	ReadinessGateConditionType = corev1.PodConditionType("podsnapshot.gke.kubeflow.org/active")

	DefaultStorageConfigName = "kubeflow-pod-snapshot-storage-config"
	IPCConfigMapName         = "jupyter-ipc-config"
	WorkspaceLabel           = "notebooks.kubeflow.org/workspace-name"

	SocketSettleGracePeriod = 3 * time.Second
)

const jupyterIPCServerConfig = `import asyncio
import selectors
class PollEventLoopPolicy(asyncio.DefaultEventLoopPolicy):
    def _loop_factory(self):
        return asyncio.SelectorEventLoop(selectors.PollSelector())
asyncio.set_event_loop_policy(PollEventLoopPolicy())

c.KernelManager.transport = 'ipc'
`

var (
	workspaceGVR = schema.GroupVersionResource{
		Group:    "kubeflow.org",
		Version:  "v1beta1",
		Resource: "workspaces",
	}
	workspaceKindGVR = schema.GroupVersionResource{
		Group:    "kubeflow.org",
		Version:  "v1beta1",
		Resource: "workspacekinds",
	}
	podSnapshotStorageConfigGVR = schema.GroupVersionResource{
		Group:    "podsnapshot.gke.io",
		Version:  "v1",
		Resource: "podsnapshotstorageconfigs",
	}
	podSnapshotPolicyGVR = schema.GroupVersionResource{
		Group:    "podsnapshot.gke.io",
		Version:  "v1",
		Resource: "podsnapshotpolicies",
	}
	podSnapshotManualTriggerGVR = schema.GroupVersionResource{
		Group:    "podsnapshot.gke.io",
		Version:  "v1",
		Resource: "podsnapshotmanualtriggers",
	}
	podSnapshotGVR = schema.GroupVersionResource{
		Group:    "podsnapshot.gke.io",
		Version:  "v1",
		Resource: "podsnapshots",
	}
)

type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

type Controller struct {
	core            coreclient.CoreV1Interface
	dynamic         dynamic.Interface
	tenants         []string
	snapshotBucket  string
	invalidateCache func(namespace, name string)
	triggerCh       chan struct{}
}

func NewController(config *rest.Config, tenants []string, snapshotBucket string, invalidateCache func(namespace, name string)) (*Controller, error) {
	core, err := coreclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	if snapshotBucket == "" {
		if len(tenants) > 0 && tenants[0] != "" {
			snapshotBucket = fmt.Sprintf("%s-snapshots-bucket", tenants[0])
		} else {
			snapshotBucket = "kubeflow-user-snapshots-bucket"
		}
	}
	return &Controller{
		core:            core,
		dynamic:         dyn,
		tenants:         tenants,
		snapshotBucket:  snapshotBucket,
		invalidateCache: invalidateCache,
		triggerCh:       make(chan struct{}, 1),
	}, nil
}

func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate-workspace", c.HandleWorkspaceMutate)
	mux.HandleFunc("/mutate-pod", c.HandlePodMutate)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func NewTLSServer(addr, certFile, keyFile string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				cert, err := tls.LoadX509KeyPair(certFile, keyFile)
				if err != nil {
					return nil, err
				}
				return &cert, nil
			},
		},
	}
}

func (c *Controller) notifyReconcile() {
	select {
	case c.triggerCh <- struct{}{}:
	default:
	}
}

func (c *Controller) HandleWorkspaceMutate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		http.Error(w, "invalid admission review", http.StatusBadRequest)
		return
	}
	resp := c.mutateWorkspace(r.Context(), review.Request)
	resp.UID = review.Request.UID
	review.Response = resp
	review.Request = nil
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(review)
}

func (c *Controller) getWorkspacePod(ctx context.Context, namespace, wsName string) (*corev1.Pod, error) {
	pods, err := c.core.Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: WorkspaceLabel + "=" + wsName,
	})
	if err != nil {
		return nil, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp == nil {
			return p, nil
		}
	}
	if len(pods.Items) > 0 {
		return &pods.Items[0], nil
	}
	return nil, apierrors.NewNotFound(corev1.Resource("pods"), wsName+"-0")
}

func (c *Controller) mutateWorkspace(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req.Operation != admissionv1.Update || req.SubResource != "" {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	var newWS, oldWS unstructured.Unstructured
	if err := json.Unmarshal(req.Object.Raw, &newWS.Object); err != nil {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	if len(req.OldObject.Raw) > 0 {
		_ = json.Unmarshal(req.OldObject.Raw, &oldWS.Object)
	}
	enabled, _ := c.isSnapshotEnabledForWorkspace(ctx, &newWS)
	if !enabled {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}

	oldPaused, _, _ := unstructured.NestedBool(oldWS.Object, "spec", "paused")
	newPaused, _, _ := unstructured.NestedBool(newWS.Object, "spec", "paused")
	annotations := newWS.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	checkpointState := annotations[AnnotationCheckpointState]
	namespace := newWS.GetNamespace()
	name := newWS.GetName()

	// Case 1: Pause requested (spec.paused transitioning false -> true)
	if !oldPaused && newPaused {
		if checkpointState == CheckpointStateReady {
			log.Printf("[snapshot-webhook] Workspace %s/%s snapshot is Ready; allowing spec.paused=true", namespace, name)
			return &admissionv1.AdmissionResponse{Allowed: true}
		}

		// Check if the pod is currently running; if there is no running pod, allow pause immediately
		pod, err := c.getWorkspacePod(ctx, namespace, name)
		if err != nil || pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
			log.Printf("[snapshot-webhook] Workspace %s/%s has no active running pod (%v); allowing spec.paused=true immediately", namespace, name, err)
			return &admissionv1.AdmissionResponse{Allowed: true}
		}
		podName := pod.Name

		log.Printf("[snapshot-webhook] Intercepting Pause on Workspace %s/%s (pod %s): holding spec.paused=false while taking GKE PodSnapshot", namespace, name, podName)

		// 1. Immediately flip the Pod's readinessGate condition to False so PodReady becomes False
		//    and workspaces-controller transitions Workspace.status.state out of Running.
		if err := c.setPodActiveCondition(ctx, namespace, podName, corev1.ConditionFalse, "CheckpointingInProgress", "Pausing: taking GKE PodSnapshot"); err != nil {
			log.Printf("[snapshot-webhook] Warning: failed to update pod readinessGate on %s/%s: %v", namespace, podName, err)
		}

		// 2. Immediately invalidate gke-access-proxy cache so open connections are blocked
		if c.invalidateCache != nil {
			c.invalidateCache(namespace, name)
		}

		// 3. Delete any stale PodSnapshotManualTrigger so reconciler creates a fresh one after settle grace period
		triggerName := fmt.Sprintf("ws-%s-trigger", name)
		_ = c.dynamic.Resource(podSnapshotManualTriggerGVR).Namespace(namespace).Delete(ctx, triggerName, metav1.DeleteOptions{})

		// 4. Mutate Workspace: keep spec.paused=false and mark checkpoint-state=Checkpointing
		annotations[AnnotationCheckpointState] = CheckpointStateCheckpointing
		annotations[AnnotationCheckpointStartedAt] = time.Now().UTC().Format(time.RFC3339)

		patches := []jsonPatchOp{
			{Op: "add", Path: "/spec/paused", Value: false},
			{Op: "add", Path: "/metadata/annotations", Value: annotations},
		}
		patchBytes, err := json.Marshal(patches)
		if err != nil {
			return &admissionv1.AdmissionResponse{Allowed: true}
		}
		patchType := admissionv1.PatchTypeJSONPatch
		c.notifyReconcile()
		return &admissionv1.AdmissionResponse{
			Allowed:   true,
			PatchType: &patchType,
			Patch:     patchBytes,
		}
	}

	// Case 2: Resume requested (spec.paused transitioning true -> false)
	if oldPaused && !newPaused && checkpointState == CheckpointStateReady {
		log.Printf("[snapshot-webhook] Workspace %s/%s resuming from checkpoint %q", namespace, name, annotations[AnnotationLastCheckpointName])
		annotations[AnnotationCheckpointState] = CheckpointStateRestoring
		patches := []jsonPatchOp{
			{Op: "add", Path: "/metadata/annotations", Value: annotations},
		}
		patchBytes, err := json.Marshal(patches)
		if err == nil {
			patchType := admissionv1.PatchTypeJSONPatch
			c.notifyReconcile()
			return &admissionv1.AdmissionResponse{
				Allowed:   true,
				PatchType: &patchType,
				Patch:     patchBytes,
			}
		}
	}

	return &admissionv1.AdmissionResponse{Allowed: true}
}

func (c *Controller) HandlePodMutate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		http.Error(w, "invalid admission review", http.StatusBadRequest)
		return
	}
	resp := c.mutatePod(r.Context(), review.Request)
	resp.UID = review.Request.UID
	review.Response = resp
	review.Request = nil
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(review)
}

func (c *Controller) mutatePod(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req.Operation != admissionv1.Create {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	wsName := pod.Labels[WorkspaceLabel]
	if wsName == "" {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	namespace := req.Namespace
	if namespace == "" {
		namespace = pod.Namespace
	}

	// Do not inject gVisor runtimeClassName on TPU pods
	for _, container := range pod.Spec.Containers {
		if _, hasTPU := container.Resources.Limits["google.com/tpu"]; hasTPU {
			return &admissionv1.AdmissionResponse{Allowed: true}
		}
		if _, hasTPU := container.Resources.Requests["google.com/tpu"]; hasTPU {
			return &admissionv1.AdmissionResponse{Allowed: true}
		}
	}

	ws, err := c.dynamic.Resource(workspaceGVR).Namespace(namespace).Get(ctx, wsName, metav1.GetOptions{})
	if err != nil {
		log.Printf("[snapshot-webhook] Could not fetch Workspace %s/%s for Pod mutation: %v", namespace, wsName, err)
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	enabled, _ := c.isSnapshotEnabledForWorkspace(ctx, ws)
	if !enabled {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}

	// Ensure jupyter-ipc-config ConfigMap exists in the namespace
	if err := c.ensureIPCConfigMap(ctx, namespace); err != nil {
		log.Printf("[snapshot-webhook] Warning: failed to ensure ConfigMap %s/%s: %v", namespace, IPCConfigMapName, err)
	}

	var patches []jsonPatchOp

	// 1. Set runtimeClassName: gvisor
	runtimeClass := DefaultGKERuntimeClass
	patches = append(patches, jsonPatchOp{
		Op:    "add",
		Path:  "/spec/runtimeClassName",
		Value: runtimeClass,
	})

	// 2. Inject readinessGate "podsnapshot.gke.kubeflow.org/active"
	readinessGates := pod.Spec.ReadinessGates
	hasGate := false
	for _, g := range readinessGates {
		if g.ConditionType == ReadinessGateConditionType {
			hasGate = true
			break
		}
	}
	if !hasGate {
		readinessGates = append(readinessGates, corev1.PodReadinessGate{
			ConditionType: ReadinessGateConditionType,
		})
		patches = append(patches, jsonPatchOp{
			Op:    "add",
			Path:  "/spec/readinessGates",
			Value: readinessGates,
		})
	}

	// 3. Inject jupyter-ipc-config Volume & VolumeMount
	volumes := pod.Spec.Volumes
	hasVolume := false
	for _, v := range volumes {
		if v.Name == IPCConfigMapName {
			hasVolume = true
			break
		}
	}
	if !hasVolume {
		volumes = append(volumes, corev1.Volume{
			Name: IPCConfigMapName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: IPCConfigMapName,
					},
				},
			},
		})
		patches = append(patches, jsonPatchOp{
			Op:    "add",
			Path:  "/spec/volumes",
			Value: volumes,
		})
	}

	if len(pod.Spec.Containers) > 0 {
		mounts := pod.Spec.Containers[0].VolumeMounts
		hasMount := false
		for _, m := range mounts {
			if m.Name == IPCConfigMapName || m.MountPath == "/etc/jupyter/jupyter_server_config.py" {
				hasMount = true
				break
			}
		}
		if !hasMount {
			mounts = append(mounts, corev1.VolumeMount{
				Name:      IPCConfigMapName,
				MountPath: "/etc/jupyter/jupyter_server_config.py",
				SubPath:   "jupyter_server_config.py",
			})
			patches = append(patches, jsonPatchOp{
				Op:    "add",
				Path:  "/spec/containers/0/volumeMounts",
				Value: mounts,
			})
		}
	}

	// 4. If Workspace has a saved checkpoint, inject podsnapshot.gke.io/ps-name annotation onto Pod
	wsAnnotations := ws.GetAnnotations()
	if lastCheckpoint := wsAnnotations[AnnotationLastCheckpointName]; lastCheckpoint != "" {
		podAnnotations := pod.Annotations
		if podAnnotations == nil {
			podAnnotations = make(map[string]string)
		}
		podAnnotations[GKERestoreAnnotation] = lastCheckpoint
		patches = append(patches, jsonPatchOp{
			Op:    "add",
			Path:  "/metadata/annotations",
			Value: podAnnotations,
		})
		log.Printf("[snapshot-webhook] Injected %s=%s onto Pod %s/%s-0", GKERestoreAnnotation, lastCheckpoint, namespace, wsName)
	} else {
		log.Printf("[snapshot-webhook] Configured Pod %s/%s-0 for GKE PodSnapshot (runtimeClassName=gvisor, IPC config, readinessGate)", namespace, wsName)
	}

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	patchType := admissionv1.PatchTypeJSONPatch
	c.notifyReconcile()
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		PatchType: &patchType,
		Patch:     patchBytes,
	}
}

func (c *Controller) isSnapshotEnabledForWorkspace(ctx context.Context, ws *unstructured.Unstructured) (bool, string) {
	annotations := ws.GetAnnotations()
	storageConfig := DefaultStorageConfigName
	if annotations != nil {
		if v, ok := annotations[AnnotationStorageConfig]; ok && v != "" {
			storageConfig = v
		}
		if v, ok := annotations[AnnotationEnabled]; ok {
			if strings.EqualFold(v, "false") {
				return false, storageConfig
			}
			if strings.EqualFold(v, "true") {
				return true, storageConfig
			}
		}
	}
	podConfig, _, _ := unstructured.NestedString(ws.Object, "spec", "podTemplate", "options", "podConfig")
	if strings.Contains(strings.ToLower(podConfig), "tpu") {
		return false, storageConfig
	}
	kindName, _, _ := unstructured.NestedString(ws.Object, "spec", "kind")
	if kindName == "" {
		return false, storageConfig
	}
	kind, err := c.dynamic.Resource(workspaceKindGVR).Get(ctx, kindName, metav1.GetOptions{})
	if err != nil {
		return false, storageConfig
	}
	kindAnnotations := kind.GetAnnotations()
	if kindAnnotations != nil {
		if v, ok := kindAnnotations[AnnotationStorageConfig]; ok && v != "" && (annotations == nil || annotations[AnnotationStorageConfig] == "") {
			storageConfig = v
		}
		if strings.EqualFold(kindAnnotations[AnnotationEnabled], "true") {
			return true, storageConfig
		}
	}
	return false, storageConfig
}

func (c *Controller) ensureIPCConfigMap(ctx context.Context, namespace string) error {
	existing, err := c.core.ConfigMaps(namespace).Get(ctx, IPCConfigMapName, metav1.GetOptions{})
	if err == nil {
		if existing.Data["jupyter_server_config.py"] == jupyterIPCServerConfig {
			return nil
		}
		existing.Data = map[string]string{"jupyter_server_config.py": jupyterIPCServerConfig}
		_, err = c.core.ConfigMaps(namespace).Update(ctx, existing, metav1.UpdateOptions{})
		return err
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      IPCConfigMapName,
			Namespace: namespace,
		},
		Data: map[string]string{
			"jupyter_server_config.py": jupyterIPCServerConfig,
		},
	}
	_, err = c.core.ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (c *Controller) ensureStorageConfigAndPolicy(ctx context.Context, ws *unstructured.Unstructured, storageConfigName string) error {
	// 1. Ensure cluster-scoped PodSnapshotStorageConfig exists
	_, err := c.dynamic.Resource(podSnapshotStorageConfigGVR).Get(ctx, storageConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		sc := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "podsnapshot.gke.io/v1",
				"kind":       "PodSnapshotStorageConfig",
				"metadata": map[string]any{
					"name": storageConfigName,
				},
				"spec": map[string]any{
					"snapshotStorageConfig": map[string]any{
						"gcs": map[string]any{
							"bucket": c.snapshotBucket,
							"path":   "kubeflow-notebooks",
						},
					},
				},
			},
		}
		if _, createErr := c.dynamic.Resource(podSnapshotStorageConfigGVR).Create(ctx, sc, metav1.CreateOptions{}); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return createErr
		}
	}

	// 2. Ensure namespace-scoped PodSnapshotPolicy exists for this Workspace
	namespace := ws.GetNamespace()
	name := ws.GetName()
	policyName := fmt.Sprintf("ws-%s-policy", name)
	_, err = c.dynamic.Resource(podSnapshotPolicyGVR).Namespace(namespace).Get(ctx, policyName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		policy := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "podsnapshot.gke.io/v1",
				"kind":       "PodSnapshotPolicy",
				"metadata": map[string]any{
					"name":      policyName,
					"namespace": namespace,
					"ownerReferences": []any{
						map[string]any{
							"apiVersion":         "kubeflow.org/v1beta1",
							"kind":               "Workspace",
							"name":               name,
							"uid":                string(ws.GetUID()),
							"controller":         false,
							"blockOwnerDeletion": true,
						},
					},
				},
				"spec": map[string]any{
					"storageConfigName": storageConfigName,
					"selector": map[string]any{
						"matchLabels": map[string]any{
							WorkspaceLabel: name,
						},
					},
					"triggerConfig": map[string]any{
						"type":           "manual",
						"postCheckpoint": "stop",
					},
					"retentionConfig": map[string]any{
						"lastAccessTimeout": "7d",
					},
				},
			},
		}
		if _, createErr := c.dynamic.Resource(podSnapshotPolicyGVR).Namespace(namespace).Create(ctx, policy, metav1.CreateOptions{}); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return createErr
		}
	}
	return nil
}

func (c *Controller) setPodActiveCondition(ctx context.Context, namespace, podName string, status corev1.ConditionStatus, reason, message string) error {
	pod, err := c.core.Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	now := metav1.Now()
	updatedGate := false
	gateChanged := false
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == ReadinessGateConditionType {
			updatedGate = true
			if pod.Status.Conditions[i].Status != status {
				pod.Status.Conditions[i].Status = status
				pod.Status.Conditions[i].LastTransitionTime = now
				pod.Status.Conditions[i].Reason = reason
				pod.Status.Conditions[i].Message = message
				gateChanged = true
			}
			break
		}
	}
	if !updatedGate {
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
			Type:               ReadinessGateConditionType,
			Status:             status,
			LastTransitionTime: now,
			Reason:             reason,
			Message:            message,
		})
		gateChanged = true
	}

	// Synchronously update PodReady so workspaces-controller sees the transition immediately
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodReady {
			if status == corev1.ConditionFalse && pod.Status.Conditions[i].Status != corev1.ConditionFalse {
				pod.Status.Conditions[i].Status = corev1.ConditionFalse
				pod.Status.Conditions[i].LastTransitionTime = now
				pod.Status.Conditions[i].Reason = reason
				pod.Status.Conditions[i].Message = message
				gateChanged = true
			} else if status == corev1.ConditionTrue && pod.Status.Conditions[i].Status != corev1.ConditionTrue && allContainersReady(pod) {
				pod.Status.Conditions[i].Status = corev1.ConditionTrue
				pod.Status.Conditions[i].LastTransitionTime = now
				pod.Status.Conditions[i].Reason = "ContainersReady"
				pod.Status.Conditions[i].Message = ""
				gateChanged = true
			}
			break
		}
	}

	if !gateChanged {
		return nil
	}
	_, err = c.core.Pods(namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	return err
}

func allContainersReady(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (c *Controller) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var mu sync.Mutex
	reconcileAll := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, ns := range c.tenants {
			c.reconcileNamespace(ctx, ns)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.triggerCh:
			reconcileAll()
		case <-ticker.C:
			reconcileAll()
		}
	}
}

func (c *Controller) reconcileNamespace(ctx context.Context, namespace string) {
	list, err := c.dynamic.Resource(workspaceGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for i := range list.Items {
		ws := &list.Items[i]
		if err := c.reconcileWorkspace(ctx, ws); err != nil {
			log.Printf("[snapshot-reconciler] Error reconciling Workspace %s/%s: %v", namespace, ws.GetName(), err)
		}
	}
}

func (c *Controller) reconcileWorkspace(ctx context.Context, ws *unstructured.Unstructured) error {
	enabled, storageConfigName := c.isSnapshotEnabledForWorkspace(ctx, ws)
	if !enabled {
		return nil
	}
	if err := c.ensureStorageConfigAndPolicy(ctx, ws, storageConfigName); err != nil {
		return fmt.Errorf("ensureStorageConfigAndPolicy: %w", err)
	}

	namespace := ws.GetNamespace()
	name := ws.GetName()
	annotations := ws.GetAnnotations()
	checkpointState := annotations[AnnotationCheckpointState]
	lastCheckpoint := annotations[AnnotationLastCheckpointName]
	paused, _, _ := unstructured.NestedBool(ws.Object, "spec", "paused")

	// 1. Handle Checkpointing state (Pause requested, spec.paused held at false)
	if checkpointState == CheckpointStateCheckpointing {
		pod, err := c.getWorkspacePod(ctx, namespace, name)
		if apierrors.IsNotFound(err) {
			// Pod already gone; finalize pause state
			return c.patchWorkspacePauseAndAnnotations(ctx, namespace, name, true, map[string]any{
				AnnotationCheckpointState:     CheckpointStateReady,
				AnnotationCheckpointStartedAt: nil,
			})
		}
		if err != nil {
			return err
		}
		podName := pod.Name

		// Ensure Pod readinessGate is False so UI shows Unknown (Connect disabled, Stop hidden)
		_ = c.setPodActiveCondition(ctx, namespace, podName, corev1.ConditionFalse, "CheckpointingInProgress", "Pausing: taking GKE PodSnapshot")

		// Enforce Socket Settle Grace Period before creating PodSnapshotManualTrigger
		if startedStr := annotations[AnnotationCheckpointStartedAt]; startedStr != "" {
			if startedAt, parseErr := time.Parse(time.RFC3339, startedStr); parseErr == nil {
				if elapsed := time.Since(startedAt); elapsed < SocketSettleGracePeriod {
					return nil
				}
			}
		}

		triggerName := fmt.Sprintf("ws-%s-trigger", name)
		trigger, err := c.dynamic.Resource(podSnapshotManualTriggerGVR).Namespace(namespace).Get(ctx, triggerName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			log.Printf("[snapshot-reconciler] Creating PodSnapshotManualTrigger %s/%s for pod %s", namespace, triggerName, podName)
			newTrigger := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "podsnapshot.gke.io/v1",
					"kind":       "PodSnapshotManualTrigger",
					"metadata": map[string]any{
						"name":      triggerName,
						"namespace": namespace,
						"ownerReferences": []any{
							map[string]any{
								"apiVersion":         "kubeflow.org/v1beta1",
								"kind":               "Workspace",
								"name":               name,
								"uid":                string(ws.GetUID()),
								"controller":         false,
								"blockOwnerDeletion": true,
							},
						},
					},
					"spec": map[string]any{
						"targetPod": podName,
					},
				},
			}
			_, err = c.dynamic.Resource(podSnapshotManualTriggerGVR).Namespace(namespace).Create(ctx, newTrigger, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}

		snapshotName, found, _ := unstructured.NestedString(trigger.Object, "status", "snapshotCreated", "name")
		if !found || snapshotName == "" {
			return nil
		}

		snapshot, err := c.dynamic.Resource(podSnapshotGVR).Namespace(namespace).Get(ctx, snapshotName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		// Attach Workspace ownerReference to PodSnapshot so deleting a paused Workspace
		// automatically triggers Kubernetes GC + GKE PodSnapshot finalizer to delete GCS files.
		if len(snapshot.GetOwnerReferences()) == 0 && ws.GetUID() != "" {
			snapshot.SetOwnerReferences([]metav1.OwnerReference{
				{
					APIVersion: "kubeflow.org/v1beta1",
					Kind:       "Workspace",
					Name:       name,
					UID:        ws.GetUID(),
				},
			})
			_, _ = c.dynamic.Resource(podSnapshotGVR).Namespace(namespace).Update(ctx, snapshot, metav1.UpdateOptions{})
		}

		conditions, _, _ := unstructured.NestedSlice(snapshot.Object, "status", "conditions")
		snapshotReady := false
		for _, condObj := range conditions {
			cond, ok := condObj.(map[string]any)
			if !ok {
				continue
			}
			if cond["type"] == "Ready" && cond["status"] == "True" {
				snapshotReady = true
				break
			}
		}
		if !snapshotReady {
			return nil
		}

		log.Printf("[snapshot-reconciler] PodSnapshot %s/%s is Ready! Completing pause (spec.paused=true) for Workspace %s", namespace, snapshotName, name)
		return c.patchWorkspacePauseAndAnnotations(ctx, namespace, name, true, map[string]any{
			AnnotationLastCheckpointName:  snapshotName,
			AnnotationCheckpointState:     CheckpointStateReady,
			AnnotationCheckpointStartedAt: nil,
		})
	}

	// 2. Handle Active / Restoring state (spec.paused == false and not Checkpointing)
	if !paused {
		pod, err := c.getWorkspacePod(ctx, namespace, name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		podName := pod.Name
		if pod.Status.Phase != corev1.PodRunning || !allContainersReady(pod) {
			return nil
		}

		// Ensure our custom readinessGate condition is True so PodReady becomes True
		if err := c.setPodActiveCondition(ctx, namespace, podName, corev1.ConditionTrue, "WorkspaceActive", "Workspace pod is active"); err != nil {
			return err
		}

		// Re-fetch or check if Pod is Ready and cleanup any consumed PodSnapshot resources
		if lastCheckpoint != "" || checkpointState != "" {
			updatedPod, err := c.core.Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil || !isPodReady(updatedPod) {
				return nil
			}
			log.Printf("[snapshot-reconciler] Workspace %s/%s restored and Ready; cleaning up PodSnapshot %q", namespace, name, lastCheckpoint)
			if lastCheckpoint != "" {
				_ = c.dynamic.Resource(podSnapshotGVR).Namespace(namespace).Delete(ctx, lastCheckpoint, metav1.DeleteOptions{})
			}
			triggerName := fmt.Sprintf("ws-%s-trigger", name)
			_ = c.dynamic.Resource(podSnapshotManualTriggerGVR).Namespace(namespace).Delete(ctx, triggerName, metav1.DeleteOptions{})

			return c.patchWorkspacePauseAndAnnotations(ctx, namespace, name, false, map[string]any{
				AnnotationLastCheckpointName:  nil,
				AnnotationCheckpointState:     nil,
				AnnotationCheckpointStartedAt: nil,
			})
		}
	}

	return nil
}

func (c *Controller) patchWorkspacePauseAndAnnotations(ctx context.Context, namespace, name string, paused bool, annotations map[string]any) error {
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": annotations,
		},
		"spec": map[string]any{
			"paused": paused,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.dynamic.Resource(workspaceGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	return err
}
