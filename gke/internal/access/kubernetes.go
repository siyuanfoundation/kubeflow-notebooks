package access

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

const workspaceLabel = "notebooks.kubeflow.org/workspace-name"

var workspaceResource = schema.GroupVersionResource{Group: "kubeflow.org", Version: "v1beta1", Resource: "workspaces"}
var kindResource = schema.GroupVersionResource{Group: "kubeflow.org", Version: "v1beta1", Resource: "workspacekinds"}

type KubernetesAccess struct {
	core          coreclient.CoreV1Interface
	authorization authorizationclient.AuthorizationV1Interface
	resources     dynamic.Interface
	tenants       map[string]bool
}

func NewKubernetesAccess(config *rest.Config, tenants []string) (*KubernetesAccess, error) {
	if len(tenants) == 0 {
		return nil, fmt.Errorf("at least one managed tenant is required")
	}
	managed := make(map[string]bool, len(tenants))
	for _, tenant := range tenants {
		if len(validation.IsDNS1123Label(tenant)) != 0 || managed[tenant] {
			return nil, fmt.Errorf("invalid or duplicate tenant %q", tenant)
		}
		managed[tenant] = true
	}
	core, err := coreclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	authorization, err := authorizationclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	resources, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &KubernetesAccess{core: core, authorization: authorization, resources: resources, tenants: managed}, nil
}

func (access *KubernetesAccess) allowed(ctx context.Context, identity Identity, namespace, verb, name string) (bool, error) {
	if identity.Email == "" || !access.tenants[namespace] {
		return false, nil
	}
	review, err := access.authorization.SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:               identity.Email,
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: verb, Group: "kubeflow.org", Resource: "workspaces", Name: name},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	if review.Status.EvaluationError != "" {
		return false, fmt.Errorf("authorization evaluation failed")
	}
	return review.Status.Allowed && !review.Status.Denied, nil
}

func (access *KubernetesAccess) Namespaces(ctx context.Context, identity Identity) ([]Namespace, error) {
	names := make([]string, 0, len(access.tenants))
	for tenant := range access.tenants {
		names = append(names, tenant)
	}
	sort.Strings(names)
	result := make([]Namespace, 0, len(names))
	for _, tenant := range names {
		allowed, err := access.allowed(ctx, identity, tenant, "list", "")
		if err != nil {
			return nil, err
		}
		if allowed {
			result = append(result, Namespace{Name: tenant})
		}
	}
	return result, nil
}

func (access *KubernetesAccess) Resolve(ctx context.Context, identity Identity, namespace, name, portID string) (Target, error) {
	allowed, err := access.allowed(ctx, identity, namespace, "get", name)
	if err != nil {
		return Target{}, err
	}
	if !allowed {
		return Target{}, ErrDenied
	}
	workspace, err := access.resources.Resource(workspaceResource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Target{}, err
	}
	if workspace.GetUID() == "" || workspace.GetDeletionTimestamp() != nil {
		return Target{}, fmt.Errorf("workspace not available")
	}
	var spec struct {
		Paused      bool   `json:"paused"`
		Kind        string `json:"kind"`
		PodTemplate struct {
			Options struct {
				ImageConfig string `json:"imageConfig"`
			} `json:"options"`
		} `json:"podTemplate"`
	}
	if err := decode(workspace.Object["spec"], &spec); err != nil {
		return Target{}, err
	}
	if spec.Paused {
		return Target{}, fmt.Errorf("workspace paused")
	}
	kind, err := access.resources.Resource(kindResource).Get(ctx, spec.Kind, metav1.GetOptions{})
	if err != nil {
		return Target{}, err
	}
	var kindSpec struct {
		PodTemplate struct {
			Ports []struct {
				ID        string `json:"id"`
				Protocol  string `json:"protocol"`
				HTTPProxy struct {
					RemovePathPrefix bool           `json:"removePathPrefix"`
					RequestHeaders   map[string]any `json:"requestHeaders"`
				} `json:"httpProxy"`
			} `json:"ports"`
			Options struct {
				ImageConfig struct {
					Values []struct {
						ID   string `json:"id"`
						Spec struct {
							Ports []struct {
								ID   string `json:"id"`
								Port int32  `json:"port"`
							} `json:"ports"`
						} `json:"spec"`
					} `json:"values"`
				} `json:"imageConfig"`
			} `json:"options"`
		} `json:"podTemplate"`
	}
	if err := decode(kind.Object["spec"], &kindSpec); err != nil {
		return Target{}, err
	}
	var portNumber int32
	for _, image := range kindSpec.PodTemplate.Options.ImageConfig.Values {
		if image.ID != spec.PodTemplate.Options.ImageConfig {
			continue
		}
		for _, port := range image.Spec.Ports {
			if port.ID == portID {
				portNumber = port.Port
			}
		}
	}
	if portNumber < 1 || portNumber > 65535 {
		return Target{}, ErrDenied
	}
	declared, removePrefix := false, false
	for _, port := range kindSpec.PodTemplate.Ports {
		if port.ID != portID {
			continue
		}
		if port.Protocol != "HTTP" || len(port.HTTPProxy.RequestHeaders) != 0 {
			return Target{}, fmt.Errorf("unsupported WorkspaceKind proxy configuration")
		}
		declared, removePrefix = true, port.HTTPProxy.RemovePathPrefix
	}
	if !declared {
		return Target{}, ErrDenied
	}
	services, err := access.core.Services(namespace).List(ctx, metav1.ListOptions{LabelSelector: workspaceLabel + "=" + name})
	if err != nil {
		return Target{}, err
	}
	var target *url.URL
	for _, service := range services.Items {
		owner := metav1.GetControllerOf(&service)
		if owner == nil || owner.UID != workspace.GetUID() || owner.Name != name || owner.Kind != "Workspace" || owner.APIVersion != "kubeflow.org/v1beta1" {
			continue
		}
		if service.DeletionTimestamp != nil || service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.ExternalName != "" || net.ParseIP(service.Spec.ClusterIP) == nil || service.Spec.Selector[workspaceLabel] != name || service.Spec.Selector["statefulset"] != name {
			return Target{}, fmt.Errorf("invalid workspace Service")
		}
		for _, port := range service.Spec.Ports {
			if port.Port != portNumber || port.Protocol != corev1.ProtocolTCP {
				continue
			}
			if target != nil {
				return Target{}, fmt.Errorf("ambiguous workspace Services")
			}
			target = &url.URL{Scheme: "http", Host: net.JoinHostPort(service.Spec.ClusterIP, strconv.Itoa(int(portNumber)))}
		}
	}
	if target == nil {
		return Target{}, fmt.Errorf("workspace Service unavailable")
	}
	return Target{URL: target, RemovePrefix: removePrefix, WorkspaceUID: string(workspace.GetUID())}, nil
}

func decode(value any, target any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}
