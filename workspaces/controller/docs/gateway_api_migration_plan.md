# Migrate Workspace Routing to Kubernetes Gateway API

This document proposes migrating the dynamic routing logic of the Kubeflow Workspaces (Notebooks v2) controller from Istio-specific `VirtualService` resources to standard, vendor-neutral Kubernetes **Gateway API** (`HTTPRoute`) resources.

---

## Historical Context

Since its inception, Kubeflow has been architecturally bound to **Istio**. 
* **Routing and Mesh Security**: Kubeflow uses Istio's ingress gateway to control incoming traffic, manage authorization policies (`AuthorizationPolicy`), inject user identity headers (`kubeflow-userid` via EnvoyFilters), and route requests to user notebooks.
* **Controller Implementation**: In alignment with this platform standard, the Kubeflow Notebooks controllers (both v1 and v2) were designed to dynamically generate Istio `VirtualService` resources at runtime to handle notebook URL paths.

### The Problem
This tight coupling makes it difficult to deploy Kubeflow Workspaces in **standalone** or **minimal** environments where Istio is not desired (e.g., small local developer clusters using lightweight NGINX or Traefik controllers). When Istio is disabled (`USE_ISTIO=false`), the controller reconciles the workspace pods and services correctly, but cannot expose them because it does not support any other routing mechanism.

### The Solution: Kubernetes Gateway API
The **Kubernetes Gateway API** is a collaborative, open-source standard designed to replace both the legacy `Ingress` resource and various proprietary routing CRDs (like Istio's `VirtualService`). By migrating to the Gateway API, the Workspaces controller can remain vendor-neutral while allowing users to choose whichever routing provider (Istio, NGINX Gateway Fabric, Cilium, Traefik, Kong, etc.) best fits their infrastructure.

---

## User Review Required

> [!IMPORTANT]
> The migration introduces changes to the configuration flags of the workspaces controller. The boolean `USE_ISTIO` environment variable/flag will be deprecated and replaced by a more flexible routing provider option.

> [!WARNING]
> In the Gateway API specification, an `HTTPRoute` must explicitly attach to a parent `Gateway` resource via `parentRefs`. Admins deploying this controller must configure the name and namespace of the target Gateway so that the controller can attach routes correctly.

---

## Open Questions

1. **Coexistence or Clean Cut?** Should the controller support both Istio `VirtualServices` and Gateway API `HTTPRoutes` simultaneously (determined by a config flag), or should we completely drop `VirtualService` generation in favor of Gateway API (assuming Istio users can install the Istio Gateway API controller)?
   * *Recommendation*: Support both initially via a config option (`ROUTING_PROVIDER=istio|gateway-api|none`) to ease migration for existing Kubeflow installations.
2. **Gateway Bindings Configuration**: How should we pass the target `Gateway` coordinates (`parentRefs`) to the controller?
   * *Recommendation*: Introduce environment variables `GATEWAY_NAME` and `GATEWAY_NAMESPACE` that default to `kubeflow-gateway` in namespace `kubeflow`.

---

## Proposed Changes

We will introduce changes to the **Workspaces Controller** component.

### [Component] Workspaces Controller (`workspaces/controller`)

#### [MODIFY] [go.mod](file:///usr/local/google/home/sizhang/Projects/kubeflow/kubeflow-notebooks/workspaces/controller/go.mod)
* Add standard Gateway API client dependencies:
  ```go
  sigs.k8s.io/gateway-api v1.1.0
  ```

#### [MODIFY] [environment.go](file:///usr/local/google/home/sizhang/Projects/kubeflow/kubeflow-notebooks/workspaces/controller/internal/config/environment.go)
* Deprecate `UseIstio` boolean.
* Add `RoutingProvider` enum config (`istio`, `gateway-api`, `none`).
* Add configuration properties for the parent Gateway:
  ```go
  type EnvConfig struct {
      ...
      RoutingProvider  string // ROUTING_PROVIDER (istio | gateway-api | none)
      GatewayName      string // GATEWAY_NAME (e.g. kubeflow-gateway)
      GatewayNamespace string // GATEWAY_NAMESPACE (e.g. kubeflow)
  }
  ```

#### [MODIFY] [workspace_controller.go](file:///usr/local/google/home/sizhang/Projects/kubeflow/kubeflow-notebooks/workspaces/controller/internal/controller/workspace_controller.go)
* Update `SetupWithManager` to conditionally watch `HTTPRoute` resources (from group `gateway.networking.k8s.io`) when the routing provider is set to `gateway-api`.
* Implement a new generation function `generateGatewayHTTPRoute(...)` that maps Workspace settings to a Gateway API `HTTPRoute` resource.

##### Mapping Rules (Istio VS vs Gateway API HTTPRoute)

| Feature | Istio `VirtualService` | Gateway API `HTTPRoute` |
| :--- | :--- | :--- |
| **Parent Attachment** | `spec.gateways` | `spec.parentRefs` |
| **Host Matching** | `spec.hosts` | `spec.hostnames` |
| **Path Matching** | `spec.http[].match[].uri.prefix` | `spec.rules[].matches[].path.value` (type: `PathPrefix`) |
| **Path Rewrite** | `spec.http[].rewrite.uri: "/"` | `spec.rules[].filters[].urlRewrite.path.value: "/"` (type: `ReplacePrefixMatch`) |
| **Header Set** | `spec.http[].headers.request.set` | `spec.rules[].filters[].requestHeaderModifier.set` |
| **Header Add** | `spec.http[].headers.request.add` | `spec.rules[].filters[].requestHeaderModifier.add` |
| **Header Remove** | `spec.http[].headers.request.remove` | `spec.rules[].filters[].requestHeaderModifier.remove` |
| **Backend Destination**| `spec.http[].route[].destination` | `spec.rules[].backendRefs` |

#### [MODIFY] [role.yaml](file:///usr/local/google/home/sizhang/Projects/kubeflow/kubeflow-notebooks/workspaces/controller/manifests/kustomize/base/manager/role.yaml)
* Add RBAC permissions to allow the controller to manage `HTTPRoute` resources:
  ```yaml
  - apiGroups:
    - gateway.networking.k8s.io
    resources:
    - httproutes
    verbs:
    - create
    - delete
    - get
    - list
    - patch
    - update
    - watch
  ```

---

## Verification Plan

### Automated Tests
* **Unit Tests**:
  * Expand tests in `workspace_controller_test.go` to mock the `RoutingProvider=gateway-api` setting and assert that the generated `HTTPRoute` matches the specifications (parent refs, path prefix, rewrite rules, and headers matching).
* **Local Integration Test**:
  * Setup a KinD cluster.
  * Install an implementation of Gateway API (e.g. **NGINX Gateway Fabric** or **Istio Gateway API controller**).
  * Build and run the controller locally with `ROUTING_PROVIDER=gateway-api`.
  * Create a workspace and assert that the corresponding `HTTPRoute` is created, and that requests to the workspace connect path are correctly proxied to the notebook container.
