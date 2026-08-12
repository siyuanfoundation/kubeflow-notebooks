#!/usr/bin/env bash

# Build and Deploy Script for Kubeflow Workspaces + Kubeflow Trainer on GKE
# This script builds all custom container images and deploys the entire stack from scratch onto a GKE cluster.

set -euo pipefail

# Configurable environment variables
export REGISTRY="${REGISTRY:-us-west1-docker.pkg.dev/sizhang-gke-dev/sizhang-repo}"
export TAG="${TAG:-local-gke-dev}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NOTEBOOKS_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${NOTEBOOKS_DIR}/.." && pwd)"
COMMUNITY_DIST_DIR="${REPO_ROOT}/kubeflow-community-distribution"

echo "=========================================================================="
echo " Starting Build and Deployment for GKE Cluster"
echo " Registry: ${REGISTRY}"
echo " Image Tag: ${TAG}"
echo "=========================================================================="

# Step 1: Configure Docker Auth
echo "=== 1. Authenticating to Artifact Registry ==="
REGISTRY_HOST="$(echo "${REGISTRY}" | cut -d'/' -f1)"
gcloud auth configure-docker "${REGISTRY_HOST}" --quiet

# Step 2: Build and Push Container Images
echo "=== 2. Building and Pushing Container Images ==="

echo "--> Workspaces Controller..."
cd "${NOTEBOOKS_DIR}/workspaces/controller"
make docker-build docker-push REGISTRY="${REGISTRY}" TAG="${TAG}" IMG="${REGISTRY}/workspaces-controller:${TAG}"

echo "--> Workspaces Backend..."
cd "${NOTEBOOKS_DIR}/workspaces/backend"
make docker-build docker-push REGISTRY="${REGISTRY}" TAG="${TAG}" IMG="${REGISTRY}/workspaces-backend:${TAG}"

echo "--> Workspaces Frontend (standalone mode)..."
cd "${NOTEBOOKS_DIR}/workspaces/frontend"
make docker-build docker-push REGISTRY="${REGISTRY}" TAG="${TAG}" IMG="${REGISTRY}/workspaces-frontend:${TAG}" DEPLOYMENT_MODE="standalone"

echo "--> Jupyter TPU Notebook (with Kubeflow & Kubernetes SDKs)..."
cd "${SCRIPT_DIR}"
docker build -t "${REGISTRY}/jupyter-tpu-notebook:${TAG}" -f jupyter-tpu.Dockerfile .
docker push "${REGISTRY}/jupyter-tpu-notebook:${TAG}"

# Step 3: Create Core System Namespaces
echo "=== 3. Creating System Namespaces ==="
kubectl create namespace kubeflow-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace kubeflow-workspaces --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace jobset-system --dry-run=client -o yaml | kubectl apply -f -

# Step 3.5: Enable Filestore CSI Driver (for RWX shared storage in demos)
echo "=== 3.5. Ensuring GKE Filestore CSI Driver is enabled ==="
# We query the current context to identify the cluster name, location, and project.
CURRENT_CONTEXT="$(kubectl config current-context)"
# Format: gke_PROJECT_LOCATION_CLUSTER
IFS='_' read -ra ADDR <<< "${CURRENT_CONTEXT}"
if [[ "${ADDR[0]}" == "gke" ]]; then
  PROJECT="${ADDR[1]}"
  LOCATION="${ADDR[2]}"
  CLUSTER_NAME="${ADDR[3]}"

  echo "Cluster: ${CLUSTER_NAME}, Location: ${LOCATION}, Project: ${PROJECT}"
  if gcloud container clusters describe "${CLUSTER_NAME}" --location "${LOCATION}" --project "${PROJECT}" \
    --format="value(addonsConfig.gcpFilestoreCsiDriverConfig.enabled)" | grep -qi "True"; then
    echo "Filestore CSI Driver is already enabled."
  else
    echo "Enabling Filestore CSI Driver (this may take a few minutes)..."
    gcloud container clusters update "${CLUSTER_NAME}" --location "${LOCATION}" --project "${PROJECT}" \
      --update-addons=GcpFilestoreCsiDriver=ENABLED --quiet || echo "WARNING: Failed to enable Filestore CSI Driver. Continuing..."
  fi
else
  echo "WARNING: Could not parse GKE cluster info from context '${CURRENT_CONTEXT}'. Skipping Filestore enablement."
  echo "If this is a GKE cluster, ensure GcpFilestoreCsiDriver is enabled manually."
fi

# Step 4: Install Core Infrastructure (Cert-Manager & Istio)
echo "=== 4. Deploying Core Infrastructure (Cert-Manager & Istio) ==="
cd "${NOTEBOOKS_DIR}"
./developing/scripts/setup-cert-manager.sh

