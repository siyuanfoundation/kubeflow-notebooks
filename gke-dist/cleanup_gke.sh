#!/usr/bin/env bash

# Robust Teardown and Cleanup Script for GKE Cluster Components
# Cleans up Workspaces, Trainer v2, TPU workloads, Istio, Cert-Manager, and CRDs.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NOTEBOOKS_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${NOTEBOOKS_DIR}/.." && pwd)"
COMMUNITY_DIST_DIR="${REPO_ROOT}/kubeflow-community-distribution"

echo "=========================================================================="
echo " Starting Teardown and Cleanup of GKE Cluster Components"
echo "=========================================================================="

# Step 1: Clean up Workspaces, TrainJobs, and TPU Placeholder Jobs in 'default' Namespace
echo "--> Step 1: Cleaning up workloads in default namespace..."
kubectl delete workspace --all -n default --ignore-not-found=true --timeout=30s || true
kubectl delete trainjob --all -n default --ignore-not-found=true --timeout=30s || true
kubectl delete jobset --all -n default --ignore-not-found=true --timeout=30s || true
kubectl delete job tpu-job-ccc -n default --ignore-not-found=true || true
kubectl delete service headless-svc-ccc -n default --ignore-not-found=true || true

kubectl delete pvc tpu-notebook-home-pvc workspace-home-pvc workspace-data-pvc -n default --ignore-not-found=true || true
kubectl delete secret workspace-secret -n default --ignore-not-found=true || true
kubectl delete configmap workspacekind-image-source jupyter-ipc-config -n default --ignore-not-found=true || true
kubectl delete serviceaccount default-editor -n default --ignore-not-found=true || true

# Step 2: Delete ClusterRoleBindings & RBAC Extensions
echo "--> Step 2: Deleting RBAC ClusterRoles and Bindings..."
kubectl delete clusterrolebinding dev-admin-cluster-admin default-sa-cluster-admin kubeflow-trainer-view-cluster-runtimes --ignore-not-found=true || true
kubectl delete clusterrole kubeflow-trainer-admin kubeflow-trainer-edit kubeflow-trainer-view kubeflow-trainer-view-cluster-runtimes --ignore-not-found=true || true

# Step 3: Remove Validating and Mutating Webhooks
echo "--> Step 3: Removing Admission Webhook Configurations..."
kubectl delete validatingwebhookconfiguration workspaces-validating-webhook-configuration cert-manager-webhook jobset-validating-webhook-configuration --ignore-not-found=true || true
kubectl delete mutatingwebhookconfiguration cert-manager-webhook jobset-mutating-webhook-configuration --ignore-not-found=true || true

# Step 4: Delete Workspaces, Trainer Runtimes, and CRD Instances
echo "--> Step 4: Deleting CRD Resources..."
kubectl delete workspacekind --all --ignore-not-found=true --timeout=30s || true
kubectl delete clustertrainingruntime --all --ignore-not-found=true --timeout=30s || true
kubectl delete podsnapshotstorageconfig --all --ignore-not-found=true --timeout=30s || true
kubectl delete podsnapshots.podsnapshot.gke.io --all --ignore-not-found=true --timeout=30s || true
kubectl delete computeclass tpu-v5-8-single-host tpu-v5-8-multi-host --ignore-not-found=true || true

# Step 5: Undeploy Workspaces & Trainer Deployments
echo "--> Step 5: Undeploying Workspaces and Trainer Deployments..."
kubectl delete deployment workspaces-controller workspaces-backend workspaces-frontend -n kubeflow-workspaces --ignore-not-found=true || true
kubectl delete deployment kubeflow-trainer-controller-manager jobset-controller-manager -n kubeflow-system --ignore-not-found=true || true

# Step 6: Uninstall Istio Service Mesh & Gateway
echo "--> Step 6: Uninstalling Istio Ingress Gateway and Mesh..."
kubectl delete -k "${NOTEBOOKS_DIR}/developing/manifests/istio-gateway" --ignore-not-found=true || true

ISTIOCTL_BIN=""
if command -v istioctl &>/dev/null; then
  ISTIOCTL_BIN="istioctl"
elif [[ -f "${NOTEBOOKS_DIR}/developing/bin/istio-1.29.1/bin/istioctl" ]]; then
  ISTIOCTL_BIN="${NOTEBOOKS_DIR}/developing/bin/istio-1.29.1/bin/istioctl"
elif [[ -f "${NOTEBOOKS_DIR}/developing/bin/istioctl" ]]; then
  ISTIOCTL_BIN="${NOTEBOOKS_DIR}/developing/bin/istioctl"
fi

if [[ -n "${ISTIOCTL_BIN}" ]]; then
  echo "   Purging Istio via ${ISTIOCTL_BIN}..."
  "${ISTIOCTL_BIN}" uninstall --purge -y || true
fi

# Step 7: Delete System Namespaces
echo "--> Step 7: Deleting System Namespaces..."
for ns in kubeflow-workspaces kubeflow-system jobset-system istio-system kubeflow cert-manager; do
  kubectl delete ns "${ns}" --ignore-not-found=true --timeout=45s || true
done

# Step 8: Uninstall Cert-Manager Manifests
echo "--> Step 8: Uninstalling Cert-Manager Manifests..."
kubectl delete -f "https://github.com/jetstack/cert-manager/releases/download/v1.20.2/cert-manager.yaml" --ignore-not-found=true || true

# Step 9: Delete CRD Definitions
echo "--> Step 9: Deleting CRDs..."
kubectl delete crd workspacekinds.kubeflow.org workspaces.kubeflow.org trainjobs.trainer.kubeflow.org jobsets.jobset.x-k8s.io --ignore-not-found=true || true

echo "=========================================================================="
echo " Cleanup Finished Successfully!"
echo "=========================================================================="
