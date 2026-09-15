# Standalone Kubeflow Workspaces on GKE: Automated Codelab & Pilot Record

> [!NOTE]
> **How this Codelab relates to [USER_GUIDE.md](USER_GUIDE.md):**
> - **[USER_GUIDE.md](USER_GUIDE.md)**: The comprehensive, step-by-step manual deployment guide explaining every GKE API, Gateway, Certificate Manager, IAP OAuth, Kubeflow Trainer, Spark Operator, and Workload Identity command in detail.
> - **CODELAB.md (this document)**:
>   - **Part I (Quickstart Codelab)**: A streamlined, end-to-end walkthrough using the automation scripts ([`deploy_standalone.sh`](deploy_standalone.sh), [`build_jupyterlab.sh`](build_jupyterlab.sh), and [`cleanup_standalone.sh`](cleanup_standalone.sh)) to deploy Standalone Kubeflow Workspaces (Notebooks v2), Kubeflow Trainer (v2), and Kubeflow Spark Operator on GKE **without Istio**, and run [`examples/distributed_tpu_example.ipynb`](examples/distributed_tpu_example.ipynb).
>   - **Part II (Maintainer Pilot Acceptance Record)**: Dated verification evidence and acceptance test records from the live GKE pilot deployments.

---

# Part I: Automated End-to-End Codelab

This quickstart deploys the entire standalone stack (Kubeflow Workspaces + Kubeflow Trainer v2 + Kubeflow Spark Operator) on GKE without Istio in 5 steps.

## Step 1: Configure Your Environment

Run commands from the repository root (`gke-notebook/`). Set your project, cluster, and user email:

```bash
export PROJECT="your-gcp-project-id"
export CLUSTER="kubeflow-notebooks"
export LOCATION="us-central1-c"             # Cluster zone or region
export REGION="us-central1"                 # Artifact Registry & GCS region
export PILOT_USERS="user1@example.com,user2@example.com"  # Comma- or space-separated Google emails to grant IAP & RBAC access
export TENANT_NAMESPACE="team-a"            # Tenant namespace
export GCS_BUCKET="${TENANT_NAMESPACE}-bucket"
```

### Optional Configuration Options
- **Option A — What if I don't have a domain? (Default: `sslip.io` zero-DNS setup)**
  Leave `NOTEBOOK_HOST` unset. `deploy_standalone.sh` automatically reserves a global external IP and configures `notebooks.<GLOBAL_EXTERNAL_IP>.sslip.io`.
- **Option B — Custom Domain (`notebooks.example.com`)**
  ```bash
  export NOTEBOOK_HOST="notebooks.example.com"
  ```
- **Option C — Google-Managed OAuth (Users within your GCP Organization)**
  Leave `OAUTH_FILE`, `IAP_CLIENT_ID`, and `IAP_SECRET_NAME` unset.
- **Option D — Custom OAuth Client (External `@gmail.com` or cross-org users)**
  Create a Web Application OAuth client in Google Auth Platform with redirect URI `https://iap.googleapis.com/v1/oauth/clientIds/CLIENT_ID:handleRedirect`, download its JSON file, and set:
  ```bash
  export OAUTH_FILE="/absolute/private/path/oauth-client.json"
  ```

---

## Step 2: Build Custom JupyterLab (CPU/GPU/TPU) & Spark Images

Build and push the custom JupyterLab and Spark 4.0.1 images (which bundle `jax[tpu]`, `kubeflow[spark]`, `kubeflow-trainer`, `google-cloud-storage`, and [`examples/distributed_tpu_example.ipynb`](examples/distributed_tpu_example.ipynb)):

```bash
./gke/build_jupyterlab.sh
```
*(Or add `--cloud-build` to build remotely with Google Cloud Build).*

---

## Step 3: Run the Automated Standalone Deployment