echo "Waiting for cert-manager webhook CA injection..."
kubectl wait --timeout=60s --for='jsonpath={.webhooks[0].clientConfig.caBundle}' validatingwebhookconfiguration/cert-manager-webhook

./developing/scripts/setup-istio.sh
kubectl apply -k developing/manifests/istio-gateway

# Step 5: Install Kubeflow Trainer v2
echo "=== 5. Deploying Kubeflow Trainer v2 ==="
cd "${COMMUNITY_DIST_DIR}/applications/trainer"
kustomize build upstream/base/crds | kubectl apply --server-side --force-conflicts -f -
kubectl wait --for condition=established crd/trainjobs.trainer.kubeflow.org --timeout=60s

# NOTE: The `overlays` build bundles the ClusterTrainingRuntime resources (via
# kubeflow-platform -> overlays/runtimes). Applying those here would invoke the
# Trainer validating webhook before the controller is running, causing the
# first-run error: "no endpoints available for service
# kubeflow-trainer-controller-manager". So we deploy the controller stack now
# WITHOUT the runtimes, and apply the runtimes later (once the webhook is
# confirmed ready). We split the manifest on the YAML document separator and
# skip any document whose kind is (Cluster)TrainingRuntime, using awk so no
# extra tooling (yq) is required.
kustomize build overlays \
  | awk 'BEGIN{RS="\n---\n"} !/\nkind: (Cluster)?TrainingRuntime\n/ {printf "%s%s", sep, $0; sep="\n---\n"}' \
  | kubectl apply --server-side --force-conflicts -f -

# Create or reset jobset webhook secret if empty to ensure cert rotation works smoothly
kubectl create secret generic jobset-webhook-server-cert -n kubeflow-system --dry-run=client -o yaml | kubectl apply -f -
kubectl patch deployment jobset-controller-manager -n kubeflow-system --type=json -p='[{"op": "replace", "path": "/spec/template/spec/containers/0/resources/requests/cpu", "value": "100m"}]' || true

echo "Waiting for Trainer & JobSet controllers..."
kubectl rollout status deployment/kubeflow-trainer-controller-manager -n kubeflow-system --timeout=300s
kubectl rollout status deployment/jobset-controller-manager -n kubeflow-system --timeout=300s

# Applying the runtimes triggers the Trainer validating webhook
# (validator.clustertrainingruntime.trainer.kubeflow.org). A successful
# `rollout status` does NOT guarantee the controller's Service has ready
# endpoints or that the webhook TLS server is accepting connections yet, which
# intermittently causes: "no endpoints available for service
# kubeflow-trainer-controller-manager". Wait for the Service to report ready
# endpoints, then retry the apply to absorb any remaining transient errors.
echo "Waiting for Trainer webhook Service endpoints to be ready..."
for i in {1..30}; do
  ENDPOINTS="$(kubectl get endpoints kubeflow-trainer-controller-manager -n kubeflow-system -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || echo "")"
  if [ -n "${ENDPOINTS}" ]; then
    echo "Trainer webhook endpoints ready: ${ENDPOINTS}"
    break
  fi
  echo "  (attempt ${i}/30) no endpoints yet, retrying in 5s..."
  sleep 5
done

echo "Applying Trainer runtimes (with retry for webhook readiness)..."
for i in {1..12}; do
  if kustomize build upstream/overlays/runtimes | kubectl apply --server-side --force-conflicts -f -; then
    echo "Trainer runtimes applied successfully."
    break
  fi
  if [ "${i}" -eq 12 ]; then
    echo "ERROR: Failed to apply Trainer runtimes after multiple attempts." >&2
    exit 1
  fi
  echo "  (attempt ${i}/12) webhook not ready yet, retrying in 10s..."
  sleep 10
done

kubectl apply -f upstream/overlays/kubeflow-platform/kubeflow-trainer-roles.yaml

# Step 5.5: Install Kubeflow Spark Operator (for the Stage 1 data-processing demo)
echo "=== 5.5. Deploying Kubeflow Spark Operator ==="
# The Spark Operator overlay places all control-plane resources in the
# `kubeflow` namespace and watches all namespaces for SparkApplications
# (--namespaces=""). Create the namespace first, then apply the standalone
# overlay. Applying twice absorbs the transient race where the operator's
# CRDs are still being established before the webhook config is applied.
kubectl create namespace kubeflow --dry-run=client -o yaml | kubectl apply -f -
for i in {1..5}; do
  if kustomize build "${COMMUNITY_DIST_DIR}/applications/spark/spark-operator/overlays/standalone" \
    | kubectl apply --server-side --force-conflicts -f -; then
    echo "Spark Operator manifests applied."
    break
  fi
  if [ "${i}" -eq 5 ]; then
    echo "ERROR: Failed to apply Spark Operator manifests." >&2
    exit 1
  fi
  echo "  (attempt ${i}/5) retrying Spark Operator apply in 10s..."
  sleep 10
