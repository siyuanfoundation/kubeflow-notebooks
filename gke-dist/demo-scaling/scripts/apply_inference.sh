#!/usr/bin/env bash
# =============================================================================
# apply_inference.sh — deploy the model inference service (Stage 3)
# =============================================================================
# Injects demo-scaling/jobs/serve.py into a ConfigMap and applies the inference
# Deployment + Service. Run this after training has written model/params.npz
# (the readiness probe will hold traffic until the model is present).
# -----------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
NAMESPACE="${NAMESPACE:-default}"

# Fetch bucket name from GKE context or default
if kubectl config current-context >/dev/null 2>&1; then
  CURRENT_CONTEXT="$(kubectl config current-context)"
  IFS='_' read -ra ADDR <<< "${CURRENT_CONTEXT}"
  if [[ "${ADDR[0]}" == "gke" ]]; then
    PROJECT_ID="${ADDR[1]}"
  else
    PROJECT_ID=$(gcloud config get-value project 2>/dev/null || echo "sizhang-gke-dev")
  fi
  DEMO_BUCKET="${DEMO_BUCKET:-${PROJECT_ID}-ml-scaling-demo-data}"
else
  # Inside a pod where context is not set
  DEMO_BUCKET="${DEMO_BUCKET:-sizhang-gke-dev-ml-demo-data}"
fi

echo "=== Building ml-scaling-demo-serve-code ConfigMap from serve.py ==="
kubectl create configmap ml-scaling-demo-serve-code \
  --from-file=serve.py="${DEMO_DIR}/jobs/serve.py" \
  -n "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "=== Applying inference Deployment + Service ==="
sed "s/BUCKET_NAME_PLACEHOLDER/${DEMO_BUCKET}/g" "${DEMO_DIR}/manifests/inference-service.yaml" | kubectl apply -f -

# Re-apply the real ConfigMap (the manifest ships a placeholder)
kubectl create configmap ml-scaling-demo-serve-code \
  --from-file=serve.py="${DEMO_DIR}/jobs/serve.py" \
  -n "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout restart deployment/scaling-model-inference -n "${NAMESPACE}"

echo "=== Waiting for inference rollout ==="
kubectl rollout status deployment/scaling-model-inference -n "${NAMESPACE}" --timeout=180s

echo ""
echo "Inference service is up. Test it from inside the cluster:"
echo "  curl -s http://scaling-model-inference.${NAMESPACE}.svc.cluster.local/metrics"