Run `deploy_standalone.sh` to automatically:
1. Enable required Google Cloud APIs (`container`, `compute`, `artifactregistry`, `certificatemanager`, `iap`).
2. Enable standard Gateway API on your GKE cluster (`--gateway-api=standard`) and wait for `gatewayclass/gke-l7-global-external-managed` to become `Accepted`.
3. Install `cert-manager` v1.21.2 for internal webhook certificates.
4. Build and push the 4 digest-pinned standalone Workspaces core images (`gke-access-proxy`, `gke-frontend`, `gke-controller`, `gke-backend`).
5. Reserve a global static IP, configure Google Certificate Manager (with `sslip.io` or your custom domain), and deploy the GKE Gateway and HTTPRoute.
6. Discover the exact GKE global backend service and configure the IAP audience (`IAP_AUDIENCE`).
7. Deploy **Kubeflow Trainer (v2)** (`jax-distributed`, `torch-distributed`, etc.) and **Kubeflow Spark Operator** without Istio.
8. Admit `${PILOT_USERS}` to IAP, configure tenant RBAC and baseline Pod Security in `${TENANT_NAMESPACE}`, register the `jupyterlab` `WorkspaceKind` and GPU/TPU `ComputeClasses`, and grant Workload Identity GCS access to `gs://${GCS_BUCKET}`.

```bash
./gke/deploy_standalone.sh
```

---

## Step 4: Connect to JupyterLab & Run `distributed_tpu_example.ipynb`

1. Open the public HTTPS URL printed at the end of `deploy_standalone.sh` (e.g. `https://notebooks.<IP>.sslip.io/workspaces/`) and sign in with one of the emails in `${PILOT_USERS}`.
   *(Note: If the Certificate Manager certificate is still `PROVISIONING`, wait 5–15 minutes for Load Balancer Authorization to reach `ACTIVE` status).*
2. Select namespace **`${TENANT_NAMESPACE}`** (e.g., `kubeflow-user` or `team-a`) and click **Create workspace**:
   - **Workspace Kind**: **JupyterLab Notebook** (`jupyterlab`)
   - **Image**: **jupyterlab (CPU)** (`jupyterlab-cpu`)
   - **Pod Configuration**: **Small CPU** (`small_cpu`)
   - **Home Volume**: Attach a new volume using StorageClass **`notebooks-gke-rwo`** (10 GiB)
3. Click **Create**, wait for **Running**, and click **Connect > JupyterLab**.
4. Inside JupyterLab, open `/home/jovyan/distributed_tpu_example.ipynb` (pre-installed in `jupyterlab:latest-cpu`) and run all cells:
   - **Stage 1 (Distributed Spark ETL)**: Submits a 1-driver + 4-executor Apache Spark job via `kubeflow.spark.SparkClient` (`SparkConnect`) that preprocesses 60k Fashion-MNIST images to `gs://${GCS_BUCKET}/processed/train/`.
   - **Stage 2 (Multi-Host TPU Training)**: Submits a 2-host Cloud TPU v5e `TrainJob` (8 TPU cores) via `kubeflow.trainer.TrainerClient` (`runtime="jax-distributed"`) that trains a data-parallel JAX MLP and writes weights/metrics to `gs://${GCS_BUCKET}/model/`.
   - **Stage 3 (Model Serving)**: Deploys the 2-replica `fashion-mnist-inference` CPU `Deployment` and `Service` and sends live `/predict` requests.

Monitor your distributed workloads from your terminal:
```bash
kubectl get workspaces,sparkconnects,trainjobs,jobsets,deployments,pods -n "${TENANT_NAMESPACE}"
```

---

## Step 5: Teardown & Cleanup

When finished, cleanly remove all deployed workloads and controllers:
```bash
./gke/cleanup_standalone.sh
```

---

# Part II: Maintainer Pilot Acceptance Record

## 2026-09-15 E2E Distributed ML Verification (`distributed_tpu_example.ipynb` on Standalone GKE)

