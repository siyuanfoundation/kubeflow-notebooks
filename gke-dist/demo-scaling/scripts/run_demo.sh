#!/usr/bin/env bash
# =============================================================================
# run_demo.sh — set up the end-to-end ML scaling demo on an already-deployed cluster
# =============================================================================
# This script:
#   1. Ensures the shared GCS bucket exists and is clean.
#   2. Applies the ComputeClass definitions to ensure TPU and GPU scheduling works.
#   3. Patches the jupyterlab WorkspaceKind to include the tpu_single_host option,
#      dynamically pulling the correct custom Jupyter TPU image currently in use.
#   4. Creates the scaling demo Jupyter Workspace (with a single-host TPU attached).
#   5. Copies the demo files (notebook + jobs/) into the notebook pod.
#   6. Prints the Dashboard URL so you can open the notebook and run it.
# -----------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
PARENT_DIR="$(cd "${DEMO_DIR}/.." && pwd)"
NAMESPACE="${NAMESPACE:-default}"

# Use project ID from current kubectl context to make the bucket name unique
CURRENT_CONTEXT="$(kubectl config current-context)"
IFS='_' read -ra ADDR <<< "${CURRENT_CONTEXT}"
if [[ "${ADDR[0]}" == "gke" ]]; then
  PROJECT_ID="${ADDR[1]}"
else
  PROJECT_ID=$(gcloud config get-value project 2>/dev/null || echo "sizhang-gke-dev")
fi
DEMO_BUCKET="${DEMO_BUCKET:-${PROJECT_ID}-ml-scaling-demo-data}"

echo "=== 1. Ensuring GCS bucket exists and is clean ==="
if ! gcloud storage buckets describe "gs://${DEMO_BUCKET}" --project="${PROJECT_ID}" &>/dev/null; then
  echo "Creating bucket gs://${DEMO_BUCKET}..."
  gcloud storage buckets create "gs://${DEMO_BUCKET}" --project="${PROJECT_ID}" --location=us-west1 --quiet
else
  echo "Bucket gs://${DEMO_BUCKET} already exists. Cleaning existing bucket contents..."
  gcloud storage rm --recursive "gs://${DEMO_BUCKET}/**" --quiet 2>/dev/null || true
fi

GSA_EMAIL=$(gcloud iam service-accounts list --project "${PROJECT_ID}" \
  --filter="displayName:'Compute Engine default service account'" \
  --format="value(email)" | head -n1 2>/dev/null || echo "")
if [[ -n "${GSA_EMAIL}" ]]; then
  gcloud storage buckets add-iam-policy-binding "gs://${DEMO_BUCKET}" \
    --member="serviceAccount:${GSA_EMAIL}" --role="roles/storage.admin" --quiet 2>/dev/null || true
fi

PROJECT_NUM=$(gcloud projects describe "${PROJECT_ID}" --format="value(projectNumber)" 2>/dev/null || echo "")
if [[ -n "${PROJECT_NUM}" ]]; then
  gcloud storage buckets add-iam-policy-binding "gs://${DEMO_BUCKET}" \
    --member="principal://iam.googleapis.com/projects/${PROJECT_NUM}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/${NAMESPACE}/sa/default-editor" \
    --role="roles/storage.admin" --quiet 2>/dev/null || true
  gcloud storage buckets add-iam-policy-binding "gs://${DEMO_BUCKET}" \
    --member="principal://iam.googleapis.com/projects/${PROJECT_NUM}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/${NAMESPACE}/sa/default" \
    --role="roles/storage.admin" --quiet 2>/dev/null || true
fi


echo "=== 2. Making sure TPU and GPU ComputeClasses are applied ==="
if [ -f "${PARENT_DIR}/tpu-compute-class.yaml" ]; then
  kubectl apply -f "${PARENT_DIR}/tpu-compute-class.yaml"
fi
if [ -f "${PARENT_DIR}/gpu-compute-class.yaml" ]; then
  kubectl apply -f "${PARENT_DIR}/gpu-compute-class.yaml"
fi


