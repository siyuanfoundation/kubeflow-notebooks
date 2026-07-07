#!/usr/bin/env bash
set -euo pipefail

# Align working directory to the repository root
cd "$(dirname "${BASH_SOURCE[0]}")/.."


# Configuration
export CONTEXT="gke_sizhang-gke-dev_us-west1-c_kubeflow-cluster"

echo "=== 1. Setting kubectl context to $CONTEXT ==="
kubectl config use-context "$CONTEXT"

echo "=== 2. Cleaning up user workspace workloads in default namespace ==="
kubectl delete workspace --all -n default --ignore-not-found=true
kubectl delete pvc --all -n default --ignore-not-found=true
kubectl delete secret workspace-secret -n default --ignore-not-found=true
kubectl delete configmap workspacekind-image-source -n default --ignore-not-found=true
kubectl delete serviceaccount default-editor -n default --ignore-not-found=true

echo "=== 3. Undeploying Workspaces frontend, backend, and controller ==="
cd workspaces/frontend
make undeploy || kubectl delete -k manifests/kustomize/overlays/istio --ignore-not-found=true

cd ../backend
make undeploy || kubectl delete -k manifests/kustomize/overlays/istio --ignore-not-found=true

cd ../controller
make undeploy || kubectl delete -k manifests/kustomize/overlays/istio --ignore-not-found=true

cd ../..

echo "=== 4. Cleaning up Workspaces CRDs and Namespace ==="
kubectl delete workspacekind --all --ignore-not-found=true
kubectl delete ns kubeflow-workspaces --ignore-not-found=true

echo "=== 5. Uninstalling Istio Gateway and Mesh ==="
kubectl delete -k developing/manifests/istio-gateway --ignore-not-found=true
if [[ -f "./developing/bin/istio-1.29.1/bin/istioctl" ]]; then
  ./developing/bin/istio-1.29.1/bin/istioctl uninstall --purge -y || true
fi
kubectl delete ns istio-system --ignore-not-found=true
kubectl delete ns kubeflow --ignore-not-found=true

echo "=== 6. Uninstalling Cert-Manager ==="
kubectl delete -f "https://github.com/jetstack/cert-manager/releases/download/v1.20.2/cert-manager.yaml" --ignore-not-found=true

echo "=== Cleanup Completed Successfully ==="