- Cluster: `sizhang-gke-dev` / `us-west1` / `kubeflow-notebooks`, GKE `1.35.7-gke.1222000`, Dataplane V2, Gateway API Standard (`gke-l7-global-external-managed`), Workload Identity (`sizhang-gke-dev.svc.id.goog`).
- Tenant Namespace: `kubeflow-user`, Pilot User: `sizhang@google.com`, Artifact Registry: `us-west1-docker.pkg.dev/sizhang-gke-dev/kubeflow-repo`, GCS Bucket: `gs://kubeflow-user-bucket`.
- Deployed via `./gke/deploy_standalone.sh` and `./gke/build_jupyterlab.sh` without Istio.
- Workspace pod `ws-test-workspace-md2wx-0` created in `kubeflow-user` with `WorkspaceKind/jupyterlab` (`jupyterlab-cpu`, `small_cpu`) and Bound 10 GiB PVC `workspace-pvc` (`StorageClass/notebooks-gke-rwo`).

Observed end-to-end execution results (`jupyter nbconvert --to notebook --execute /home/jovyan/distributed_tpu_example.ipynb`):

| Stage | Component & Workloads | Result |
| --- | --- | --- |
| **Cell 0 & Storage** | Workload Identity & Tenant RBAC | `can-i create` verified for `trainjobs.trainer.kubeflow.org`, `sparkconnects.sparkoperator.k8s.io`, and `deployments.apps`. `gs://kubeflow-user-bucket` verified accessible via Workload Identity (`roles/storage.objectUser`). |
| **Stage 1: Spark ETL** | `SparkConnect/fashion-mnist-etl` (1 driver + 4 executors) | Driver `fashion-mnist-etl-server` and 4 executors (`exec-1` through `exec-4`) launched in `kubeflow-user` using dual-Python (`3.11`/`3.12`) `spark-py312:latest`. Preprocessed 60,000 train examples across 4 shards (`shard-000.npz` to `shard-003.npz`) + `_SUCCESS` marker in `gs://kubeflow-user-bucket/processed/`. |
| **Stage 2: Multi-Host TPU** | `TrainJob/cc805800034c` (`jax-distributed`, `tpu-v5-8-multi-host`) | GKE Node Auto-Provisioning provisioned 2 `ct5lp-hightpu-4t` nodes (`gke-tpu-bdf9bfbf-4hgw`, `gke-tpu-bdf9bfbf-t4jh`). 2 worker pods (`node-0-0`, `node-0-1`, 4 TPU chips each = 8 global TPU cores) trained 5 epochs in 4.2s (`final_test_accuracy: 0.8289`) and wrote `model/metrics.json` + `model/params.npz` to `gs://kubeflow-user-bucket/model/`. |
| **Stage 3: Serving** | `Deployment/fashion-mnist-inference` & `Service` | Deployed 2-replica inference service in `kubeflow-user`. Sent 5 test images from `ws-test-workspace-md2wx-0` to `http://fashion-mnist-inference.kubeflow-user.svc.cluster.local/predict`; 5/5 predictions matched ground truth (`Ankle boot`, `Pullover`, `Trouser`, `Trouser`, `Shirt`). |

## 2026-09-12 working single-user pilot

- Cluster: `ipv6-project-379110` / `us-central1-c` / `kubeflow-notebooks`, GKE `1.35.7-gke.1222000`, Dataplane V2, VPC-native networking, Workload Identity enabled.
- Edge: Global static IP `8.233.28.206`, hostname `notebooks.8.233.28.206.sslip.io`, Certificate Manager certificate map `notebooks-gke` (`ACTIVE`).
- Custom OAuth client ID `628944397724-a2uj76f0gd8g04asj0dtmq0qeod2kdn6.apps.googleusercontent.com` configured on GCPBackendPolicy.
- Google login as `aojea@google.com` reached the frontend successfully. Tenant discovery returned HTTP 200 with only `team-a`. Backend workspace audit data records that verified email as the creator.
- Browser creation produced Workspace `team-a/gke-pilot` and Bound 10 GiB PVC `gke-pilot-home`.

Observed acceptance results:

