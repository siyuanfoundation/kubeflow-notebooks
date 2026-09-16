#!/usr/bin/env bash
# ==============================================================================
# Teardown & Cleanup Standalone Kubeflow Workspaces, Trainer, & Spark Operator on GKE
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_DIR="${DIST_DIR:-/tmp/kubeflow-community-distribution}"

export PROJECT="${PROJECT:-${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}}"
export CLUSTER="${CLUSTER:-kubeflow-notebooks}"
export LOCATION="${LOCATION:-us-central1-c}"
export TENANT_NAMESPACE="${TENANT_NAMESPACE:-team-a}"
export ADDRESS_NAME="${ADDRESS_NAME:-notebooks-gke-global}"
export CERTIFICATE_NAME="${CERTIFICATE_NAME:-notebooks-gke}"
export CERTIFICATE_MAP="${CERTIFICATE_MAP:-notebooks-gke}"
export CONTEXT="${CONTEXT:-gke_${PROJECT}_${LOCATION}_${CLUSTER}}"
export DELETE_EDGE_RESOURCES="${DELETE_EDGE_RESOURCES:-false}"

echo "=================================================================="
echo "Cleaning up Standalone Kubeflow Workspaces on GKE (${CLUSTER})..."
echo "=================================================================="

# 1. Remove tenant workloads
echo "Deleting tenant workspaces, trainjobs, sparkapplications, and deployments in ${TENANT_NAMESPACE}..."
kubectl --context="${CONTEXT}" delete workspaces --all -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete trainjobs --all -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete sparkconnects --all -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete sparkapplications --all -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete deployment fashion-mnist-inference -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete service fashion-mnist-inference -n "${TENANT_NAMESPACE}" --ignore-not-found || true

# 2. Remove Kubeflow Spark Operator and Trainer
if [[ -d "${DIST_DIR}" ]]; then
  echo "Removing Kubeflow Spark Operator and Kubeflow Trainer..."
  kubectl --context="${CONTEXT}" delete -k "${DIST_DIR}/applications/spark/spark-operator/overlays/kubeflow" --ignore-not-found || true
  kubectl --context="${CONTEXT}" delete -k "${DIST_DIR}/applications/trainer/overlays" --ignore-not-found || true
fi

# 3. Remove WorkspaceKinds and ComputeClasses
kubectl --context="${CONTEXT}" delete workspacekind jupyterlab gke-jupyterlab --ignore-not-found || true
if [[ -d "${SCRIPT_DIR}/manifests/compute-classes" ]]; then
  kubectl --context="${CONTEXT}" delete -f "${SCRIPT_DIR}/manifests/compute-classes/" --ignore-not-found || true
fi

# 4. Remove standalone edge and application resources
if [[ -d "${SCRIPT_DIR}/rendered/ready" ]]; then
  kubectl --context="${CONTEXT}" delete -f "${SCRIPT_DIR}/rendered/ready/edge.json" --ignore-not-found || true
  kubectl --context="${CONTEXT}" delete -f "${SCRIPT_DIR}/rendered/ready/applications.json" --ignore-not-found || true
  kubectl --context="${CONTEXT}" delete -f "${SCRIPT_DIR}/rendered/ready/isolation.json" --ignore-not-found || true
fi

kubectl --context="${CONTEXT}" delete validatingadmissionpolicybinding notebooks-gke-pilot-workspaces --ignore-not-found || true
kubectl --context="${CONTEXT}" delete validatingadmissionpolicy notebooks-gke-pilot-workspaces --ignore-not-found || true
kubectl --context="${CONTEXT}" delete namespace notebooks-connections --ignore-not-found || true

if [[ "${DELETE_EDGE_RESOURCES}" == "true" ]]; then
  echo "Deleting Certificate Manager certificate map and Global IP (${ADDRESS_NAME})..."
  gcloud certificate-manager maps entries delete notebooks --map="${CERTIFICATE_MAP}" --project="${PROJECT}" --quiet || true
  gcloud certificate-manager maps entries delete notebooks-desktop --map="${CERTIFICATE_MAP}" --project="${PROJECT}" --quiet || true
  gcloud certificate-manager maps delete "${CERTIFICATE_MAP}" --project="${PROJECT}" --quiet || true
  gcloud certificate-manager certificates delete "${CERTIFICATE_NAME}" --project="${PROJECT}" --quiet || true
  gcloud certificate-manager certificates delete "${CERTIFICATE_NAME}-desktop" --project="${PROJECT}" --quiet || true
  gcloud compute addresses delete "${ADDRESS_NAME}" --global --project="${PROJECT}" --quiet || true
fi

echo "=================================================================="
echo "✅ Standalone Kubeflow Workspaces cleanup complete."
echo "=================================================================="
