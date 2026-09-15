# Deploying Standalone Kubeflow Workspaces, Trainer, & Spark Operator on GKE (No Istio)

This guide provides a clear, step-by-step walkthrough for deploying **Standalone Kubeflow Workspaces (Notebooks v2)**, **Kubeflow Trainer (v2)**, and **Kubeflow Spark Operator** on Google Kubernetes Engine (GKE) **without depending on Istio**.

Instead of Istio service mesh, ingress gateways, and sidecars, this standalone architecture uses native Google Cloud and Kubernetes primitives:
- **GKE Gateway API (`gke-l7-global-external-managed`)**: Google-managed global external HTTPS load balancing.
- **Google Certificate Manager**: Automated public TLS certificates using Load Balancer Authorization.
- **Identity-Aware Proxy (IAP)**: Google authentication and identity assertion at the edge.
- **GKE Access Proxy (`gke-access-proxy`)**: Validates signed IAP JWT assertions, enforces per-request Kubernetes `SubjectAccessReview` RBAC checks, routes HTTP and WebSocket connections to workspaces, and supports optional Kubernetes-minted connection tokens for the VS Code Jupyter extension.
- **Kubernetes NetworkPolicy**: Strictly protects Workspace pods (`gke-tenant-ingress`) so **only `gke-access-proxy` and `workspaces-controller` can reach Workspace pods** (no direct pod-to-pod access to notebooks), while a separate scoped policy (`gke-tenant-workloads-ingress` excluding `notebooks.kubeflow.org/workspace-name`) allows non-workspace workload pods in `team-a` (Spark driver/executors, multi-host TPU `TrainJob` hosts, and inference pods) to communicate within `team-a`.

---

## Architecture Comparison & Document Guide

| Feature | Standalone GKE Deployment (`gke-notebook/gke`) | Community Distribution (`kubeflow-notebooks/gke`) |
| --- | --- | --- |
| **Service Mesh / Ingress** | **No Istio** — Native GKE Gateway API (`gke-l7-global-external-managed`) | Istio IngressGateway + Istio CNI + mTLS sidecars |
| **Public HTTPS / TLS** | Google Certificate Manager (Load Balancer Authorization) | `cert-manager` + Let's Encrypt ACME HTTP-01 solver |
| **Authentication** | Google Identity-Aware Proxy (IAP) + Signed JWT verification | Dex OIDC + `oauth2-proxy` |
| **Authorization** | Access Proxy + Kubernetes `SubjectAccessReview` RBAC | Istio `AuthorizationPolicy` + Kubeflow Profiles |
| **Workloads Supported** | Kubeflow Workspaces (v2), Kubeflow Trainer (v2), Spark Operator | Full Kubeflow Community Distribution |

> [!TIP]
> **Which document should I read?**
> - **[USER_GUIDE.md](USER_GUIDE.md) (this document)**: The authoritative, step-by-step customer deployment guide with configuration options (domain vs. `sslip.io`, Google-managed OAuth vs. custom OAuth), distributed Spark + TPU setup, and VS Code desktop connection instructions.
> - **[CODELAB.md](CODELAB.md)**: A streamlined quickstart walkthrough using the automation scripts (`deploy_standalone.sh`, `build_jupyterlab.sh`, `cleanup_standalone.sh`) along with the historical pilot verification record.
> - **[DESIGN.md](DESIGN.md)**: Detailed security architecture and trade-offs of the Istio-free access proxy.

