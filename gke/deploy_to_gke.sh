#!/usr/bin/env bash
set -euo pipefail

# Align working directory to the repository root
cd "$(dirname "${BASH_SOURCE[0]}")/.."


# Configuration
export REGISTRY="us-west1-docker.pkg.dev/sizhang-gke-dev/sizhang-repo"
export TAG="local-gke-dev-$(date +%s)"
export CONTEXT="gke_sizhang-gke-dev_us-west1-c_kubeflow-cluster"

echo "=== 1. Setting kubectl context to $CONTEXT ==="
kubectl config use-context "$CONTEXT"

echo "=== 2. Authenticating to Artifact Registry ==="
gcloud auth configure-docker us-west1-docker.pkg.dev --quiet

echo "=== 3. Building and pushing Docker images ==="

echo "--> Controller..."
cd workspaces/controller
make docker-build docker-push REGISTRY="${REGISTRY}" TAG="${TAG}" IMG="${REGISTRY}/workspaces-controller:${TAG}"

echo "--> Backend..."
cd ../backend
make docker-build docker-push REGISTRY="${REGISTRY}" TAG="${TAG}" IMG="${REGISTRY}/workspaces-backend:${TAG}"

echo "--> Frontend..."
cd ../frontend
make docker-build docker-push REGISTRY="${REGISTRY}" TAG="${TAG}" IMG="${REGISTRY}/workspaces-frontend:${TAG}" DEPLOYMENT_MODE="standalone"

cd ../..

echo "=== 4. Deploying core infrastructure (Cert-Manager & Istio) ==="
./developing/scripts/setup-cert-manager.sh
./developing/scripts/setup-istio.sh
kubectl apply -k developing/manifests/istio-gateway

echo "=== 5. Deploying Workspaces stack ==="
cd workspaces/controller
make deploy REGISTRY="ghcr.io/kubeflow/notebooks" TAG="${TAG}" IMG="${REGISTRY}/workspaces-controller:${TAG}"

cd ../backend
make deploy REGISTRY="ghcr.io/kubeflow/notebooks" TAG="${TAG}" IMG="${REGISTRY}/workspaces-backend:${TAG}"

cd ../frontend
make deploy REGISTRY="ghcr.io/kubeflow/notebooks" TAG="${TAG}" IMG="${REGISTRY}/workspaces-frontend:${TAG}"

cd ../..

echo "=== 6. Configuring Standalone Auth & RBAC ==="

# Bind admin to cluster-admin
cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: dev-admin-cluster-admin
subjects:
- kind: User
  name: admin
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: cluster-admin
  apiGroup: rbac.authorization.k8s.io
EOF

# Patch backend VirtualService to inject default user headers
kubectl patch virtualservice workspaces-backend -n kubeflow-workspaces --type=json \
  -p='[{"op": "add", "path": "/spec/http/0/headers", "value": {"request": {"set": {"kubeflow-userid": "admin", "kubeflow-groups": "admin"}}}}]'

# Label default storage classes to be selectable in the Workspaces UI
echo "Configuring default StorageClasses..."
kubectl label storageclass standard-rwo notebooks.kubeflow.org/can-use=true --overwrite=true || true
kubectl label storageclass standard notebooks.kubeflow.org/can-use=true --overwrite=true || true


echo "=== 7. Waiting for Workspaces system pods to become ready ==="
kubectl wait --for=condition=Available deployment/workspaces-controller -n kubeflow-workspaces --timeout=300s
kubectl wait --for=condition=Available deployment/workspaces-backend -n kubeflow-workspaces --timeout=300s
kubectl wait --for=condition=Available deployment/workspaces-frontend -n kubeflow-workspaces --timeout=300s

echo "=== 8. Applying WorkspaceKind templates ==="
kubectl apply -k workspaces/controller/manifests/kustomize/samples/common
kubectl apply -f workspaces/controller/manifests/kustomize/samples/jupyterlab_v1beta1_workspacekind.yaml
kubectl apply -f workspaces/controller/manifests/kustomize/samples/codeserver_v1beta1_workspacekind.yaml
kubectl apply -f workspaces/controller/manifests/kustomize/samples/rstudio_v1beta1_workspacekind.yaml

echo "=== 9. Retrieving Gateway Ingress IP ==="
INGRESS_IP=""
echo "Waiting for External IP to be provisioned for Ingress Gateway (this might take up to 2 minutes)..."
for i in {1..30}; do
  INGRESS_IP=$(kubectl get svc istio-ingressgateway -n istio-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")
  if [ -n "$INGRESS_IP" ]; then
    break
  fi
  sleep 10
done

if [ -z "$INGRESS_IP" ]; then
  # Try hostname if GKE is using a hostname instead of IP (e.g. on AWS/hybrid, though on GCP it is always IP)
  INGRESS_IP=$(kubectl get svc istio-ingressgateway -n istio-system -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || echo "")
fi

if [ -z "$INGRESS_IP" ]; then
  echo "WARNING: Ingress External IP/Hostname could not be retrieved yet. You can find it later using:"
  echo "  kubectl get svc istio-ingressgateway -n istio-system"
else
  echo "Ingress External IP: $INGRESS_IP"
  echo "Dashboard URL: https://${INGRESS_IP}/workspaces/"
  
  echo "=== 10. Verifying Dashboard reachability via HTTP request ==="
  # Send an insecure HTTP request to check if it responds (expecting a redirect or 200/404 depending on index config, normally a redirect or 200 for /workspaces/)
  HTTP_STATUS=$(curl -k -s -o /dev/null -w "%{http_code}" "https://${INGRESS_IP}/workspaces/")
  echo "HTTP Status Response: $HTTP_STATUS"
  if [[ "$HTTP_STATUS" == "200" || "$HTTP_STATUS" == "301" || "$HTTP_STATUS" == "302" ]]; then
    echo "SUCCESS: Dashboard is reachable!"
  else
    echo "WARNING: Received HTTP Status $HTTP_STATUS. Please check ingress gateway logs or routing configuration."
  fi
fi

echo "=== Deployment Completed Successfully ==="
