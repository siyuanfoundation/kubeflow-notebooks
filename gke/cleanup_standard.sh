#!/usr/bin/env bash
# Clean up everything deployed by deploy_standard.sh on GKE Standard
set -euo pipefail

# ==============================================================================
# 1. User & Namespace Configuration (matching deploy_standard.sh)
# ==============================================================================
export ADMIN_NAME="${ADMIN_NAME:-admin@example.com}"
export ADMIN_NAMESPACE="${ADMIN_NAMESPACE:-kubeflow-admin-example-com}"

export USER_NAME="${USER_NAME:-user@example.com}"
export USER_NAMESPACE="${USER_NAMESPACE:-kubeflow-user-example-com}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="${REPO_DIR:-/tmp/kubeflow-community-distribution}"

if [ ! -d "${REPO_DIR}" ]; then
  echo "Cloning kubeflow-community-distribution to ${REPO_DIR} for manifest deletion..."
  git clone https://github.com/kubeflow/community-distribution.git "${REPO_DIR}"
fi

cd "${REPO_DIR}"

echo "=================================================================="
echo "Step 1: Deleting User Workspaces, WorkspaceKinds, and RBAC..."
echo "=================================================================="
# Delete all Workspace instances across all namespaces first while controller/webhooks are still active
if kubectl get crd workspaces.kubeflow.org >/dev/null 2>&1; then
  kubectl delete workspaces.kubeflow.org --all -A --ignore-not-found=true --timeout=60s || true
fi

if kubectl get crd workspacekinds.kubeflow.org >/dev/null 2>&1; then
  kubectl delete workspacekinds.kubeflow.org --all --ignore-not-found=true --timeout=60s || true
fi

kubectl delete clusterrolebinding "kubeflow-workspaces-cluster-admin-${ADMIN_NAME%%@*}" --ignore-not-found=true
kubectl delete clusterrolebinding "kubeflow-admin-${ADMIN_NAME%%@*}" --ignore-not-found=true
kubectl delete clusterrole kubeflow-workspaces-cluster-admin --ignore-not-found=true

echo "=================================================================="
echo "Step 2: Deleting Kubeflow Profiles & User Namespaces..."
echo "=================================================================="
if kubectl get crd profiles.kubeflow.org >/dev/null 2>&1; then
  kubectl delete profile "${ADMIN_NAMESPACE}" "${USER_NAMESPACE}" --ignore-not-found=true --timeout=60s || true
  # Strip finalizers if any profile is stuck terminating
  for p in $(kubectl get profiles.kubeflow.org -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true); do
    kubectl patch profiles.kubeflow.org "${p}" --type=json -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
  done
fi

kubectl delete namespace "${ADMIN_NAMESPACE}" "${USER_NAMESPACE}" --ignore-not-found=true --wait=false

echo "=================================================================="
echo "Step 3: Deleting Kubeflow Trainer (v2)..."
echo "=================================================================="
kubectl delete -k applications/trainer/overlays --ignore-not-found=true --wait=false || true

echo "=================================================================="
echo "Step 4: Deleting Kubeflow Workspaces (Notebooks v2)..."
echo "=================================================================="
kubectl delete --namespace kubeflow --filename "https://raw.githubusercontent.com/kubeflow/community-distribution/26.03.1/applications/workspaces/components/centraldashboard/centraldashboard-config.yaml" --ignore-not-found=true || true
kubectl delete -k applications/workspaces/overlays/istio --ignore-not-found=true --wait=false || true

echo "=================================================================="
echo "Step 5: Deleting Kubeflow Central Dashboard..."
echo "=================================================================="
kubectl delete -k applications/dashboard/overlays/istio --ignore-not-found=true --wait=false || true

echo "=================================================================="
echo "Step 6: Deleting Authentication (Dex & OAuth2-Proxy)..."
echo "=================================================================="
kubectl delete configmap dex -n auth --ignore-not-found=true || true
kubectl delete secret dex-passwords -n auth --ignore-not-found=true || true
kubectl delete -k common/oauth2-proxy/overlays/m2m-dex-only --ignore-not-found=true --wait=false || true
kubectl delete -k common/dex/overlays/oauth2-proxy --ignore-not-found=true --wait=false || true

echo "=================================================================="
echo "Step 7: Deleting Core Istio Infrastructure, CNI, & Kubeflow Roles..."
echo "=================================================================="
kubectl delete -k common/istio/kubeflow-istio-resources/base --ignore-not-found=true --wait=false || true
kubectl delete -k common/kubeflow-roles/base --ignore-not-found=true --wait=false || true
kubectl delete -k common/istio/istio-install/overlays/gke --ignore-not-found=true --wait=false || true
kubectl delete -k common/kubeflow-namespace/base --ignore-not-found=true --wait=false || true
kubectl delete -k common/istio/istio-namespace/base --ignore-not-found=true --wait=false || true
kubectl delete -k common/istio/istio-crds/base --ignore-not-found=true --wait=false || true

echo "=================================================================="
echo "Step 8: Deleting Cert-Manager..."
echo "=================================================================="
kubectl delete -k common/cert-manager/overlays/kubeflow --ignore-not-found=true --wait=false || true
kubectl delete -k common/cert-manager/base --ignore-not-found=true --wait=false || true

echo "=================================================================="
echo "Step 9: Reverting GKE StorageClass Labels & Annotations..."
echo "=================================================================="
kubectl label storageclass standard-rwo "notebooks.kubeflow.org/can-use-" 2>/dev/null || true
kubectl annotate storageclass standard-rwo \
  "notebooks.kubeflow.org/display-name-" \
  "notebooks.kubeflow.org/description-" 2>/dev/null || true

echo "=================================================================="
echo "Step 10: Cleaning up remaining namespaces & stuck finalizers..."
echo "=================================================================="
NAMESPACES=(
  "${ADMIN_NAMESPACE}"
  "${USER_NAMESPACE}"
  "kubeflow-workspaces"
  "kubeflow-system"
  "kubeflow"
  "oauth2-proxy"
  "auth"
  "istio-system"
  "cert-manager"
)

for ns in "${NAMESPACES[@]}"; do
  if kubectl get namespace "${ns}" >/dev/null 2>&1; then
    echo "Deleting namespace ${ns}..."
    kubectl delete namespace "${ns}" --ignore-not-found=true --wait=false || true
  fi
done

# Wait briefly and clear any stuck namespace finalizers
sleep 5
for ns in "${NAMESPACES[@]}"; do
  if kubectl get namespace "${ns}" >/dev/null 2>&1; then
    echo "Force-clearing finalizers on namespace ${ns}..."
    kubectl get namespace "${ns}" -o json \
      | tr -d "\n" | sed "s/\"finalizers\": \[[^]]*\]/\"finalizers\": []/" \
      | kubectl replace --raw "/api/v1/namespaces/${ns}/finalize" -f - >/dev/null 2>&1 || true
  fi
done

echo "=================================================================="
echo "Cleanup complete!"
echo "=================================================================="