### Automated Deployment Scripts
To streamline the entire installation, use the scripts in this directory:
- **[`deploy_standalone.sh`](deploy_standalone.sh)**: Automates API enablement, Gateway controller setup, `cert-manager` installation, core image builds, Certificate Manager setup (with automatic `sslip.io` fallback if you don't have a domain), IAP audience discovery, Kubeflow Trainer + Spark Operator installation, tenant RBAC, and GCS Workload Identity IAM bindings.
- **[`build_jupyterlab.sh`](build_jupyterlab.sh)**: Builds and pushes custom JupyterLab (CPU, GPU, TPU) and Spark 4.0.1 images (bundled with `examples/distributed_tpu_example.ipynb`) and registers the `jupyterlab` `WorkspaceKind` and GPU/TPU `ComputeClasses`.
- **[`cleanup_standalone.sh`](cleanup_standalone.sh)**: Cleanly tears down deployed resources.

---

## 1. Prerequisites & Cluster Setup

### Required Tools
Ensure the following tools are installed on your workstation:
- `gcloud` CLI with `gke-gcloud-auth-plugin`
- `kubectl` (v1.28+)
- `docker` (with `linux/amd64` build support)
- `go` (v1.25+)
- `jq`, `curl`, `sha256sum`, `envsubst` (from `gettext`)

### Create or Select a GKE Cluster
You need a VPC-native GKE cluster with **Dataplane V2** (`ADVANCED_DATAPATH`), **Workload Identity Federation for GKE**, **HTTP Load Balancing**, **GCE Persistent Disk CSI Driver**, and **Gateway API (`--gateway-api=standard`)** enabled.

If you do not have a cluster yet, create one using `gcloud`:

```bash
export PROJECT="your-gcp-project-id"
export CLUSTER="kubeflow-notebooks"
export LOCATION="us-central1-c"   # Zone or region where you have TPU / GPU quota
export REGION="us-central1"       # Region for Artifact Registry and GCS bucket

gcloud container clusters create "${CLUSTER}" \
  --project="${PROJECT}" \
  --location="${LOCATION}" \
  --enable-dataplane-v2 `# Required: Enforces Kubernetes NetworkPolicies` \
  --gateway-api=standard `# Required: Enables GKE Gateway API controller` \
  --workload-pool="${PROJECT}.svc.id.goog" `# Required: Enables Workload Identity for GCS access` \
  --workload-metadata=GKE_METADATA \
  --addons=HttpLoadBalancing,GcePersistentDiskCsiDriver,GcsFuseCsiDriver \
  --num-nodes=1 \
  --machine-type=e2-standard-4 \
  --enable-image-streaming

# Optional: Create an autoscaling CPU node pool for Spark executors and inference pods
gcloud container node-pools create cpu-autoscaling-pool \
  --cluster="${CLUSTER}" \
  --project="${PROJECT}" \
  --location="${LOCATION}" \
  --machine-type=e2-standard-4 \
  --enable-autoscaling \
  --min-nodes=0 \
  --max-nodes=3
```

### Set Environment Variables
Run all commands from the root of the repository (`gke-notebook/`). Export your deployment variables:

```bash
set -euo pipefail
export PROJECT="your-gcp-project-id"
export PROJECT_ID="${PROJECT}"
export CLUSTER="kubeflow-notebooks"
export LOCATION="us-central1-c"
export REGION="us-central1"
export PILOT_USERS="user1@example.com,user2@example.com" # Comma- or space-separated Google account emails of users
export TENANT_NAMESPACE="team-a"                  # Tenant namespace for notebooks & jobs
export REPOSITORY="notebooks"                     # Artifact Registry repository name
export ADDRESS_NAME="notebooks-gke-global"        # Global static external IP name
export CERTIFICATE_NAME="notebooks-gke"
export CERTIFICATE_MAP="notebooks-gke"
export CONTEXT="gke_${PROJECT}_${LOCATION}_${CLUSTER}"
export REGISTRY="${REGION}-docker.pkg.dev/${PROJECT}/${REPOSITORY}"
export TAG="pilot-$(date -u +%Y%m%d%H%M%S)"
export GCS_BUCKET="${TENANT_NAMESPACE}-bucket"    # Shared GCS bucket for distributed_tpu_example.ipynb
```

Authenticate `kubectl` to your cluster:

```bash
gcloud container clusters get-credentials "${CLUSTER}" \
  --location="${LOCATION}" \
  --project="${PROJECT}"
```

---

## 2. Enable GCP APIs, GKE Gateway Controller, & Cert-Manager

> [!IMPORTANT]
> You **must** enable the required Google Cloud APIs and enable `--gateway-api=standard` on your cluster **before** checking `kubectl get gatewayclasses` or installing Gateway resources.

### Step 2.1: Enable Google Cloud APIs
```bash
gcloud services enable \
  container.googleapis.com \
  compute.googleapis.com \
  artifactregistry.googleapis.com \
  certificatemanager.googleapis.com \
  iap.googleapis.com \
  --project="${PROJECT}"
```

### Step 2.2: Enable & Verify GKE Gateway API Controller
If you created an existing cluster without `--gateway-api=standard`, enable the standard Gateway API controller now and wait for the `gke-l7-global-external-managed` `GatewayClass` to become `Accepted`:

```bash
gcloud container clusters update "${CLUSTER}" \
  --location="${LOCATION}" \
  --project="${PROJECT}" \
  --gateway-api=standard \
  --quiet

# Wait for GKE to register and accept the global external GatewayClass
kubectl --context="${CONTEXT}" wait gatewayclass/gke-l7-global-external-managed \
  --for=condition=Accepted --timeout=10m

# Verify Gateway API and GKE policy CRDs are installed
kubectl --context="${CONTEXT}" get crd \
  gateways.gateway.networking.k8s.io \
  httproutes.gateway.networking.k8s.io \
  gcpbackendpolicies.networking.gke.io \
  healthcheckpolicies.networking.gke.io
```

### Step 2.3: Record Cluster Control Plane Endpoint
Inspect your cluster and record its verified control-plane IP address as `CONTROL_PLANE_CIDR`:

```bash
CONTROL_PLANE_IP=$(gcloud container clusters describe "${CLUSTER}" \
  --location="${LOCATION}" --project="${PROJECT}" \
  --format='value(privateClusterConfig.privateEndpoint,controlPlaneEndpointsConfig.ipEndpointsConfig.privateEndpoint)' | awk '{print $1}')
if [[ -z "${CONTROL_PLANE_IP}" ]]; then
  CONTROL_PLANE_IP=$(gcloud container clusters describe "${CLUSTER}" \
    --location="${LOCATION}" --project="${PROJECT}" \
    --format='value(endpoint)')
fi
export CONTROL_PLANE_CIDR="${CONTROL_PLANE_IP}/32"
echo "CONTROL_PLANE_CIDR: ${CONTROL_PLANE_CIDR}"
```

### Step 2.4: Create Artifact Registry Repository
Create the Docker repository in Artifact Registry if it does not exist:

```bash
gcloud artifacts repositories create "${REPOSITORY}" \
  --project="${PROJECT}" \
  --location="${REGION}" \
  --repository-format=docker || true
```

### Step 2.5: Install Cert-Manager (v1.21.2)
`cert-manager` issues internal TLS certificates for the admission webhooks used by Kubeflow Workspaces, Trainer, and Spark Operator. Install `cert-manager` v1.21.2 if not already present:

```bash
mkdir -p gke/bin
curl -fsSL https://github.com/cert-manager/cert-manager/releases/download/v1.21.2/cert-manager.yaml \
  -o gke/bin/cert-manager-v1.21.2.yaml
printf '%s  %s\n' e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f \
  gke/bin/cert-manager-v1.21.2.yaml | sha256sum --check
kubectl --context="${CONTEXT}" apply --server-side \
  --field-manager=notebooks-gke-platform -f gke/bin/cert-manager-v1.21.2.yaml

for component in cert-manager cert-manager-webhook cert-manager-cainjector; do
  kubectl --context="${CONTEXT}" -n cert-manager rollout status \
    deployment/"${component}" --timeout=5m
done
```

---

## 3. Build & Push Application & Custom JupyterLab/Spark Images

### Step 3.1: Build & Push Standalone Workspaces Core Images
Build and push the four standalone core images (`gke-access-proxy`, `gke-frontend`, `gke-controller`, `gke-backend`) and pin their registry digests:

```bash
make -C gke test
gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet

docker build --platform=linux/amd64 -t "${REGISTRY}/gke-access-proxy:${TAG}" gke
docker build --platform=linux/amd64 -f gke/frontend.Dockerfile \
  -t "${REGISTRY}/gke-frontend:${TAG}" .
docker build --platform=linux/amd64 -f workspaces/controller/Dockerfile \
  -t "${REGISTRY}/gke-controller:${TAG}" workspaces/controller
docker build --platform=linux/amd64 -f workspaces/backend/Dockerfile \
  -t "${REGISTRY}/gke-backend:${TAG}" workspaces

for component in access-proxy frontend controller backend; do
  docker push "${REGISTRY}/gke-${component}:${TAG}"
done
```

### Step 3.2: Build & Push Custom JupyterLab & Spark Images for `distributed_tpu_example.ipynb`
To run the distributed Spark ETL and Cloud TPU training workflow in [`examples/distributed_tpu_example.ipynb`](examples/distributed_tpu_example.ipynb), build the custom JupyterLab (CPU, GPU, TPU) and Spark 4.0.1 images using [`gke/build_jupyterlab.sh`](build_jupyterlab.sh):

```bash
PROJECT_ID="${PROJECT}" \
REGION="${REGION}" \
REPO_NAME="${REPOSITORY}" \
TENANT_NAMESPACE="${TENANT_NAMESPACE}" \
GCS_BUCKET="${GCS_BUCKET}" \
  bash gke/build_jupyterlab.sh
```
*(Tip: Add `--cloud-build` to build remotely via Google Cloud Build instead of local Docker.)*

---

## 4. Configuration Options: Domain, TLS Certificates, & Google Login (OAuth)

### Part A: Domain & TLS Certificate Configuration Options

The GKE Gateway serves HTTPS using a **Google Certificate Manager** certificate with **Load Balancer Authorization**. You can configure this with or without your own custom domain:

```mermaid
sequenceDiagram
    participant User as User Browser
    participant DNS as DNS Provider<br/>(sslip.io or Custom Domain)
    participant CM as Google Certificate Manager
    participant GW as GKE Gateway<br/>(gke-l7-global-external-managed)
    participant IAP as Identity-Aware Proxy (IAP)
    participant Proxy as gke-access-proxy

    Note over GW,CM: 1. Global External IP reserved & Certificate Map attached to Gateway
    User->>DNS: Resolve NOTEBOOK_HOST (e.g. notebooks.<IP>.sslip.io)
    DNS-->>User: Returns Global External IP (<ADDRESS>)
    CM->>GW: Verify Load Balancer Authorization via Global IP
    CM-->>GW: Activate Public TLS Certificate (ACTIVE)
    User->>GW: HTTPS GET https://<NOTEBOOK_HOST>/workspaces/
    GW->>IAP: Authenticate User via Google Login (OAuth)
    IAP->>Proxy: Forward request + signed x-goog-iap-jwt-assertion header
    Proxy->>Proxy: Verify IAP JWT signature & Kubernetes SubjectAccessReview RBAC
    Proxy-->>User: Render Kubeflow Workspaces UI / Connect to JupyterLab
```

#### Step 4A.1: Reserve Global Static External IPv4 Address
First, reserve a global external IPv4 address for the Gateway:

```bash
gcloud compute addresses create "${ADDRESS_NAME}" --global --ip-version=IPV4 \
  --network-tier=PREMIUM --project="${PROJECT}" || true

export ADDRESS=$(gcloud compute addresses describe "${ADDRESS_NAME}" \
  --global --project="${PROJECT}" --format='value(address)')
echo "Reserved Global External IP: ${ADDRESS}"
```

Now choose **Option A1 (No Domain)** or **Option A2 (Custom Domain)**:

---

#### Option A1: What To Do If You Do Not Have a Domain (`sslip.io` Zero-DNS Setup)
If you do not own a domain name or want an instant, zero-DNS setup, use **[sslip.io](https://sslip.io/)**. Any hostname of the form `notebooks.<IP>.sslip.io` automatically resolves to `<IP>` on public DNS without any configuration:

```bash
export NOTEBOOK_HOST="notebooks.${ADDRESS}.sslip.io"
echo "Using automatic sslip.io hostname: ${NOTEBOOK_HOST}"

# Verify public DNS resolution
getent ahostsv4 "${NOTEBOOK_HOST}"
```

*(Note: When running `./gke/deploy_standalone.sh`, if `NOTEBOOK_HOST` is left unset, the script automatically configures `notebooks.${ADDRESS}.sslip.io` for you.)*

---

#### Option A2: What To Do If You Have Your Own Custom Domain
If you own a custom domain (e.g., `notebooks.example.com`):
1. Set your desired hostname:
   ```bash
   export NOTEBOOK_HOST="notebooks.example.com"
   ```
2. In your DNS provider (Cloud DNS, Route 53, Cloudflare, etc.), create an **A record**:
   - **Name / Host**: `notebooks` (for `notebooks.example.com`)
   - **Type**: `A`
   - **Value**: `${ADDRESS}` (your reserved global external IP)
   - **TTL**: `300` seconds
   *(If using Cloudflare, set proxy status to **DNS only / grey cloud**).*
3. Verify public DNS resolves to `${ADDRESS}` before continuing:
   ```bash
   dig +short "${NOTEBOOK_HOST}"
   ```

---

#### Step 4A.2: Create Google Certificate Manager Certificate & Map Entry
Once `NOTEBOOK_HOST` is set (via either Option A1 or Option A2), create the Certificate Manager certificate and certificate map entry:

```bash
gcloud certificate-manager certificates create "${CERTIFICATE_NAME}" \
  --domains="${NOTEBOOK_HOST}" --project="${PROJECT}"

gcloud certificate-manager maps create "${CERTIFICATE_MAP}" --project="${PROJECT}"

gcloud certificate-manager maps entries create notebooks \
  --map="${CERTIFICATE_MAP}" --certificates="${CERTIFICATE_NAME}" \
  --hostname="${NOTEBOOK_HOST}" --project="${PROJECT}"
```
> [!NOTE]
> Because this certificate uses Load Balancer Authorization (no DNS challenge required), it will remain in `PROVISIONING` state until Step 5 creates the GKE Gateway and attaches the certificate map. Do **not** wait for `ACTIVE` before continuing to Step 5.

---

### Part B: Authentication Configuration Options (Google Login & IAP OAuth)

Identity-Aware Proxy (IAP) authenticates users with Google accounts. Choose **Option B1** or **Option B2** based on whether your users belong to your GCP organization:

| Accounts that will sign in | Recommended OAuth Configuration |
| --- | --- |
| **Google Workspace / Cloud Identity users within the GCP project's organization** | **Option B1: Google-Managed OAuth** (Zero OAuth client or secret needed) |
| **External Google accounts (`@gmail.com`) or users outside the GCP project's organization** | **Option B2: Custom OAuth Client** (Dedicated Web Application OAuth client + Secret) |

#### Option B1: Google-Managed OAuth (Users Within Your GCP Organization)
If the Google accounts signing in belong to the same Google Cloud Organization that owns `${PROJECT}`, leave both `IAP_CLIENT_ID` and `IAP_SECRET_NAME` empty:

```bash
export IAP_CLIENT_ID=""
export IAP_SECRET_NAME=""
```
*(Skip directly to Section 5.)*

#### Option B2: Custom OAuth Client (External / Cross-Organization Users)
Google-managed OAuth only allows users inside the project's organization. If your users are external (e.g., `@gmail.com` or from a different organization):
1. Open [Google Auth Platform Branding](https://console.cloud.google.com/auth/branding) in your project and configure the app name and support email.
2. If using External testing mode, add the emails in `${PILOT_USERS}` to the **Test users** list.
3. Open [Google Auth Platform Clients](https://console.cloud.google.com/auth/clients), create a **Web application** OAuth client named **Notebooks GKE**, and download the JSON file to a private location (e.g. `~/oauth-client.json`).
4. Edit the OAuth client in the Cloud Console and add the exact **Authorized redirect URI** (substituting your `CLIENT_ID`):
   ```
   https://iap.googleapis.com/v1/oauth/clientIds/CLIENT_ID:handleRedirect
   ```
5. Export `IAP_CLIENT_ID` and `IAP_SECRET_NAME`:
   ```bash
   export OAUTH_FILE="/absolute/private/path/oauth-client.json"
   chmod 600 "${OAUTH_FILE}"
   export IAP_CLIENT_ID=$(jq -er '.web.client_id' "${OAUTH_FILE}")
   export IAP_SECRET_NAME="iap-oauth"
   ```

---

## 5. Step-by-Step Deployment

> [!TIP]
> **Automated Execution**: You can execute all of Section 5 automatically by running:
> ```bash
> ./gke/deploy_standalone.sh
> ```
> Or follow the individual steps below to inspect and apply each stage manually.

### Step 5.1: Generate Deployment Plan & Apply Fail-Closed Bootstrap
Resolve the `@sha256:` digests of your built core images and generate the deployment configuration:

```bash
PROXY_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-access-proxy:${TAG}" \
  --project="${PROJECT}" --format='value(image_summary.fully_qualified_digest)')
FRONTEND_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-frontend:${TAG}" \
  --project="${PROJECT}" --format='value(image_summary.fully_qualified_digest)')
CONTROLLER_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-controller:${TAG}" \
  --project="${PROJECT}" --format='value(image_summary.fully_qualified_digest)')
BACKEND_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-backend:${TAG}" \
  --project="${PROJECT}" --format='value(image_summary.fully_qualified_digest)')

jq -n --arg cidr "${CONTROL_PLANE_CIDR}" --arg host "${NOTEBOOK_HOST}" \
  --arg certificateMap "${CERTIFICATE_MAP}" --arg addressName "${ADDRESS_NAME}" \
  --arg client "${IAP_CLIENT_ID}" --arg secret "${IAP_SECRET_NAME}" \
  --arg tenant "${TENANT_NAMESPACE}" \
  --arg proxy "${PROXY_IMAGE}" --arg frontend "${FRONTEND_IMAGE}" \
  --arg controller "${CONTROLLER_IMAGE}" --arg backend "${BACKEND_IMAGE}" \
  '{controlPlaneCIDR:$cidr,hostname:$host,certificateMap:$certificateMap,
    addressName:$addressName,iapClientID:$client,iapSecretName:$secret,
    iapAudience:"",tenants:[$tenant],
    images:{proxy:$proxy,frontend:$frontend,controller:$controller,backend:$backend}}' \
  > gke/deployment.local.json