echo "=== 3. Updating WorkspaceKind with TPU options ==="
# Dynamically fetch image from existing jupyterlab WorkspaceKind
IMAGE_NAME=$(kubectl get workspacekind jupyterlab -o jsonpath='{.spec.podTemplate.options.imageConfig.values[?(@.id=="jupyter-tpu-custom")].spec.image}' 2>/dev/null || echo "")
if [ -z "${IMAGE_NAME}" ]; then
  REGISTRY="${REGISTRY:-us-west1-docker.pkg.dev/sizhang-gke-dev/sizhang-repo}"
  TAG="${TAG:-local-gke-dev}"
  IMAGE_NAME="${REGISTRY}/jupyter-tpu-notebook:${TAG}"
fi
echo "Using custom Jupyter TPU image: ${IMAGE_NAME}"

sed "s|JUPYTER_TPU_IMAGE_PLACEHOLDER|${IMAGE_NAME}|g" "${DEMO_DIR}/manifests/jupyterlab_workspacekind_scaling.yaml" | kubectl apply -f -


echo "=== 4. Creating the scaling demo Jupyter Workspace ==="
if ! kubectl get workspace ml-scaling-demo-notebook -n "${NAMESPACE}" &>/dev/null; then
  kubectl apply -f "${DEMO_DIR}/manifests/demo-workspace-scaling.yaml"
else
  echo "Workspace ml-scaling-demo-notebook already exists."
fi


echo "=== 5. Waiting for the notebook pod to become ready ==="
POD=""
for i in {1..90}; do
  POD="$(kubectl get pods -n "${NAMESPACE}" -l notebooks.kubeflow.org/workspace-name=ml-scaling-demo-notebook -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")"
  if [ -n "${POD}" ]; then
    PHASE="$(kubectl get pod "${POD}" -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")"
    if [ "${PHASE}" = "Running" ]; then
      echo "Notebook pod ready: ${POD}"
      break
    fi
  fi
  echo "  (attempt ${i}/90) waiting for notebook pod..."
  sleep 5
done

if [ -z "${POD}" ]; then
  echo "ERROR: notebook pod did not become ready in time." >&2
  exit 1
fi


echo "=== 6. Copying demo files into the notebook pod ==="
kubectl exec "${POD}" -n "${NAMESPACE}" -c main -- mkdir -p /home/jovyan/demo/jobs
kubectl cp "${DEMO_DIR}/manifests" \
  "${NAMESPACE}/${POD}:/home/jovyan/demo/manifests" -c main

# Copy jobs files, substituting bucket in pipeline.py
for f in __init__.py pipeline.py data_processing.py train.py serve.py; do
  if [ "$f" = "pipeline.py" ]; then
    TMP_FILE=$(mktemp)
    sed "s|sizhang-gke-dev-ml-demo-data|${DEMO_BUCKET}|g" "${DEMO_DIR}/jobs/${f}" > "${TMP_FILE}"
    kubectl cp "${TMP_FILE}" "${NAMESPACE}/${POD}:/home/jovyan/demo/jobs/${f}" -c main
    rm "${TMP_FILE}"
  else
    kubectl cp "${DEMO_DIR}/jobs/${f}" "${NAMESPACE}/${POD}:/home/jovyan/demo/jobs/${f}" -c main
  fi
done

# Copy notebook, substituting bucket
TMP_NOTEBOOK=$(mktemp)
sed "s|sizhang-gke-dev-ml-demo-data|${DEMO_BUCKET}|g" "${DEMO_DIR}/ml_workflow_scaling_demo.ipynb" > "${TMP_NOTEBOOK}"
kubectl cp "${TMP_NOTEBOOK}" "${NAMESPACE}/${POD}:/home/jovyan/demo/ml_workflow_scaling_demo.ipynb" -c main
rm "${TMP_NOTEBOOK}"


echo "=== 7. Dashboard URL ==="
INGRESS_IP="$(kubectl get svc istio-ingressgateway -n istio-system \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")"
if [ -n "${INGRESS_IP}" ]; then
  echo "  Open: https://${INGRESS_IP}/workspaces/"
  echo "  -> open workspace 'ml-scaling-demo-notebook', then demo/ml_workflow_scaling_demo.ipynb"
else
  echo "  Get the ingress IP with: kubectl get svc istio-ingressgateway -n istio-system"
fi

echo ""
echo "=========================================================================="
echo " Scaling Demo ready! Open the notebook and run the cells top to bottom."
echo "=========================================================================="
