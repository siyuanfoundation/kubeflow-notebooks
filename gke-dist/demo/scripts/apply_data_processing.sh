#!/usr/bin/env bash
# =============================================================================
# apply_data_processing.sh — run the Spark ETL job (Stage 1) from the CLI
# =============================================================================
# Injects demo/jobs/data_processing.py into a ConfigMap and submits the
# SparkApplication via the Kubeflow Spark Operator. Waits for the job to reach
# the COMPLETED state.
#
# Prerequisites:
#   * the Kubeflow Spark Operator is installed (community distribution),
#   * the shared GCS bucket exists (scripts/run_demo.sh).
# -----------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
NAMESPACE="${NAMESPACE:-default}"
APP="fashion-mnist-etl"

echo "=== Building ml-demo-spark-code ConfigMap from data_processing.py ==="
kubectl create configmap ml-demo-spark-code \
  --from-file=data_processing.py="${DEMO_DIR}/jobs/data_processing.py" \
  -n "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "=== Submitting SparkApplication ${APP} ==="
kubectl delete sparkapplication "${APP}" -n "${NAMESPACE}" --ignore-not-found
sleep 2
kubectl apply -f "${DEMO_DIR}/manifests/spark-data-processing.yaml"

# Re-apply the real code ConfigMap (the manifest ships a placeholder).
kubectl create configmap ml-demo-spark-code \
  --from-file=data_processing.py="${DEMO_DIR}/jobs/data_processing.py" \
  -n "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "=== Waiting for Spark ETL to complete ==="
for i in {1..180}; do
  STATE="$(kubectl get sparkapplication "${APP}" -n "${NAMESPACE}" \
    -o jsonpath='{.status.applicationState.state}' 2>/dev/null || echo "")"
  case "${STATE}" in
    COMPLETED)
      echo "Spark ETL COMPLETED."
      break
      ;;
    FAILED|SUBMISSION_FAILED|FAILING)
      echo "ERROR: Spark ETL entered state ${STATE}." >&2
      kubectl logs -n "${NAMESPACE}" \
        -l "sparkoperator.k8s.io/app-name=${APP},spark-role=driver" --tail=100 || true
      exit 1
      ;;
    *)
      echo "  (attempt ${i}/180) Spark state: ${STATE:-<pending>}"
      sleep 5
      ;;
  esac
done

echo ""
echo "Processed shards on the shared volume:"
echo "  kubectl exec <notebook-pod> -c main -- ls -lh /data/processed/train/"