rm -rf gke/rendered/bootstrap
make -C gke plan CONFIG=deployment.local.json OUTPUT=rendered/bootstrap
```

Apply namespaces and NetworkPolicy isolation rules first:

```bash
kubectl --context="${CONTEXT}" apply --server-side --field-manager=notebooks-gke \
  -f gke/rendered/bootstrap/namespaces.json
kubectl --context="${CONTEXT}" apply --server-side --field-manager=notebooks-gke \
  -f gke/rendered/bootstrap/isolation.json
```

If using **Option B2 (Custom OAuth)**, create the Kubernetes Secret containing the OAuth client secret:

```bash
if [[ -n "${OAUTH_FILE:-}" && -n "${IAP_SECRET_NAME:-}" ]]; then
  jq -jr '.web.client_secret' "${OAUTH_FILE}" | \
    kubectl --context="${CONTEXT}" -n kubeflow-workspaces create secret generic "${IAP_SECRET_NAME}" \
      --from-file=client_secret=/dev/stdin
fi
```

Apply CRDs, wait for them to establish, and apply the application and edge manifests:

```bash
jq '{apiVersion,kind,items:[.items[]|select(.kind=="CustomResourceDefinition")]}' \
  gke/rendered/bootstrap/applications.json | \
  kubectl --context="${CONTEXT}" apply --server-side --field-manager=notebooks-gke -f -

