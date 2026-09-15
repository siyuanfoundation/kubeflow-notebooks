# Integrating Kubeflow Notebooks v2 with Ray (KubeRay) on GKE

This guide walks you through integrating **Kubeflow Notebooks v2** (`Workspace` & `WorkspaceKind`) with **Ray (KubeRay)** on Google Kubernetes Engine (GKE).

In this architecture, a lightweight Kubeflow Notebook pod (`tiny_cpu`: 0.1 CPU, 128 MiB RAM) acts as an interactive **control plane** to dynamically create, connect to, submit distributed tasks/jobs to, and tear down a multi-node `RayCluster` within the user's namespace.

---

## Directory Contents

| File | Description |
|------|-------------|
| [`ray-clusterrole.yaml`](./ray-clusterrole.yaml) | `ClusterRole` definitions (`ray-edit` and `ray-view`) granting permissions to create, inspect, update, and delete `RayCluster`, `RayJob`, and `RayService` custom resources. |
| [`jupyterlab_v1beta1_workspacekind.yaml`](./jupyterlab_v1beta1_workspacekind.yaml) | Sample `WorkspaceKind` (`jupyterlab-ray`) pre-configured with `serviceAccount.clusterRoles` binding `kubeflow-edit` and `ray-edit` to each Workspace's ServiceAccount (`ws-<workspace-name>`). |
| [`raycluster.yaml`](./raycluster.yaml) | Sample `RayCluster` manifest (`ray.io/v1`) with a dedicated Head Pod and autoscaling Worker Group using Python 3.11 (`rayproject/ray:2.41.0-py311`) matching `jupyter-scipy:v1.10.0`. |
| [`ray_example.ipynb`](./ray_example.ipynb) | End-to-end sample Jupyter Notebook demonstrating programmatic `RayCluster` creation, interactive `@ray.remote` tasks & actors via Ray Client (`:10001`), batch jobs via Ray Jobs API (`:8265`), and cluster cleanup. |

---

## Architecture

```
  ┌─────────────────────────────────────────────────────────────────┐
  │       Kubeflow Notebook v2 Pod (Tiny CPU: 0.1 CPU, 128Mi)       │
  │         ServiceAccount: ws-<workspace-name> (ray-edit)          │
  └───────────────┬─────────────────────────────────┬───────────────┘
                  │ 1. Create / Delete RayCluster   │ 2. Submit Tasks / Jobs
                  ▼ (Kubernetes CustomObjectsApi)   ▼ (Ray Client :10001 / Jobs API :8265)
  ┌───────────────────────────────┐ ┌───────────────────────────────────────────────┐
  │    Kubernetes API Server      │ │           RayCluster (ray.io/v1)              │
  │      + KubeRay Operator       │ │  ┌─────────────────┐   ┌───────────────────┐  │
  └───────────────────────────────┘ │  │  Ray Head Pod   │   │  Ray Worker Pods  │  │
                                    │  │ (GCS / Dash /   │──▶│  (Distributed     │  │
                                    │  │  Client Server) │   │   Task Execution) │  │
                                    │  └─────────────────┘   └───────────────────┘  │
                                    └───────────────────────────────────────────────┘
```

---

## Step 1: Install the KubeRay Operator on GKE

You can enable the KubeRay operator using either GKE's managed Ray Operator add-on or the upstream KubeRay Helm chart.

### Option A: GKE Managed Ray Operator Add-on (Recommended for GKE)
```bash
export PROJECT_ID="<your-project-id>"
export CLUSTER_NAME="<your-cluster-name>"
export REGION="<your-region>"

gcloud container clusters update "${CLUSTER_NAME}" \
  --region="${REGION}" \
  --project="${PROJECT_ID}" \
  --update-addons=RayOperator=ENABLED
```

### Option B: Upstream KubeRay Operator via Helm
```bash
helm repo add kuberay https://ray-project.github.io/kuberay-helm/
helm repo update

helm install kuberay-operator kuberay/kuberay-operator \
  --namespace kuberay-system \
  --create-namespace \
  --version 1.3.0
```