| Check | Result |
| --- | --- |
| Public TLS and unauthenticated access | Valid TLS; anonymous HTTPS returns 302 to Google login |
| Notebook HTTP | Public Jupyter status and kernelspec APIs return 200; JupyterLab renders |
| Kernel WebSocket | Executed a notebook cell through JupyterLab; output was `GKE kernel result: 42` |
| File operations | Wrote and read `gke-pilot-validation.txt`; saved executed `gke-pilot-validation.ipynb` and read its output back |
| Terminal WebSocket | Received `GKE_TERMINAL_OK` and UID `1000` over the public terminal WebSocket |
| Pause/resume | UI Stop removed the pod while the PVC stayed Bound; UI Start created a replacement pod on the same PVC/disk; both saved files and notebook output survived |
| Cross-tenant authorization | Authenticated API and notebook requests to `default` returned 403 |
| Forged identity headers | Cross-tenant API request with forged Kubeflow user/group and impersonation headers still returned 403 |
| Pilot RBAC | Required workspace/PVC actions allowed; tested Secret, pod, Service, policy, role, WorkspaceKind creation and cross-tenant actions denied |
| Admission | Allowed notebook dry run passed; Secret mounts, pod metadata, and an unreviewed kind were each rejected by the pilot admission policy |
| Direct ingress bypass | Tokenless probe in `default` reached public HTTPS (302), while direct backend/frontend/proxy Services and notebook Service/pod connections all timed out; probe was deleted afterward |
| Pod hardening | Actual notebook runs as UID 1000, without privilege escalation, with all capabilities dropped and RuntimeDefault seccomp |

## 2026-09-12 Kubernetes tokens and VS Code

Implemented optional `desktopHostname` support entirely under `gke/`. The IAP-protected page `/workspaces/connections` lists the signed-in user's grants, issues a connection URL for an authorized running workspace, and revokes grants. Each grant is a dedicated no-RBAC ServiceAccount in `notebooks-connections`, with TokenRequest audience `https://connect.8.233.28.206.sslip.io/desktop`.

Verification:
- `scripts/test-connection-tokens.sh` confirmed dedicated audience acceptance, wrong-audience rejection, and rejection of ordinary Kubernetes API authentication.
- Public unauthenticated discovery returned 401 over verified TLS. An issued token discovered `python3`, created a kernel (201), and executed `DESKTOP_KERNEL_OK 42` over WebSocket. Revocation returned 204, the active socket closed, and later requests with that token returned 403.
- The operator generated a fresh URL through the page, selected its remote Python kernel in the standard Microsoft Jupyter extension 2025.9.1, and Cell 2 of `vscode_validation.ipynb` executed through VS Code (`ws-gke-pilot-2qdwp-0`, Linux, `/opt/conda/bin/python`, UID 1000 assertions, result 42).

## 2026-09-12 configurable token lifetimes

Replaced the initial fixed one-hour lifetime with administrator-configurable `connectionTokenDefaultSeconds` (default 86400) and `connectionTokenMaxSeconds` (default 604800). The common policy requires `600 <= default <= maximum <= 2592000` seconds.

Live GKE TokenRequest measurements:

| Requested | Granted |
| --- | --- |
| 86400 seconds (1 day) | 86400 seconds, within one-second measurement overhead |
| 604800 seconds (7 days) | 172800 seconds (2 days) |
| 2592000 seconds (30 days) | 172800 seconds (2 days) |

A public token-based kernel test assigned `lifetime_value = 42`, disconnected, and revoked the original grant. A replacement grant accessed the same kernel ID and executed `print(lifetime_value)`, yielding 42. The original token returned 403.

## 2026-09-12 shutdown recovery snapshot

- Repository branch `gke`, HEAD `24ce51e5b19adcef194aa989e9df6bbee3b3c1f9`.
- Latest deployed image and complete plan are the configurable-lifetime versions recorded above: `rendered/desktop-lifetimes-v1`, image digest `sha256:2470150446b3e87644d851fe1b2817db5db1f56a5d1738a13632ddbc8d0c5e32`.
