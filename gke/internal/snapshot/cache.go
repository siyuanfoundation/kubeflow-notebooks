package snapshot

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

// getResource reads an object from its informer cache, falling back to a live read
// while the caches are still warming up or when the resource is not watched.
func (c *Controller) getResource(ctx context.Context, gvr schema.GroupVersionResource, lister cache.GenericLister, namespace, name string) (*unstructured.Unstructured, error) {
	if lister != nil {
		var object any
		var err error
		if namespace == "" {
			object, err = lister.Get(name)
		} else {
			object, err = lister.ByNamespace(namespace).Get(name)
		}
		if err != nil {
			return nil, err
		}
		if converted := asUnstructured(object); converted != nil {
			return converted, nil
		}
		return nil, apierrors.NewNotFound(gvr.GroupResource(), name)
	}
	client := c.dynamic.Resource(gvr)
	if namespace != "" {
		return client.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	return client.Get(ctx, name, metav1.GetOptions{})
}

// getWorkspace returns a Workspace, preferring the informer cache.
func (c *Controller) getWorkspace(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	lister := c.workspaceLister
	if !c.cachesReady.Load() {
		lister = nil
	}
	return c.getResource(ctx, workspaceGVR, lister, namespace, name)
}

// getWorkspaceKind returns a cluster-scoped WorkspaceKind, preferring the cache.
func (c *Controller) getWorkspaceKind(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	lister := c.kindLister
	if !c.cachesReady.Load() {
		lister = nil
	}
	return c.getResource(ctx, workspaceKindGVR, lister, "", name)
}

// findWorkspacePod returns the Pod backing a Workspace. It reads the indexed
// informer cache and only lists against the API server while the cache is cold,
// which can happen for admission requests during the first seconds after start-up.
func (c *Controller) findWorkspacePod(ctx context.Context, namespace, name string) *corev1.Pod {
	if c.cachesReady.Load() {
		return c.workspacePod(namespace, name)
	}
	pods, err := c.core.Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: WorkspaceLabel + "=" + name})
	if err != nil {
		return nil
	}
	var fallback *corev1.Pod
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.DeletionTimestamp == nil {
			return pod
		}
		fallback = pod
	}
	return fallback
}

// snapshotEnabled reports whether stateful pause/resume is enabled for a Workspace
// and which PodSnapshotStorageConfig it should use. The Workspace annotation wins
// over the WorkspaceKind annotation, and TPU Workspaces are always excluded because
// gVisor cannot checkpoint TPU devices.
func (c *Controller) snapshotEnabled(ctx context.Context, workspace *unstructured.Unstructured) (bool, string) {
	annotations := workspace.GetAnnotations()
	storageConfig := DefaultStorageConfigName
	if value, ok := annotations[AnnotationStorageConfig]; ok && value != "" {
		storageConfig = value
	}
	if value, ok := annotations[AnnotationEnabled]; ok {
		if strings.EqualFold(value, "false") {
			return false, storageConfig
		}
		if strings.EqualFold(value, "true") {
			return true, storageConfig
		}
	}
	podConfig, _, _ := unstructured.NestedString(workspace.Object, "spec", "podTemplate", "options", "podConfig")
	if strings.Contains(strings.ToLower(podConfig), "tpu") {
		return false, storageConfig
	}
	kindName, _, _ := unstructured.NestedString(workspace.Object, "spec", "kind")
	if kindName == "" {
		return false, storageConfig
	}
	kind, err := c.getWorkspaceKind(ctx, kindName)
	if err != nil {
		return false, storageConfig
	}
	kindAnnotations := kind.GetAnnotations()
	if value, ok := kindAnnotations[AnnotationStorageConfig]; ok && value != "" && annotations[AnnotationStorageConfig] == "" {
		storageConfig = value
	}
	return strings.EqualFold(kindAnnotations[AnnotationEnabled], "true"), storageConfig
}
