#!/usr/bin/env bash
# =============================================================================
# apply_inference.sh — deploy the model inference service (Stage 3)
# =============================================================================
# Injects demo/jobs/serve.py into a ConfigMap and applies the inference
# Deployment + Service. Run this after training has written /data/model/params.npz
# (the readiness probe will hold traffic until the model is present).
# -----------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
NAMESPACE="${NAMESPACE:-default}"

echo "=== Building ml-demo-serve-code ConfigMap from serve.py ==="
kubectl create configmap ml-demo-serve-code \
  --from-file=serve.py="${DEMO_DIR}/jobs/serve.py" \
  -n "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "=== Applying inference Deployment + Service ==="
BUCKET_NAME="${BUCKET_NAME:-sizhang-gke-dev-ml-demo-data}"
sed "s/BUCKET_NAME_PLACEHOLDER/${BUCKET_NAME}/g" "${DEMO_DIR}/manifests/inference-service.yaml" | kubectl apply -f -

# The manifest ships a placeholder ConfigMap; re-apply the real one so the
# rollout picks up serve.py, then restart to pull the mounted code.
kubectl create configmap ml-demo-serve-code \
  --from-file=serve.py="${DEMO_DIR}/jobs/serve.py" \
  -n "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout restart deployment/fashion-mnist-inference -n "${NAMESPACE}"

echo "=== Waiting for inference rollout ==="
kubectl rollout status deployment/fashion-mnist-inference -n "${NAMESPACE}" --timeout=180s

echo ""
echo "Inference service is up. Test it from inside the cluster:"
echo "  curl -s http://fashion-mnist-inference.${NAMESPACE}.svc.cluster.local/metrics"