### Verify KubeRay CRD & Operator Status
```bash
kubectl get crd rayclusters.ray.io
kubectl get pods -A | grep -i kuberay
```

---

## Step 2: Configure RBAC (`ClusterRole`) for Workspace Pods

### How Kubeflow Notebooks v2 RBAC Works
In Kubeflow Notebooks v2, each `Workspace` pod runs under its own dedicated Kubernetes `ServiceAccount` named `ws-<workspace-name>`, which the `workspaces-controller` creates and manages automatically.

To grant Workspace pods permission to create and delete `RayCluster` resources strictly within their own namespace:
1. Define a `ClusterRole` (`ray-edit`) containing rules for `apiGroups: ["ray.io"]` (`rayclusters`, `rayjobs`, `rayservices`).
2. Reference `ray-edit` in `WorkspaceKind.spec.podTemplate.serviceAccount.clusterRoles`. The `workspaces-controller` automatically reconciles a namespace-scoped `RoleBinding` for every Workspace of that kind.

### 2.1 Apply the `ray-edit` and `ray-view` ClusterRoles
```bash
kubectl apply -f kubeflow-notebooks/gke/ray/ray-clusterrole.yaml
```

> **Note on Aggregation:** `ray-clusterrole.yaml` includes the label `rbac.authorization.kubeflow.org/aggregate-to-kubeflow-edit: "true"`. If your cluster uses aggregated `kubeflow-edit` ClusterRoles, `ray-edit` permissions are automatically aggregated into `kubeflow-edit`.

### 2.2 Bind `ray-edit` to Your `WorkspaceKind`

You can either register the dedicated `jupyterlab-ray` `WorkspaceKind` or patch your existing `jupyterlab` `WorkspaceKind`:

#### Option A: Register `jupyterlab-ray` WorkspaceKind
```bash
kubectl apply -f kubeflow-notebooks/gke/ray/jupyterlab_v1beta1_workspacekind.yaml
```

#### Option B: Patch an Existing `jupyterlab` WorkspaceKind
Because `serviceAccount.clusterRoles` is mutable and reconciled dynamically via `RoleBinding`s, patching an existing `WorkspaceKind` takes effect immediately for all running Workspaces without restarting their pods:
```bash
kubectl patch workspacekind jupyterlab --type='merge' -p='{
  "spec": {
    "podTemplate": {
      "serviceAccount": {
        "clusterRoles": [
          {"name": "kubeflow-edit"},
          {"name": "ray-edit"}
        ]
      }
    }
  }
}'
```

### 2.3 Verify Workspace ServiceAccount Permissions
Replace `<user-namespace>` and `<workspace-name>` with your target namespace and workspace name:
```bash
export NAMESPACE="kubeflow-user-example-com"
export WORKSPACE_NAME="my-notebook"

kubectl auth can-i create rayclusters.ray.io \
  --as="system:serviceaccount:${NAMESPACE}:ws-${WORKSPACE_NAME}" \
  -n "${NAMESPACE}"
# Expected output: yes
```

---

## Step 3: Create and Manage a `RayCluster`

### Important: Python Version Alignment
When connecting to a `RayCluster` via **Ray Client** (`ray://`), the Python minor version in the notebook container **must match** the Python minor version in the `RayCluster` head/worker containers.
- Upstream `jupyter-scipy:v1.10.0` runs **Python 3.11**.
- Therefore, `raycluster.yaml` uses `rayproject/ray:2.41.0-py311`.