done

echo "Waiting for Spark Operator controller & webhook..."
kubectl rollout status deployment/spark-operator-controller -n kubeflow --timeout=180s
kubectl rollout status deployment/spark-operator-webhook -n kubeflow --timeout=180s

# Step 6: Deploy Kubeflow Workspaces Stack & Patch Images
echo "=== 6. Deploying Kubeflow Workspaces Stack ==="
cd "${NOTEBOOKS_DIR}/workspaces/controller"
make deploy REGISTRY="ghcr.io/kubeflow/notebooks" TAG="${TAG}" IMG="${REGISTRY}/workspaces-controller:${TAG}"

cd "${NOTEBOOKS_DIR}/workspaces/backend"
make deploy REGISTRY="ghcr.io/kubeflow/notebooks" TAG="${TAG}" IMG="${REGISTRY}/workspaces-backend:${TAG}"

cd "${NOTEBOOKS_DIR}/workspaces/frontend"
make deploy REGISTRY="ghcr.io/kubeflow/notebooks" TAG="${TAG}" IMG="${REGISTRY}/workspaces-frontend:${TAG}"

echo "Updating Workspaces deployments to use pushed images..."
# Resolve each freshly-pushed image to its immutable digest so that re-runs of
# this script always pull the newly built image. Without this, the deployments
# use a fixed tag with imagePullPolicy=IfNotPresent, so nodes keep serving a
# stale image cached under the same tag (this previously caused the frontend to
# run in the wrong deployment mode).
CONTROLLER_DIGEST="$(docker inspect --format='{{index .RepoDigests 0}}' "${REGISTRY}/workspaces-controller:${TAG}")"
BACKEND_DIGEST="$(docker inspect --format='{{index .RepoDigests 0}}' "${REGISTRY}/workspaces-backend:${TAG}")"
FRONTEND_DIGEST="$(docker inspect --format='{{index .RepoDigests 0}}' "${REGISTRY}/workspaces-frontend:${TAG}")"

kubectl set image deployment/workspaces-controller manager="${CONTROLLER_DIGEST}" -n kubeflow-workspaces
kubectl set image deployment/workspaces-backend workspaces-backend="${BACKEND_DIGEST}" -n kubeflow-workspaces
kubectl set image deployment/workspaces-frontend workspaces-frontend="${FRONTEND_DIGEST}" -n kubeflow-workspaces

echo "Waiting for Workspaces system pods to become ready..."
kubectl wait --for=condition=Available deployment/workspaces-controller -n kubeflow-workspaces --timeout=300s
kubectl wait --for=condition=Available deployment/workspaces-backend -n kubeflow-workspaces --timeout=300s
kubectl wait --for=condition=Available deployment/workspaces-frontend -n kubeflow-workspaces --timeout=300s

# Step 7: Configure Standalone Auth & RBAC
echo "=== 7. Configuring Standalone Auth & RBAC ==="
kubectl create serviceaccount default-editor -n default --dry-run=client -o yaml | kubectl apply -f -
cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: dev-admin-cluster-admin
subjects:
- kind: User
  name: admin
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: cluster-admin
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: default-sa-cluster-admin
subjects:
- kind: ServiceAccount
  name: default
  namespace: default
- kind: ServiceAccount
  name: default-editor
  namespace: default
roleRef:
  kind: ClusterRole
  name: cluster-admin
  apiGroup: rbac.authorization.k8s.io
EOF

kubectl patch virtualservice workspaces-backend -n kubeflow-workspaces --type=json \
  -p='[{"op": "add", "path": "/spec/http/0/headers", "value": {"request": {"set": {"kubeflow-userid": "admin", "kubeflow-groups": "admin"}}}}]'

