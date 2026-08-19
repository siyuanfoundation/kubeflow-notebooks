# GKE Deployment & Execution Guide: Kubeflow Workspaces + Trainer TPU Jobs

This directory contains complete build scripts, standalone manifests, dedicated container Dockerfiles, PodSnapshot configurations, and automated verification tooling to deploy **Kubeflow Workspaces v2** and **Kubeflow Trainer v2** onto an empty Google Kubernetes Engine (GKE) cluster, enabling a CPU-based Jupyter Notebook Workspace (with Snapshot & Restore support) to trigger distributed JAX TPU training jobs (`TrainJobs`) on Cloud TPU instances.

---

## 1. System Architecture

```
+---------------------------------------------------------------------------------------------------+
| GKE Cluster                                                                                       |
|                                                                                                   |
|  +-------------------------------------+      +------------------------------------------------+  |
|  | Namespace: kubeflow-workspaces      |      | Namespace: kubeflow-system                     |  |
|  |                                     |      |                                                |  |
|  |  +-------------------------------+  |      |  +------------------------------------------+  |  |
|  |  | workspaces-controller         |  |      |  | kubeflow-trainer-controller-manager      |  |  |
|  |  | workspaces-backend            |  |      |  | jobset-controller-manager                |  |  |
|  |  | workspaces-frontend           |  |      |  +------------------------------------------+  |  |
|  |  +-------------------------------+  |                                                       |  |
|  +-------------------------------------+                                                       |  |
|                                                                                                   |  |
|  +---------------------------------------------------------------------------------------------+  |  |
|  | Namespace: default                                                                          |  |  |
|  |                                                                                             |  |  |
|  |  +-------------------------------+                 +--------------------------------------+ |  |  |
|  |  | CPU Node (gVisor enabled)     |                 | TPU Nodes (Cloud TPU v5e/v5)         | |  |  |
|  |  |                               |  Submits        |                                      | |  |  |
|  |  |  +--------------------------+ |  TrainJob /     |  +---------------------------------+ | |  |  |
|  |  |  | Jupyter Workspace Pod    | |  JobSet         |  | TrainJob Pod 0 (Host 1, 4 TPUs)    | | |  |  |
|  |  |  | (Custom Image with       | | ------------->  |  +---------------------------------+ | |  |  |
|  |  |  | kubeflow + kubernetes,   | |                 |  | TrainJob Pod 1 (Host 2, 4 TPUs)    | | |  |  |
|  |  |  | PodSnapshot enabled)     | |                 |  +---------------------------------+ | |  |  |
|  |  |  +--------------------------+ |                 +--------------------------------------+ |  |  |
|  |  +-------------------------------+                                                          |  |  |
|  +---------------------------------------------------------------------------------------------+  |  |
+---------------------------------------------------------------------------------------------------+
```

---

## 2. Manifest & File Inventory

- **`build_and_deploy_gke.sh`**: One-command automated build and deployment script. Builds custom container images, pushes them to Artifact Registry, deploys system stack, registers `WorkspaceKind` & `Workspace` YAML manifests, sets up PodSnapshot configs, and checks/displays Dashboard URL.
- **`jupyter-tpu.Dockerfile`**: Dedicated Dockerfile building the custom Jupyter notebook container image with `kubeflow` (`kubeflow-trainer-api`) and `kubernetes` Python SDKs pre-installed.
- **`jupyterlab_workspacekind.yaml`**: Kubernetes YAML manifest for `WorkspaceKind` `jupyterlab`, defining spawner options, custom image specs, `gvisor` runtime, IPC configmap mounts, and GKE `podCheckpoint` snapshot/restore capabilities.
- **`pod-snapshot-storage-config.yaml`**: Manifest defining `PodSnapshotStorageConfig` for GKE GCS snapshot storage (`sizhang_pod_snapshots`).
- **`jupyter_ipc_configmap.yaml`**: ConfigMap configuring IPC transport for Jupyter KernelManager required by GKE PodSnapshot.
- **`workspace_tpu_notebook.yaml`**: Kubernetes YAML manifest defining `PersistentVolumeClaim` `tpu-notebook-home-pvc` and `Workspace` resource `tpu-notebook`.
- **`test_jax_tpu.py`**: Python script using `kubeflow.trainer` SDK (`TrainerClient`) to submit a multi-host JAX TPU training job on 8 TPU cores across 2 hosts.
- **`verify_tpu_trainer.py`**: End-to-end validation script that temporarily suspends the TPU placeholder job, executes `test_jax_tpu.py` inside the workspace pod, verifies completion, and restores the placeholder job.
- **`tpu-compute-class.yaml`**: GKE `ComputeClass` definitions for single-host and multi-host Cloud TPU v5e slices (`tpu-v5-8-multi-host`).
- **`tpu-job-ccc.yaml`**: TPU reservation placeholder job used to hold TPU node quotas when active training is not running.

---

## 3. Prerequisites

### Create a GKE Cluster with Podsnapshot Enabled

```bash
export REGION=us-west1
export PROJECT_ID="${USER}-gke-dev"
export CLUSTER_NAME=kubeflow-cluster
export NODEPOOL_NAME=n2-pool

gcloud container clusters create $CLUSTER_NAME \
  --enable-pod-snapshots \
  --addons=GcsFuseCsiDriver,GcpFilestoreCsiDriver \
  --workload-pool=${PROJECT_ID}.svc.id.goog \
  --workload-metadata=GKE_METADATA \
  --num-nodes 3 \
  --machine-type e2-medium \
  --location=$REGION \
  --enable-image-streaming \
  --project=$PROJECT_ID

gcloud container node-pools create ${NODEPOOL_NAME}-gvisor \
    --cluster=$CLUSTER_NAME --project=$PROJECT_ID \
    --machine-type=n2-standard-16 \
    --location=$REGION \
    --image-type=cos_containerd \
    --sandbox type=gvisor \
    --enable-autoscaling \
    --min-nodes=0 \
    --max-nodes=3

gcloud container node-pools create $NODEPOOL_NAME \
    --cluster=$CLUSTER_NAME --project=$PROJECT_ID \
    --machine-type=n2-standard-16 \
    --location=$REGION \
    --enable-autoscaling \
    --min-nodes=0 \
    --max-nodes=3

gcloud container clusters get-credentials $CLUSTER_NAME --location $REGION --project $PROJECT_ID

```