kubectl --context="${CONTEXT}" wait --for=condition=Established --timeout=2m \
  crd/workspaces.kubeflow.org crd/workspacekinds.kubeflow.org

kubectl --context="${CONTEXT}" apply --server-side --field-manager=notebooks-gke \
  -f gke/rendered/bootstrap/applications.json
kubectl --context="${CONTEXT}" apply --server-side --field-manager=notebooks-gke \
  -f gke/rendered/bootstrap/edge.json
```
*(Because `iapAudience` is empty during bootstrap, `gke-access-proxy` intentionally exits at startup until Step 5.2 configures the verified IAP audience.)*

---

### Step 5.2: Discover IAP Backend Audience & Finalize Access Proxy
Once GKE creates the global backend service for the proxy's Network Endpoint Group (NEG), discover its numeric backend ID and configure `IAP_AUDIENCE`:

```bash
# 1. Discover the proxy Service's NEG name
NEG_NAME=$(kubectl --context="${CONTEXT}" -n kubeflow-workspaces get service gke-access-proxy \
  -o json | jq -er '.metadata.annotations["cloud.google.com/neg-status"] | fromjson | .network_endpoint_groups["8080"]')

# 2. Match the exact GKE global backend service
MATCHED_BACKEND=$(gcloud compute backend-services list --global --project="${PROJECT}" \
  --format='json(name,id,backends,iap.enabled)' | jq -ce --arg neg "${NEG_NAME}" \
  '[.[] | select(any(.backends[]?; .group | endswith("/networkEndpointGroups/"+$neg)))]
   | if length==1 then .[0] else error("Expected exactly one proxy backend") end')

export BACKEND_SERVICE=$(jq -er '.name' <<< "${MATCHED_BACKEND}")
BACKEND_ID=$(jq -er '.id' <<< "${MATCHED_BACKEND}")
PROJECT_NUMBER=$(gcloud projects describe "${PROJECT}" --format='value(projectNumber)')
export IAP_AUDIENCE="/projects/${PROJECT_NUMBER}/global/backendServices/${BACKEND_ID}"
echo "Discovered IAP Backend: ${BACKEND_SERVICE} (Audience: ${IAP_AUDIENCE})"

# 3. Render ready plan with the verified IAP audience and update the deployment
rm -rf gke/rendered/ready
jq --arg audience "${IAP_AUDIENCE}" '.iapAudience=$audience' \
  gke/deployment.local.json > gke/rendered/deployment.ready.json
make -C gke plan CONFIG=rendered/deployment.ready.json OUTPUT=rendered/ready

kubectl --context="${CONTEXT}" apply --server-side --field-manager=notebooks-gke \
  -f gke/rendered/ready/applications.json
kubectl --context="${CONTEXT}" -n kubeflow-workspaces rollout restart deployment/gke-access-proxy

for component in workspaces-controller workspaces-backend workspaces-frontend gke-access-proxy; do
  kubectl --context="${CONTEXT}" -n kubeflow-workspaces rollout status deployment/"${component}" --timeout=5m
done
```

---

### Step 5.3: Deploy Kubeflow Trainer (v2) & Kubeflow Spark Operator (Standalone, No Istio)
To orchestrate distributed Apache Spark ETL jobs and multi-host Cloud TPU training jobs from inside your standalone Kubeflow Workspace, deploy **Kubeflow Trainer (v2)** and **Kubeflow Spark Operator** directly from [`kubeflow/community-distribution`](https://github.com/kubeflow/community-distribution) (neither component requires Istio):

```bash
export DIST_DIR="/tmp/kubeflow-community-distribution"
if [[ ! -d "${DIST_DIR}" ]]; then
  git clone https://github.com/kubeflow/community-distribution.git "${DIST_DIR}"
fi

# 1. Create Kubeflow core namespaces (kubeflow, kubeflow-system) and aggregated RBAC roles
# Note: We create kubeflow-system and kubeflow directly instead of applying
# ${DIST_DIR}/common/kubeflow-namespace/base because that directory adds Istio-specific
# namespace labels (istio-injection: enabled) and Istio-dependent NetworkPolicies.
kubectl --context="${CONTEXT}" create namespace kubeflow-system --dry-run=client -o yaml | kubectl --context="${CONTEXT}" apply -f -
kubectl --context="${CONTEXT}" create namespace kubeflow --dry-run=client -o yaml | kubectl --context="${CONTEXT}" apply -f -
kubectl --context="${CONTEXT}" apply -k "${DIST_DIR}/common/kubeflow-roles/base"

# 2. Deploy Kubeflow Trainer (v2) & JobSet controller + ClusterTrainingRuntimes (jax-distributed, torch-distributed)
kubectl --context="${CONTEXT}" apply -k "${DIST_DIR}/applications/trainer/overlays" --server-side --force-conflicts || true
kubectl --context="${CONTEXT}" wait --for=condition=Established crd/clustertrainingruntimes.trainer.kubeflow.org --timeout=60s
kubectl --context="${CONTEXT}" wait --for=condition=Established crd/trainingruntimes.trainer.kubeflow.org --timeout=60s
kubectl --context="${CONTEXT}" wait --for=condition=Established crd/trainjobs.trainer.kubeflow.org --timeout=60s
kubectl --context="${CONTEXT}" apply -k "${DIST_DIR}/applications/trainer/overlays" --server-side --force-conflicts
kubectl --context="${CONTEXT}" rollout status deployment/kubeflow-trainer-controller-manager -n kubeflow-system --timeout=180s
kubectl --context="${CONTEXT}" rollout status deployment/jobset-controller-manager -n kubeflow-system --timeout=180s

# 3. Deploy Kubeflow Spark Operator (SparkApplication & SparkConnect CRDs + controller/webhook)
kubectl --context="${CONTEXT}" apply -k "${DIST_DIR}/applications/spark/spark-operator/overlays/kubeflow" --server-side --force-conflicts
kubectl --context="${CONTEXT}" wait --for=condition=Established crd/sparkapplications.sparkoperator.k8s.io --timeout=60s
kubectl --context="${CONTEXT}" wait --for=condition=Established crd/scheduledsparkapplications.sparkoperator.k8s.io --timeout=60s
kubectl --context="${CONTEXT}" wait --for=condition=Established crd/sparkconnects.sparkoperator.k8s.io --timeout=60s
kubectl --context="${CONTEXT}" rollout status deployment/spark-operator-controller -n kubeflow --timeout=180s
kubectl --context="${CONTEXT}" rollout status deployment/spark-operator-webhook -n kubeflow --timeout=180s
```

Verify available cluster training runtimes:
```bash
kubectl --context="${CONTEXT}" get clustertrainingruntime
# Expected output includes: jax-distributed, torch-distributed, deepspeed-distributed, torchtune
```

---

### Step 5.4: Admit Users via IAP & Configure Tenant Workspace (`team-a`)
1. **Grant IAP Access to Your Users**:
   ```bash
   for user_email in $(echo "${PILOT_USERS}" | tr ',' ' '); do
     gcloud iap web add-iam-policy-binding --project="${PROJECT}" \
       --resource-type=backend-services --service="${BACKEND_SERVICE}" \
       --member="user:${user_email}" --role=roles/iap.httpsResourceAccessor --condition=None
   done
   ```

2. **Apply Tenant RBAC, ServiceAccount, StorageClass, and ValidatingAdmissionPolicy**:
   Render [`gke/manifests/pilot`](manifests/pilot/kustomization.yaml) with all users in `${PILOT_USERS}` added to the `RoleBinding` and `ClusterRoleBinding` subjects:
   ```bash
   kubectl kustomize --load-restrictor=LoadRestrictionsNone gke/manifests/pilot | \
     python3 -c 'import yaml, json, sys; print(json.dumps({"apiVersion": "v1", "kind": "List", "items": [d for d in yaml.safe_load_all(sys.stdin) if d]}))' | \
     jq --arg users "${PILOT_USERS}" --arg ns "${TENANT_NAMESPACE}" \
       '([ $users | split(",")[] | split(" ")[] | select(length > 0) | {kind: "User", name: ., apiGroup: "rbac.authorization.k8s.io"} ]) as $user_subjects |
        {apiVersion:"v1",kind:"List",items:(.items | map(
         (if .kind=="Namespace" then .metadata.name=$ns else . end) |
         (if .metadata.namespace != null then .metadata.namespace=$ns else . end) |
         (if .kind=="ValidatingAdmissionPolicy" then .spec.matchConstraints.namespaceSelector.matchLabels["kubernetes.io/metadata.name"]=$ns else . end) |
         (if .kind=="RoleBinding" or .kind=="ClusterRoleBinding" then
           .subjects = (
             (.subjects | map(
               select(.kind != "User") |
               (if .namespace != null then .namespace=$ns else . end) |
               (if .kind=="Group" and (.name | startswith("system:serviceaccounts:")) then .name=("system:serviceaccounts:"+$ns) else . end)
             )) + $user_subjects
           )
         else . end)
       ))}' > gke/rendered/ready/customer-pilot.json

   kubectl --context="${CONTEXT}" apply --server-side --field-manager=notebooks-gke-pilot \
     -f gke/rendered/ready/customer-pilot.json
   ```

3. **Register Custom `jupyterlab` WorkspaceKind & GPU/TPU ComputeClasses**:
   Register the `jupyterlab` `WorkspaceKind` (which includes CPU, GPU, and TPU image/pod options and injects `REGISTRY` and `GCS_BUCKET` into workspace pods) and the GKE `ComputeClass` definitions (`tpu-v5-8-multi-host`, `tpu-v5-4-single-host`, `gpu-l4-spot`, `gpu-t4-spot`):
   ```bash
   PROJECT_ID="${PROJECT}" \
   REGION="${REGION}" \
   REPO_NAME="${REPOSITORY}" \
   IMAGE_NAME="jupyterlab" \
   GCS_BUCKET="${GCS_BUCKET}" \
     envsubst < gke/jupyterlab/workspacekind.yaml | kubectl --context="${CONTEXT}" apply -f -

   kubectl --context="${CONTEXT}" apply -f gke/manifests/compute-classes/
   ```

---

### Step 5.5: Verify Gateway & Certificate Readiness
Confirm that the Gateway is `Programmed` and the Certificate Manager certificate is `ACTIVE`:

```bash
kubectl --context="${CONTEXT}" -n kubeflow-workspaces get \
  gateway,httproute,gcpbackendpolicy,healthcheckpolicy,certificate

gcloud certificate-manager certificates describe "${CERTIFICATE_NAME}" \
  --project="${PROJECT}" --format='yaml(managed)'
```
*(Note: Google Certificate Manager load-balancer authorization typically transitions from `PROVISIONING` to `ACTIVE` within 5–15 minutes after the Gateway is attached.)*

---

## 6. Running End-to-End Distributed Spark + TPU Example (`distributed_tpu_example.ipynb`)

The [`examples/distributed_tpu_example.ipynb`](examples/distributed_tpu_example.ipynb) notebook demonstrates how a lightweight CPU Kubeflow Workspace pod acts as the **single control plane** for an end-to-end distributed ML workflow on Kubernetes:

| Stage | Workload | Execution Target | Kubernetes API / SDK |
| --- | --- | --- | --- |
| **Stage 1: Distributed ETL** | Apache Spark Fashion-MNIST preprocessing & augmentation | 1 Driver + 4 Executor Pods | Kubeflow Spark SDK (`kubeflow.spark.SparkClient` / `SparkConnect`) |
| **Stage 2: Distributed Training** | Multi-host JAX data-parallel training (`pmap` + `pmean`) | 2 Cloud TPU v5e Hosts (8 TPU cores) | Kubeflow Trainer (`kubeflow.trainer.TrainerClient` / `TrainJob`) |
| **Stage 3: Model Serving** | CPU HTTP inference server (`/predict`) | 2-Replica CPU `Deployment` + `Service` | `apps/v1 Deployment` (`fashion-mnist-inference`) |

All three stages share data shards, model checkpoints, and metrics via a **Google Cloud Storage (GCS) bucket** (`gs://${GCS_BUCKET}`).

### Step 6.1: Provision GCS Bucket & Grant Workload Identity IAM Access
Because Workload Identity Federation (`--workload-pool=${PROJECT}.svc.id.goog`) is enabled on the cluster, you can grant GCS bucket access directly to all ServiceAccounts in `${TENANT_NAMESPACE}` (`team-a`) without managing service account keys:

```bash
PROJECT_NUMBER=$(gcloud projects describe "${PROJECT}" --format='value(projectNumber)')

# 1. Create the shared GCS bucket (if it does not exist yet)
gcloud storage buckets create "gs://${GCS_BUCKET}" \
  --location="${REGION}" \
  --project="${PROJECT}" || true

# 2. Grant roles/storage.objectUser to all pods/ServiceAccounts in namespace team-a
#    (covers the workspace pod, Spark driver/executors, TPU TrainJob hosts, and inference pods)
gcloud storage buckets add-iam-policy-binding "gs://${GCS_BUCKET}" \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT}.svc.id.goog/namespace/${TENANT_NAMESPACE}" \
  --role="roles/storage.objectUser"
```

### Step 6.2: Create & Connect to Your Workspace
1. Open `https://${NOTEBOOK_HOST}/workspaces/` in your browser and sign in with one of the Google accounts in `${PILOT_USERS}`.
2. Select tenant namespace **`team-a`** from the namespace dropdown.
3. Click **Create workspace**:
   - **Workspace Kind**: Choose **JupyterLab Notebook** (`jupyterlab`)
   - **Image**: Choose **jupyterlab (CPU)** (`jupyterlab-cpu`, pre-loaded with `kubeflow[spark]`, `kubeflow-trainer`, `google-cloud-storage`, and `examples/distributed_tpu_example.ipynb`)
   - **Pod Configuration**: Choose **Small CPU** (`small_cpu`, 1 CPU / 2 GiB RAM)
   - **Home Volume**: Attach a new PersistentVolumeClaim using StorageClass **`notebooks-gke-rwo`** (ReadWriteOnce, 10 GiB)
4. Click **Create**. Wait for the workspace status to reach **Running**, then click **Connect > JupyterLab**.

### Step 6.3: Execute the 3-Stage Distributed Workflow
Inside JupyterLab, open `distributed_tpu_example.ipynb` (located in `/home/jovyan/demo/distributed_tpu_example.ipynb` or upload [`examples/distributed_tpu_example.ipynb`](examples/distributed_tpu_example.ipynb) together with [`examples/jobs/`](examples/jobs/)) and run the cells in sequence:

1. **Cell 0 (Setup & RBAC Check)**:
   Confirms in-cluster ServiceAccount credentials and verifies `can-i create` permissions in `team-a` for `trainjobs.trainer.kubeflow.org`, `sparkconnects.sparkoperator.k8s.io`, `sparkapplications.sparkoperator.k8s.io`, and `deployments.apps`.
2. **Stage 1 (Distributed Data Processing with Apache Spark)**:
   Runs `pipeline.run_data_processing(num_executors=4, num_shards=4, wait=True)`.
   - Spins up a `SparkConnect` cluster (1 driver + 4 executor pods) in `team-a`.
   - Processes 60,000 Fashion-MNIST images in parallel and writes compressed `.npz` shards to `gs://${GCS_BUCKET}/processed/train/`.
3. **Stage 2 (Multi-Host Cloud TPU Training with Kubeflow Trainer)**:
   Runs `pipeline.run_training(num_hosts=2, epochs=5, global_batch_size=1024, wait=True)`.
   - Submits a `TrainJob` using the `jax-distributed` runtime on 2 Cloud TPU v5e hosts (`cloud.google.com/compute-class: tpu-v5-8-multi-host`, 8 TPU cores total).
   - Trains a 3-layer MLP with `jax.pmap` across all 8 TPU cores and writes model parameters and `metrics.json` to `gs://${GCS_BUCKET}/model/`.
4. **Stage 3 (CPU Model Serving & Inference)**:
   Deploys the 2-replica `fashion-mnist-inference` `Deployment` and `Service` in `team-a` and sends live HTTP prediction requests to `http://fashion-mnist-inference:8080/predict`.
5. **Stage 4 (Pause & Resume Workspace)**:
   Demonstrates pausing the workspace from the Kubeflow Workspaces UI to release compute resources while preserving persistent files on the GKE Persistent Disk (`notebooks-gke-rwo`), and resuming the workspace when ready.

You can monitor all spawned resources from your workstation terminal:
```bash
kubectl --context="${CONTEXT}" get workspaces,sparkconnects,trainjobs,jobsets,deployments,pods -n "${TENANT_NAMESPACE}"
```

---

## 7. VS Code Jupyter Extension (Desktop Endpoint)

The optional desktop endpoint allows standard VS Code (`ms-toolsai.jupyter`) to connect to running workspaces using Kubernetes-minted connection tokens. IAP continues to protect browser access and token issuance, while a separate desktop backend validates short-lived tokens without exposing dashboard APIs.

### Step 7.1: Enable the Desktop Endpoint
Choose a second, dedicated DNS name (e.g., `connect.example.com` or `connect.${ADDRESS}.sslip.io`) pointing at the same global external IP `${ADDRESS}`, create a Certificate Manager certificate and map entry, and render the desktop plan:

```bash
export DESKTOP_HOST="connect.${ADDRESS}.sslip.io" # Or connect.YOUR_DOMAIN
export DESKTOP_CERTIFICATE="notebooks-desktop"

gcloud certificate-manager certificates create "${DESKTOP_CERTIFICATE}" \
  --domains="${DESKTOP_HOST}" --project="${PROJECT}"
gcloud certificate-manager maps entries create notebooks-desktop \
  --map="${CERTIFICATE_MAP}" --certificates="${DESKTOP_CERTIFICATE}" \
  --hostname="${DESKTOP_HOST}" --project="${PROJECT}"

jq --arg host "${DESKTOP_HOST}" '.desktopHostname=$host' \
  gke/rendered/deployment.ready.json > gke/rendered/deployment.desktop.json
make -C gke plan CONFIG=rendered/deployment.desktop.json OUTPUT=rendered/desktop

for stage in namespaces isolation applications edge; do
  kubectl --context="${CONTEXT}" apply --server-side --field-manager=notebooks-gke \
    -f "gke/rendered/desktop/${stage}.json"
done

kubectl --context="${CONTEXT}" -n kubeflow-workspaces rollout restart deployment/gke-access-proxy
kubectl --context="${CONTEXT}" -n kubeflow-workspaces rollout status deployment/gke-access-proxy --timeout=5m
```

### Step 7.2: Connect from VS Code
1. Open `https://${NOTEBOOK_HOST}/workspaces/connections` in your browser and sign in with Google.
2. Select your running workspace, port (`jupyterlab`), and duration, then click **Generate connection** and **Copy URL**.
3. In VS Code, open any `.ipynb` notebook and select **Select Kernel > Select Another Kernel > Existing Jupyter Server**, then paste the copied URL (including `?token=`).
4. When finished, click **Revoke** on the connection page to immediately terminate active desktop WebSockets and invalidate the token.

---

## 8. Enrolling Additional Users

IAP admission and Kubernetes RBAC are configured as independent layers:
1. **Grant IAP Admission (Google Group or Individual User)**:
   ```bash
   export NEW_USER="colleague@example.com"
   gcloud iap web add-iam-policy-binding --project="${PROJECT}" \
     --resource-type=backend-services --service="${BACKEND_SERVICE}" \
     --member="user:${NEW_USER}" --role=roles/iap.httpsResourceAccessor --condition=None
   ```
2. **Grant Kubernetes RBAC in Tenant Namespace (`team-a`)**:
   ```bash
   kubectl --context="${CONTEXT}" -n "${TENANT_NAMESPACE}" patch rolebinding notebooks-gke-pilot --type=json \
     -p '[{"op":"add","path":"/subjects/-","value":{"kind":"User","apiGroup":"rbac.authorization.k8s.io","name":"'"${NEW_USER}"'"}}]'
   kubectl --context="${CONTEXT}" patch clusterrolebinding notebooks-gke-pilot-discovery --type=json \
     -p '[{"op":"add","path":"/subjects/-","value":{"kind":"User","apiGroup":"rbac.authorization.k8s.io","name":"'"${NEW_USER}"'"}}]'
   ```

---

## 9. Teardown & Cleanup

To remove all deployed components cleanly using the automated cleanup script:

```bash
./gke/cleanup_standalone.sh
```
To also delete the global external IP address and Google Certificate Manager certificate map:
```bash
DELETE_EDGE_RESOURCES=true ./gke/cleanup_standalone.sh
```

---

## 10. Troubleshooting

| Symptom | Resolution |
| --- | --- |
| `kubectl get gatewayclasses` shows no `gke-l7-global-external-managed` | Ensure you ran `gcloud container clusters update "$CLUSTER" --location="$LOCATION" --gateway-api=standard` first (Section 2.2). |
| Google login succeeds but IAP returns `Access Denied` | Verify `gcloud iap web add-iam-policy-binding` was granted on `--service="${BACKEND_SERVICE}"` and that external users are added to Google Auth Platform Test Users if using Custom OAuth. |
| Certificate Manager status stays `PROVISIONING` | Ensure the Gateway is `Programmed`, `NOTEBOOK_HOST` resolves to `${ADDRESS}`, and wait 5–15 minutes for Load Balancer Authorization. |
| `gke-access-proxy` CrashLoopBackOff during Step 5.1 | Expected fail-closed bootstrap behavior before `IAP_AUDIENCE` is configured in Step 5.2. |
| Spark or TPU pods fail to create in `team-a` | Verify `gke/manifests/pilot/access.yaml` has been applied (grants RBAC on `sparkoperator.k8s.io` and `trainer.kubeflow.org` and configures baseline Pod Security and expanded `ResourceQuota`). |
| GCS permission denied (`403`) during Spark ETL or TPU training | Verify Workload Identity IAM binding on `gs://${GCS_BUCKET}` for `principalSet://.../namespace/${TENANT_NAMESPACE}` (Section 6.1). |
