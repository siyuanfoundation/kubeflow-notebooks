#!/usr/bin/env bash
# =============================================================================
# cleanup_demo.sh — remove all demo resources
# =============================================================================
# Deletes the demo workspace, inference service, jobs, ConfigMap, and shared
# volumes. Does NOT touch the base gke-dist stack (Trainer, Workspaces, Istio).
# -----------------------------------------------------------------------------
set -euo pipefail

NAMESPACE="${NAMESPACE:-default}"

echo "=== Deleting inference service ==="
kubectl delete deployment fashion-mnist-inference -n "${NAMESPACE}" --ignore-not-found
kubectl delete service fashion-mnist-inference -n "${NAMESPACE}" --ignore-not-found
kubectl delete configmap ml-demo-serve-code -n "${NAMESPACE}" --ignore-not-found

echo "=== Deleting the Spark ETL job ==="
kubectl delete sparkapplication fashion-mnist-etl -n "${NAMESPACE}" --ignore-not-found || true
kubectl delete configmap ml-demo-spark-code -n "${NAMESPACE}" --ignore-not-found

echo "=== Deleting any leftover demo TrainJobs / JobSets ==="
kubectl delete trainjobs.trainer.kubeflow.org --all -n "${NAMESPACE}" --ignore-not-found || true

echo "=== Deleting the demo workspace ==="
kubectl delete workspace ml-demo-notebook -n "${NAMESPACE}" --ignore-not-found

echo "=== Deleting demo volumes ==="
kubectl delete pvc ml-demo-notebook-home-pvc -n "${NAMESPACE}" --ignore-not-found
kubectl delete pvc ml-demo-gcs-pvc -n "${NAMESPACE}" --ignore-not-found
kubectl delete pv ml-demo-gcs-pv --ignore-not-found
# The shared storage is now in GCS.
CURRENT_CONTEXT="$(kubectl config current-context)"
IFS='_' read -ra ADDR <<< "${CURRENT_CONTEXT}"
if [[ "${ADDR[0]}" == "gke" ]]; then
  PROJECT_ID="${ADDR[1]}"
else
  PROJECT_ID=$(gcloud config get-value project 2>/dev/null || echo "sizhang-gke-dev")
fi
DEMO_BUCKET="${DEMO_BUCKET:-${PROJECT_ID}-ml-demo-data}"

read -r -p "Also delete the shared GCS bucket gs://${DEMO_BUCKET} (dataset + model)? [y/N] " ans
if [[ "${ans}" =~ ^[Yy]$ ]]; then
  gcloud storage rm --project="${PROJECT_ID}" -r "gs://${DEMO_BUCKET}"
  echo "Shared data bucket deleted."
else
  echo "Kept gs://${DEMO_BUCKET} (dataset + model preserved)."
fi

echo "Demo cleanup complete."
