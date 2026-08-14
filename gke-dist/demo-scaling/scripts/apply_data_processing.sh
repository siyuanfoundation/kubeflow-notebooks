#!/usr/bin/env bash
# =============================================================================
# apply_data_processing.sh — run the Spark ETL job (Stage 1) via notebook pod
# =============================================================================
# Triggers the Spark ETL dynamically using the SparkClient python SDK inside
# the notebook pod, processing 1,000,000 records in parallel.
# -----------------------------------------------------------------------------
set -euo pipefail

NAMESPACE="${NAMESPACE:-default}"

POD="$(kubectl get pods -n "${NAMESPACE}" -l notebooks.kubeflow.org/workspace-name=ml-scaling-demo-notebook -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")"
if [ -z "${POD}" ]; then
  echo "ERROR: ml-scaling-demo-notebook pod not found in namespace ${NAMESPACE}. Run run_demo.sh first." >&2
  exit 1
fi

echo "=== Running Spark ETL (Stage 1) inside notebook pod ==="
kubectl exec "${POD}" -n "${NAMESPACE}" -c main -- python -c "
import os
import sys
DEMO_DIR = '/home/jovyan/demo'
if DEMO_DIR not in sys.path:
    sys.path.insert(0, DEMO_DIR)
import jobs.pipeline as p
p.run_data_processing(num_executors=4, num_shards=10, num_records=1000000)
"

echo ""
echo "Processed shards are now available on the GCS bucket."