### Important: Disabling Istio Sidecar Injection (`sidecar.istio.io/inject: "false"`)
In Kubeflow user namespaces (`istio-injection=enabled`), Istio injects an `istio-proxy` sidecar and enforces a namespace `AuthorizationPolicy` (`ns-owner-access-istio`) requiring mTLS peer identity (`source.namespace`).
- Ray Head and Worker pods communicate via direct Pod-IP-to-Pod-IP gRPC connections on dynamically allocated ephemeral ports (e.g., `ray.rpc.NodeManagerService/GetResourceLoad`, `grpc.health.v1.Health/Check`).
- Because ephemeral ports are not registered in a Kubernetes `Service`, Envoy routes them through `PassthroughCluster` without mTLS client certificates. The receiving worker pod's `istio-proxy` denies those unauthenticated inbound gRPC health checks (`RBAC: access denied`), causing the Ray Head GCS health check manager to mark the worker nodes dead (`CrashLoopBackOff`).
- Setting `sidecar.istio.io/inject: "false"` in `headGroupSpec.template.metadata.labels` and `workerGroupSpecs[].template.metadata.labels` excludes the Ray Head and Worker pods from Istio sidecar injection while allowing the Kubeflow Notebook pod (which has `istio-proxy` with auto-mTLS) to seamlessly connect to `ray://<head-svc>:10001` and `http://<head-svc>:8265`.

### Option A: Create `RayCluster` Declaratively via `kubectl`
```bash
export NAMESPACE="kubeflow-user-example-com"

kubectl apply -f kubeflow-notebooks/gke/ray/raycluster.yaml -n "${NAMESPACE}"
```

Check the status of the cluster and its automatically created head Service (`raycluster-sample-head-svc`):
```bash
kubectl get rayclusters -n "${NAMESPACE}"
kubectl get pods -l ray.io/cluster=raycluster-sample -n "${NAMESPACE}"
kubectl get svc raycluster-sample-head-svc -n "${NAMESPACE}"
```

### Option B: Create `RayCluster` Programmatically from a Notebook
Open [`ray_example.ipynb`](./ray_example.ipynb) inside your JupyterLab Workspace. Step 1 of the notebook uses the in-cluster Kubernetes Python client (`kubernetes.client.CustomObjectsApi`) to create the `RayCluster` directly from Python.

---

## Step 4: Define and Submit Ray Workloads from a Notebook

Once the `RayCluster` reaches `state: ready`, the KubeRay operator exposes the Head Pod via a Kubernetes Service in the same namespace:
- **Service DNS**: `raycluster-sample-head-svc.<namespace>.svc.cluster.local`
- **Ray Client Port (`10001`)**: Interactive session (`ray://raycluster-sample-head-svc:10001`)
- **Ray Dashboard & Jobs API Port (`8265`)**: Batch job submission & UI (`http://raycluster-sample-head-svc:8265`)

### Pattern 1: Interactive `@ray.remote` Tasks & Actors (Ray Client)
```python
import ray

# Connect to the RayCluster head service in the same namespace
ray.init(address="ray://raycluster-sample-head-svc:10001")

@ray.remote
def square(x: int) -> int:
    return x * x

# Execute 10 tasks in parallel across worker pods
futures = [square.remote(i) for i in range(10)]
print(ray.get(futures))

ray.shutdown()
```

### Pattern 2: Production Batch Jobs (`JobSubmissionClient`)
For long-running training jobs that should continue running even if you close your notebook browser tab or pause your Workspace:
```python
from ray.job_submission import JobSubmissionClient

client = JobSubmissionClient("http://raycluster-sample-head-svc:8265")
job_id = client.submit_job(
    entrypoint="python train_job.py",
    runtime_env={"working_dir": "./ray_job_src"},
)
print("Submitted Job ID:", job_id)
print(client.get_job_logs(job_id))
```

### Viewing the Ray Dashboard UI Locally
To inspect your cluster's resource utilization, worker logs, and task flamegraphs in the Ray Dashboard:
```bash
kubectl port-forward svc/raycluster-sample-head-svc -n "${NAMESPACE}" 8265:8265
```
Then open `http://localhost:8265` in your browser.

---

## Step 5: Cleanup

To delete the `RayCluster` and release all worker/head pods back to GKE:

### From Terminal (`kubectl`):
```bash
kubectl delete -f kubeflow-notebooks/gke/ray/raycluster.yaml -n "${NAMESPACE}"
```

### From Python (`ray_example.ipynb` Step 5):
```python
from kubernetes import client, config

config.load_incluster_config()
client.CustomObjectsApi().delete_namespaced_custom_object(
    group="ray.io",
    version="v1",
    namespace="<your-namespace>",
    plural="rayclusters",
    name="raycluster-sample",
)
```
