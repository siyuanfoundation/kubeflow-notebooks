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

package helper

import (
	"google.golang.org/protobuf/proto"
	istiov1 "istio.io/client-go/pkg/apis/networking/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// copyLabelFields copies metadata.labels from desired to target, returning the updated map and whether an update is required.
func copyLabelFields(desiredLabels map[string]string, targetLabels map[string]string) (map[string]string, bool) {
	requireUpdate := false

	for k, v := range targetLabels {
		if desiredLabels[k] != v {
			requireUpdate = true
		}
	}
	return desiredLabels, requireUpdate
}

// copyAnnotationFields copies metadata.annotations from desired to target, returning the updated map and whether an update is required.
func copyAnnotationFields(desiredAnnotations map[string]string, targetAnnotations map[string]string) (map[string]string, bool) {
	requireUpdate := false

	for k, v := range targetAnnotations {
		if desiredAnnotations[k] != v {
			requireUpdate = true
		}
	}
	return desiredAnnotations, requireUpdate
}

// CopyStatefulSetFields updates a target StatefulSet with the fields from a desired StatefulSet, returning true if an update is required.
func CopyStatefulSetFields(desired *appsv1.StatefulSet, target *appsv1.StatefulSet) bool {
	requireUpdate := false

	// copy `metadata.labels`
	var updated bool
	target.Labels, updated = copyLabelFields(desired.Labels, target.Labels)
	if updated {
		requireUpdate = true
	}

	// copy `metadata.annotations`
	target.Annotations, updated = copyAnnotationFields(desired.Annotations, target.Annotations)
	if updated {
		requireUpdate = true
	}

	// copy `spec.replicas`
	if *desired.Spec.Replicas != *target.Spec.Replicas {
		*target.Spec.Replicas = *desired.Spec.Replicas
		requireUpdate = true
	}

	// copy `spec.selector`
	//
	// TODO: confirm if StatefulSets support updates to the selector
	//       if not, we might need to recreate the StatefulSet
	//
	if !equality.Semantic.DeepEqual(target.Spec.Selector, desired.Spec.Selector) {
		target.Spec.Selector = desired.Spec.Selector
		requireUpdate = true
	}

	// copy `spec.template`
	if TemplatesDiffer(&desired.Spec.Template, &target.Spec.Template, false) {
		target.Spec.Template = desired.Spec.Template
		requireUpdate = true
	}

	// copy `spec.updateStrategy`
	if !equality.Semantic.DeepEqual(target.Spec.UpdateStrategy, desired.Spec.UpdateStrategy) {
		target.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
		requireUpdate = true
	}

	return requireUpdate
}

// CopyServiceFields updates a target Service with the fields from a desired Service, returning true if an update is required.
func CopyServiceFields(desired *corev1.Service, target *corev1.Service) bool {
	requireUpdate := false

	// copy `metadata.labels`
	var updated bool
	target.Labels, updated = copyLabelFields(desired.Labels, target.Labels)
	if updated {
		requireUpdate = true
	}

	// copy `metadata.annotations`
	target.Annotations, updated = copyAnnotationFields(desired.Annotations, target.Annotations)
	if updated {
		requireUpdate = true
	}

	// NOTE: we don't copy the entire `spec` because we can't overwrite the `spec.clusterIp` and similar fields

	// copy `spec.ports`
	if !equality.Semantic.DeepEqual(target.Spec.Ports, desired.Spec.Ports) {
		target.Spec.Ports = desired.Spec.Ports
		requireUpdate = true
	}

	// copy `spec.selector`
	if !equality.Semantic.DeepEqual(target.Spec.Selector, desired.Spec.Selector) {
		target.Spec.Selector = desired.Spec.Selector
		requireUpdate = true
	}

	// copy `spec.type`
	if target.Spec.Type != desired.Spec.Type {
		target.Spec.Type = desired.Spec.Type
		requireUpdate = true
	}

	return requireUpdate
}

// CopyVirtualServiceFields updates a target VirtualService with the fields from a desired VirtualService, returning true if an update is required.
func CopyVirtualServiceFields(desired *istiov1.VirtualService, target *istiov1.VirtualService) bool {
	requireUpdate := false

	// copy `metadata.labels`
	var updated bool
	target.Labels, updated = copyLabelFields(desired.Labels, target.Labels)
	if updated {
		requireUpdate = true
	}

	// copy `metadata.annotations`
	target.Annotations, updated = copyAnnotationFields(desired.Annotations, target.Annotations)
	if updated {
		requireUpdate = true
	}

	// copy `spec`
	// NOTE: we use proto.Equal to compare the specs of Istio resources are protobuf messages
	//       and messages with the same value are not considered equal with reflect.DeepEqual
	if !proto.Equal(&target.Spec, &desired.Spec) {
		target.Spec = *desired.Spec.DeepCopy()
		requireUpdate = true
	}

	return requireUpdate
}

// TemplatesDiffer compares two PodTemplateSpecs, returning true if they differ in fields we manage.
// If ignoreRestoreAnnotation is true, the GKE pod snapshot restore annotation is ignored in the comparison.
func TemplatesDiffer(desired, target *corev1.PodTemplateSpec, ignoreRestoreAnnotation bool) bool {
	// 1. Compare Annotations
	desiredAnn := desired.Annotations
	targetAnn := target.Annotations
	if ignoreRestoreAnnotation {
		desiredAnn = cloneAndRemove(desiredAnn, "podsnapshot.gke.io/ps-name")
		targetAnn = cloneAndRemove(targetAnn, "podsnapshot.gke.io/ps-name")
	}
	if !equality.Semantic.DeepEqual(desiredAnn, targetAnn) {
		return true
	}

	// 2. Compare Labels
	if !equality.Semantic.DeepEqual(desired.Labels, target.Labels) {
		return true
	}

	// 3. Compare RuntimeClassName
	if !equality.Semantic.DeepEqual(desired.Spec.RuntimeClassName, target.Spec.RuntimeClassName) {
		return true
	}

	// 4. Compare ServiceAccountName
	if desired.Spec.ServiceAccountName != target.Spec.ServiceAccountName {
		return true
	}

	// 5. Compare Volumes
	if volumesDiffer(desired.Spec.Volumes, target.Spec.Volumes) {
		return true
	}

	// 6. Compare Containers
	if len(desired.Spec.Containers) != len(target.Spec.Containers) {
		return true
	}
	for i := range desired.Spec.Containers {
		d := desired.Spec.Containers[i]
		t := target.Spec.Containers[i]
		if d.Name != t.Name {
			return true
		}
		if d.Image != t.Image {
			return true
		}
		if !equality.Semantic.DeepEqual(d.Ports, t.Ports) {
			return true
		}
		if !equality.Semantic.DeepEqual(d.Env, t.Env) {
			return true
		}
		if !equality.Semantic.DeepEqual(d.Resources.Requests, t.Resources.Requests) ||
			!equality.Semantic.DeepEqual(d.Resources.Limits, t.Resources.Limits) {
			return true
		}
		if !equality.Semantic.DeepEqual(d.VolumeMounts, t.VolumeMounts) {
			return true
		}
		// Compare security context fields we set
		if d.SecurityContext == nil && t.SecurityContext != nil {
			if t.SecurityContext.AllowPrivilegeEscalation != nil ||
				t.SecurityContext.RunAsNonRoot != nil ||
				t.SecurityContext.Capabilities != nil {
				return true
			}
		} else if d.SecurityContext != nil && t.SecurityContext == nil {
			return true
		} else if d.SecurityContext != nil && t.SecurityContext != nil {
			if !equality.Semantic.DeepEqual(d.SecurityContext.AllowPrivilegeEscalation, t.SecurityContext.AllowPrivilegeEscalation) ||
				!equality.Semantic.DeepEqual(d.SecurityContext.RunAsNonRoot, t.SecurityContext.RunAsNonRoot) ||
				!equality.Semantic.DeepEqual(d.SecurityContext.Capabilities, t.SecurityContext.Capabilities) {
				return true
			}
		}
	}

	return false
}

func cloneAndRemove(m map[string]string, key string) map[string]string {
	if m == nil {
		return nil
	}
	res := make(map[string]string)
	for k, v := range m {
		if k != key {
			res[k] = v
		}
	}
	return res
}

func volumesDiffer(desired, target []corev1.Volume) bool {
	if len(desired) != len(target) {
		return true
	}
	// Create a map of target volumes by name
	targetMap := make(map[string]corev1.Volume)
	for _, v := range target {
		targetMap[v.Name] = v
	}

	for _, d := range desired {
		t, ok := targetMap[d.Name]
		if !ok {
			return true
		}
		// Compare PVC source
		if d.PersistentVolumeClaim != nil {
			if t.PersistentVolumeClaim == nil {
				return true
			}
			if d.PersistentVolumeClaim.ClaimName != t.PersistentVolumeClaim.ClaimName {
				return true
			}
			if d.PersistentVolumeClaim.ReadOnly != t.PersistentVolumeClaim.ReadOnly {
				return true
			}
		} else if t.PersistentVolumeClaim != nil {
			return true
		}

		// Compare ConfigMap source
		if d.ConfigMap != nil {
			if t.ConfigMap == nil {
				return true
			}
			if d.ConfigMap.Name != t.ConfigMap.Name {
				return true
			}
			// Only compare defaultMode if desired specifies it
			if d.ConfigMap.DefaultMode != nil {
				if t.ConfigMap.DefaultMode == nil || *d.ConfigMap.DefaultMode != *t.ConfigMap.DefaultMode {
					return true
				}
			}
		} else if t.ConfigMap != nil {
			return true
		}

		// Compare Secret source
		if d.Secret != nil {
			if t.Secret == nil {
				return true
			}
			if d.Secret.SecretName != t.Secret.SecretName {
				return true
			}
			if d.Secret.DefaultMode != nil {
				if t.Secret.DefaultMode == nil || *d.Secret.DefaultMode != *t.Secret.DefaultMode {
					return true
				}
			}
		} else if t.Secret != nil {
			return true
		}

		// Compare EmptyDir source
		if d.EmptyDir != nil {
			if t.EmptyDir == nil {
				return true
			}
			if d.EmptyDir.Medium != t.EmptyDir.Medium {
				return true
			}
			if !equality.Semantic.DeepEqual(d.EmptyDir.SizeLimit, t.EmptyDir.SizeLimit) {
				return true
			}
		} else if t.EmptyDir != nil {
			return true
		}
	}
	return false
}