# Step 7.5: Configure Workload Identity for GCS access
echo "=== 7.5. Configuring Workload Identity for GCS access ==="
# If the cluster has Workload Identity enabled, we need to bind the K8s SA to a GSA.
# Format: gke_PROJECT_LOCATION_CLUSTER
IFS='_' read -ra ADDR <<< "$(kubectl config current-context)"
if [[ "${ADDR[0]}" == "gke" ]]; then
  PROJECT="${ADDR[1]}"
  
  # Find the Compute Engine default service account email
  GSA_EMAIL=$(gcloud iam service-accounts list --project "${PROJECT}" \
    --filter="displayName:'Compute Engine default service account'" \
    --format="value(email)" | head -n1)
  
  if [[ -n "${GSA_EMAIL}" ]]; then
    echo "Binding K8s SA 'default-editor' to GSA '${GSA_EMAIL}'..."
    
    # 1. Allow K8s SA to impersonate GSA
    gcloud iam service-accounts add-iam-policy-binding "${GSA_EMAIL}" \
      --project "${PROJECT}" \
      --role "roles/iam.workloadIdentityUser" \
      --member "serviceAccount:${PROJECT}.svc.id.goog[default/default-editor]" \
      --quiet
    
    # 2. Annotate K8s SA
    kubectl annotate serviceaccount default-editor -n default \
      iam.gke.io/gsa-email="${GSA_EMAIL}" --overwrite
    
    # 3. Ensure GSA has storage.admin role on project
    gcloud projects add-iam-policy-binding "${PROJECT}" \
      --member="serviceAccount:${GSA_EMAIL}" \
      --role="roles/storage.admin" \
      --quiet
    
    echo "Workload Identity configured for default-editor."
  else
    echo "WARNING: Could not find Compute Engine default service account. GCSFuse might fail."
  fi
fi

echo "Configuring default StorageClasses..."
kubectl label storageclass standard-rwo notebooks.kubeflow.org/can-use=true --overwrite=true || true
kubectl label storageclass standard notebooks.kubeflow.org/can-use=true --overwrite=true || true

# Step 8: Register WorkspaceKinds & PodSnapshot Storage Configs
echo "=== 8. Applying WorkspaceKind Templates & Snapshot Configurations ==="
cd "${NOTEBOOKS_DIR}"
kubectl apply -k workspaces/controller/manifests/kustomize/samples/common
kubectl apply -f workspaces/controller/manifests/kustomize/samples/codeserver_v1beta1_workspacekind.yaml
kubectl apply -f workspaces/controller/manifests/kustomize/samples/rstudio_v1beta1_workspacekind.yaml

# Apply PodSnapshot storage config and Jupyter IPC configmap for snapshot/restore
kubectl apply -f "${SCRIPT_DIR}/pod-snapshot-storage-config.yaml"
kubectl apply -f "${SCRIPT_DIR}/jupyter_ipc_configmap.yaml"

# Substitute custom image placeholder in jupyterlab_workspacekind.yaml and apply
sed "s|JUPYTER_TPU_IMAGE_PLACEHOLDER|${REGISTRY}/jupyter-tpu-notebook:${TAG}|g" "${SCRIPT_DIR}/jupyterlab_workspacekind.yaml" | kubectl apply -f -

# Step 9: Apply TPU Resources & Sample Workspace
echo "=== 9. Applying TPU ComputeClass, Reservation Job & Sample Workspace ==="
kubectl apply -f "${SCRIPT_DIR}/tpu-compute-class.yaml"
kubectl apply -f "${SCRIPT_DIR}/tpu-job-ccc.yaml"
kubectl apply -f "${SCRIPT_DIR}/workspace_tpu_notebook.yaml"

# Step 10: Retrieve Gateway Ingress IP and Show Dashboard URL
echo "=== 10. Retrieving Gateway Ingress IP & Verifying Dashboard ==="
INGRESS_IP=""
echo "Waiting for External IP to be provisioned for Ingress Gateway..."
for i in {1..30}; do
  INGRESS_IP=$(kubectl get svc istio-ingressgateway -n istio-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")
  if [ -n "$INGRESS_IP" ]; then
    break
  fi
  sleep 10
done

if [ -z "$INGRESS_IP" ]; then
  INGRESS_IP=$(kubectl get svc istio-ingressgateway -n istio-system -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || echo "")
fi

if [ -z "$INGRESS_IP" ]; then
  echo "WARNING: Ingress External IP/Hostname could not be retrieved yet. You can find it later using:"
  echo "  kubectl get svc istio-ingressgateway -n istio-system"
else
  echo "Ingress External IP: $INGRESS_IP"
  echo "Dashboard URL: https://${INGRESS_IP}/workspaces/"

  echo "Verifying Dashboard reachability via HTTP request..."
  HTTP_STATUS=$(curl -k -s -o /dev/null -w "%{http_code}" "https://${INGRESS_IP}/workspaces/")
  echo "HTTP Status Response: $HTTP_STATUS"
  if [[ "$HTTP_STATUS" == "200" || "$HTTP_STATUS" == "301" || "$HTTP_STATUS" == "302" ]]; then
    echo "SUCCESS: Dashboard is reachable at https://${INGRESS_IP}/workspaces/"
  else
    echo "WARNING: Received HTTP Status $HTTP_STATUS. Please check ingress gateway logs or routing configuration."
  fi
fi

echo "=========================================================================="
echo " Deployment Completed Successfully!"
echo "=========================================================================="