### Prepare for PodSnapshot

Follow the instructions in https://docs.cloud.google.com/kubernetes-engine/docs/how-to/pod-snapshots-prepare to prepare your cluster for PodSnapshot.

---

## 4. Quickstart: Automated Build & Deployment

Execute the deployment script directly from this directory:

```bash
# Optional: configure your container registry and image tag
gcloud auth configure-docker ${REGION}-docker.pkg.dev
export REGISTRY="${REGION}-docker.pkg.dev/${PROJECT_ID}/${PROJECT_ID}-repo"
export TAG="local-gke-dev"

# Execute full build and deployment
./build_and_deploy_gke.sh
```

### What `build_and_deploy_gke.sh` Performs:
1. **Container Builds & Pushes**: Builds and pushes custom `workspaces-controller`, `workspaces-backend`, `workspaces-frontend`, and `jupyter-tpu-notebook` container images.
2. **Infrastructure**: Installs Cert-Manager, Istio service mesh, Istio Ingress Gateway, and required system namespaces (`kubeflow-system`, `kubeflow-workspaces`, `jobset-system`).
3. **Kubeflow Trainer v2**: Applies Trainer CRDs, Controller Manager, JobSet controller, ClusterTrainingRuntimes (`jax-distributed`, etc.), and RBAC definitions.
4. **Kubeflow Spark Operator**: Creates the `kubeflow` namespace and installs the Spark Operator (controller + webhook) used by the end-to-end demo's Stage 1 data processing (`SparkApplication`s). It watches all namespaces.
5. **Kubeflow Workspaces v2**: Applies Workspaces CRDs, Controller, Backend, Frontend, and patches image tags.
6. **Auth & RBAC**: Sets up standalone header injection (`kubeflow-userid: admin`) and binds default namespace ServiceAccounts to `cluster-admin` so notebook pods can create `TrainJobs` and `SparkApplication`s.
7. **Snapshot & Workspace Setup**: Replaces image placeholder and applies `pod-snapshot-storage-config.yaml`, `jupyter_ipc_configmap.yaml`, `jupyterlab_workspacekind.yaml`, `workspace_tpu_notebook.yaml`, `tpu-compute-class.yaml`, and `tpu-job-ccc.yaml`.
8. **Dashboard Reachability**: Queries `istio-ingressgateway` external IP, outputs `https://${INGRESS_IP}/workspaces/`, and verifies HTTP reachability via `curl`.

---

## 5. Verification: Running JAX TPU Jobs from Workspace

To verify that the workspace notebook pod can submit TPU training jobs end-to-end:

```bash
python3 verify_tpu_trainer.py
```

### Verification Steps Executed:
1. Suspends `tpu-job-ccc` placeholder job to release TPU hardware allocation.
2. Copies `test_jax_tpu.py` into the running workspace pod `ws-tpu-notebook-z66c9-0`.
3. Executes `test_jax_tpu.py` inside the workspace container using `kubectl exec`.
4. Verifies that `TrainerClient` creates `TrainJob`, schedules 2 pods on TPU nodes, initializes 8 TPU cores (`Global device count: 8`), and executes `jax.pmap` successfully.
5. Automatically re-applies `tpu-job-ccc.yaml` to ensure TPU instances remain reserved.

---

## 6. Accessing the Dashboard & Jupyter UI

### 1. Get Ingress Gateway IP:
```bash
kubectl get svc istio-ingressgateway -n istio-system
```
### 2. Open Dashboard in Browser:
Open `https://<EXTERNAL-IP>/workspaces/` in your browser. Navigating to a workspace will display options to Snapshot, Pause, Restore, or Terminate your running JupyterLab instance.


---

## 7. TPU Quota Safeguards

To prevent node autoscalers from deleting idle TPU nodes or losing hardware reservation:
- **`tpu-job-ccc.yaml`** holds TPU quota while training jobs are idle.
- When launching a `TrainJob`, delete or suspend `tpu-job-ccc`.
- Once `TrainJob` completes, re-apply `tpu-job-ccc.yaml`:
  ```bash
  kubectl apply -f tpu-job-ccc.yaml
  ```

---

## 8. End-to-End ML Workflow Demo

Once the base stack is deployed, the [`demo/`](demo/README.md) directory contains
a complete, realistic ML workflow that showcases **Kubeflow Notebooks as the
gateway to Kubernetes**: distributed data processing (Apache Spark via the Spark
Operator), distributed model training (multi-host TPU `TrainJob`), and inference
serving (CPU `Deployment`) — all orchestrated from a single Jupyter Workspace,
with a built-in **Pause & Resume** (snapshot/restore) demonstration.

```bash
cd demo
./scripts/run_demo.sh          # provision storage + workspace, copy files in
# then open demo/ml_workflow_demo.ipynb in the workspace and run it
```

See [`demo/README.md`](demo/README.md) for the full walkthrough.

---

## 9. Cleanup

To tear down all deployed components and workloads from your cluster:

```bash
cd ..
./gke/cleanup_gke.sh
```

To remove just the demo resources (leaving the base stack intact):

```bash
cd demo
./scripts/cleanup_demo.sh
```
