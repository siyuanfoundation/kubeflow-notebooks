#!/usr/bin/env bash
# =============================================================================
# cleanup_demo.sh — tear down all scaling demo resources on GKE
# =============================================================================
set -euo pipefail

NAMESPACE="${NAMESPACE:-default}"

echo "=== Tearing down Stage 3: Inference / Serving ==="
kubectl delete deployment scaling-model-inference -n "${NAMESPACE}" --ignore-not-found
kubectl delete service scaling-model-inference -n "${NAMESPACE}" --ignore-not-found
kubectl delete configmap ml-scaling-demo-serve-code -n "${NAMESPACE}" --ignore-not-found

echo "=== Tearing down Stage 2: TPU TrainJobs ==="
# Delete all TrainJobs in the namespace
kubectl delete trainjobs --all -n "${NAMESPACE}" --ignore-not-found

echo "=== Tearing down Stage 1: Spark Applications ==="
kubectl delete sparkapplication scaling-data-etl -n "${NAMESPACE}" --ignore-not-found
kubectl delete sparkconnect scaling-data-etl -n "${NAMESPACE}" --ignore-not-found
kubectl delete configmap ml-demo-spark-code -n "${NAMESPACE}" --ignore-not-found

echo "=== Tearing down the Notebook Workspace ==="
kubectl delete workspace ml-scaling-demo-notebook -n "${NAMESPACE}" --ignore-not-found
kubectl delete pvc ml-scaling-demo-home-pvc -n "${NAMESPACE}" --ignore-not-found

echo "=== Restoring TPU Reservation Placeholder Job ==="
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PARENT_DIR="$(cd "${SCRIPT_DIR}/../../" && pwd)"
if [ -f "${PARENT_DIR}/tpu-job-ccc.yaml" ]; then
  kubectl apply -f "${PARENT_DIR}/tpu-job-ccc.yaml"
fi

echo "=========================================================================="
echo " Cleanup complete! All GKE scaling demo resources have been deleted."
echo "=========================================================================="
