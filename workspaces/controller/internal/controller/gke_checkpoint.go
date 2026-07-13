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
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kubefloworgv1beta1 "github.com/kubeflow/notebooks/workspaces/controller/api/v1beta1"
)

const (
	defaultGKERuntimeClass = "gvisor"
	gkeRestoreAnnotation   = "podsnapshot.gke.io/ps-name"
)

var (
	podSnapshotPolicyGVK = schema.GroupVersionKind{
		Group:   "podsnapshot.gke.io",
		Version: "v1",
		Kind:    "PodSnapshotPolicy",
	}
	podSnapshotManualTriggerGVK = schema.GroupVersionKind{
		Group:   "podsnapshot.gke.io",
		Version: "v1",
		Kind:    "PodSnapshotManualTrigger",
	}
	podSnapshotGVK = schema.GroupVersionKind{
		Group:   "podsnapshot.gke.io",
		Version: "v1",
		Kind:    "PodSnapshot",
	}
)

func (r *WorkspaceReconciler) reconcileGKEPodCheckpoint(ctx context.Context, log logr.Logger, workspace *kubefloworgv1beta1.Workspace, workspaceKind *kubefloworgv1beta1.WorkspaceKind, pod *corev1.Pod) (ctrl.Result, error) {
	checkpoint := workspaceKind.Spec.PodTemplate.PodCheckpoint
	gkeConfig := checkpoint.GKE
	storageConfigName := "kubeflow-pod-snapshot-storage-config"
	if gkeConfig != nil && gkeConfig.StorageConfigName != nil && *gkeConfig.StorageConfigName != "" {
		storageConfigName = *gkeConfig.StorageConfigName
	}

	// 1. Reconcile PodSnapshotPolicy
	policyName := fmt.Sprintf("ws-%s-policy", workspace.Name)
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(podSnapshotPolicyGVK)
	err := r.Get(ctx, types.NamespacedName{Name: policyName, Namespace: workspace.Namespace}, policy)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Creating PodSnapshotPolicy", "name", policyName)
			policy.SetName(policyName)
			policy.SetNamespace(workspace.Namespace)
			if err := controllerutil.SetControllerReference(workspace, policy, r.Scheme); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to set controller reference on PodSnapshotPolicy: %w", err)
			}
			policy.Object["spec"] = map[string]interface{}{
				"storageConfigName": storageConfigName,
				"selector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						workspaceNameLabel: workspace.Name,
					},
				},
				"triggerConfig": map[string]interface{}{
					"type":           "manual",
					"postCheckpoint": "stop",
				},
			}
			if err := r.Create(ctx, policy); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create PodSnapshotPolicy: %w", err)
			}
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to get PodSnapshotPolicy: %w", err)
		}
	}

	// 2. Handle pause state
	if ptr.Deref(workspace.Spec.Paused, false) {
		if workspace.Status.LastPodCheckpointName != "" {
			// Snapshot already taken successfully and scaled down. Nothing to do.
			return ctrl.Result{}, nil
		}

		// We need to take a snapshot.
		if pod == nil {
			log.Info("Pod is nil, waiting for Pod to be running to trigger snapshot")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		if pod.Status.Phase != corev1.PodRunning {
			log.Info("Pod is not running, waiting to trigger snapshot", "phase", pod.Status.Phase)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Trigger snapshot
		triggerName := fmt.Sprintf("ws-%s-trigger", workspace.Name)
		trigger := &unstructured.Unstructured{}
		trigger.SetGroupVersionKind(podSnapshotManualTriggerGVK)
		err := r.Get(ctx, types.NamespacedName{Name: triggerName, Namespace: workspace.Namespace}, trigger)
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Info("Creating PodSnapshotManualTrigger", "name", triggerName, "targetPod", pod.Name)
				trigger.SetName(triggerName)
				trigger.SetNamespace(workspace.Namespace)
				if err := controllerutil.SetControllerReference(workspace, trigger, r.Scheme); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to set controller reference on PodSnapshotManualTrigger: %w", err)
				}
				trigger.Object["spec"] = map[string]interface{}{
					"targetPod": pod.Name,
				}
				if err := r.Create(ctx, trigger); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to create PodSnapshotManualTrigger: %w", err)
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to get PodSnapshotManualTrigger: %w", err)
		}

		// Trigger exists, check if GKE has created the snapshot and populated status.snapshotCreated.name
		snapshotName, found, err := unstructured.NestedString(trigger.Object, "status", "snapshotCreated", "name")
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to read status.snapshotCreated.name from trigger: %w", err)
		}
		if !found || snapshotName == "" {
			log.Info("Waiting for snapshotCreated field to be set on trigger status")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Fetch the PodSnapshot and check if it's Ready
		snapshot := &unstructured.Unstructured{}
		snapshot.SetGroupVersionKind(podSnapshotGVK)
		err = r.Get(ctx, types.NamespacedName{Name: snapshotName, Namespace: workspace.Namespace}, snapshot)
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Info("PodSnapshot not found, waiting", "name", snapshotName)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to get PodSnapshot: %w", err)
		}

		// Check if snapshot is Ready
		conditions, found, err := unstructured.NestedSlice(snapshot.Object, "status", "conditions")
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to read status.conditions from snapshot: %w", err)
		}
		if !found || len(conditions) == 0 {
			log.Info("Waiting for conditions to be set on snapshot status")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		ready := false
		for _, condObj := range conditions {
			cond, ok := condObj.(map[string]interface{})
			if !ok {
				continue
			}
			condType, _ := cond["type"].(string)
			condStatus, _ := cond["status"].(string)
			if condType == "Ready" && condStatus == "True" {
				ready = true
				break
			}
		}

		if !ready {
			log.Info("PodSnapshot is not Ready yet, waiting...")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		log.Info("PodSnapshot is Ready. Saving to status", "snapshotName", snapshotName)
		workspace.Status.LastPodCheckpointName = snapshotName

		return ctrl.Result{}, nil
	}

	// 3. Handle resume state (not paused)
	if !ptr.Deref(workspace.Spec.Paused, false) && workspace.Status.LastPodCheckpointName != "" {
		if pod == nil {
			log.Info("Waiting for Pod to be recreated for restore")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		if pod.Status.Phase != corev1.PodRunning {
			log.Info("Pod is not Running yet for restore", "phase", pod.Status.Phase)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Check if pod is Ready
		podReady := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				podReady = true
				break
			}
		}

		if !podReady {
			log.Info("Pod is running but not Ready yet for restore, waiting...")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		log.Info("Restore completed. Cleaning up PodSnapshot resources to prevent stale restores.", "snapshotName", workspace.Status.LastPodCheckpointName)

		snapshot := &unstructured.Unstructured{}
		snapshot.SetGroupVersionKind(podSnapshotGVK)
		snapshot.SetName(workspace.Status.LastPodCheckpointName)
		snapshot.SetNamespace(workspace.Namespace)
		if err := r.Delete(ctx, snapshot); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to delete PodSnapshot resource: %w", err)
		}

		triggerName := fmt.Sprintf("ws-%s-trigger", workspace.Name)
		trigger := &unstructured.Unstructured{}
		trigger.SetGroupVersionKind(podSnapshotManualTriggerGVK)
		trigger.SetName(triggerName)
		trigger.SetNamespace(workspace.Namespace)
		if err := r.Delete(ctx, trigger); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to delete PodSnapshotManualTrigger resource: %w", err)
		}

		workspace.Status.LastPodCheckpointName = ""
		log.Info("PodSnapshot cleanup successful.")
	}

	return ctrl.Result{}, nil
}

func applyGKECheckpointRestoreConfig(lastPodCheckpointName string, podAnnotations map[string]string) {
	podAnnotations[gkeRestoreAnnotation] = lastPodCheckpointName
}
