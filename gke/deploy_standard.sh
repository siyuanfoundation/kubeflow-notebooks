#!/usr/bin/env bash
# Deploy Kubeflow Workspaces (Notebooks v2) & Trainer (v2) on GKE Standard
# Based on gke_deployment_guide_standard.md
set -euo pipefail

# ==============================================================================
# 1. User & Namespace Configuration
# ==============================================================================
export ADMIN_NAME="${ADMIN_NAME:-admin@example.com}"
export ADMIN_PASSWORD="${ADMIN_PASSWORD:-admin1234}"
export ADMIN_NAMESPACE="${ADMIN_NAMESPACE:-kubeflow-admin-example-com}"

export USER_NAME="${USER_NAME:-user@example.com}"
export USER_PASSWORD="${USER_PASSWORD:-12341234}"
export USER_NAMESPACE="${USER_NAMESPACE:-kubeflow-user-example-com}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="${REPO_DIR:-/tmp/kubeflow-community-distribution}"

echo "=================================================================="
echo "Configuring GKE StorageClass for Kubeflow Workspaces..."
echo "=================================================================="
kubectl label storageclass standard-rwo "notebooks.kubeflow.org/can-use=true" --overwrite=true
kubectl annotate storageclass standard-rwo \
  "notebooks.kubeflow.org/display-name=Standard RWO (Persistent Disk)" \
  "notebooks.kubeflow.org/description=Compute Engine persistent disk storage on GKE." \
  --overwrite=true

# ==============================================================================
# 2. Clone kubeflow-community-distribution (Pure Upstream Manifests)
# ==============================================================================
echo "=================================================================="
echo "Preparing kubeflow-community-distribution (26.03.1)..."
echo "=================================================================="
if [ ! -d "${REPO_DIR}" ]; then
  git clone --branch 26.03.1 https://github.com/kubeflow/community-distribution.git "${REPO_DIR}"
else
  echo "Directory ${REPO_DIR} already exists, ensuring branch/tag 26.03.1..."
  git -C "${REPO_DIR}" fetch --tags
  git -C "${REPO_DIR}" checkout 26.03.1
fi

cd "${REPO_DIR}"

# ==============================================================================
# Step 1: Deploy Cert-Manager
# ==============================================================================
echo "=================================================================="
echo "Step 1: Deploying Cert-Manager..."
echo "=================================================================="
kubectl apply -k common/cert-manager/base

echo "Waiting for cert-manager deployments to be ready..."
kubectl -n cert-manager rollout status deployment/cert-manager --timeout=180s
kubectl -n cert-manager rollout status deployment/cert-manager-cainjector --timeout=180s
kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=180s

# Apply kubeflow cert-manager overlay (ClusterIssuer) after webhook is ready
kubectl apply -k common/cert-manager/overlays/kubeflow

# ==============================================================================
# Step 2: Deploy Core Istio Infrastructure & CNI for GKE
# ==============================================================================
echo "=================================================================="
echo "Step 2: Deploying Core Istio Infrastructure & CNI for GKE..."
echo "=================================================================="
kubectl apply -k common/istio/istio-crds/base
kubectl apply -k common/istio/istio-namespace/base
kubectl apply -k common/kubeflow-namespace/base

kubectl apply -k common/istio/istio-install/overlays/gke --server-side --force-conflicts

echo "Waiting for Istio control plane and CNI daemonset to be ready..."
kubectl rollout status deployment/istiod -n istio-system --timeout=180s
kubectl rollout status daemonset/istio-cni-node -n kube-system --timeout=180s

kubectl apply -k common/kubeflow-roles/base
kubectl apply -k common/istio/kubeflow-istio-resources/base

# ==============================================================================
# Step 3: Deploy Authentication (Dex & OAuth2-Proxy)
# ==============================================================================
echo "=================================================================="
echo "Step 3: Deploying Authentication (Dex & OAuth2-Proxy)..."
echo "=================================================================="
kubectl apply -k common/dex/overlays/oauth2-proxy
kubectl apply -k common/oauth2-proxy/overlays/m2m-dex-only

ADMIN_HASH=$(python3 -c 'import bcrypt, os; print(bcrypt.hashpw(os.environ["ADMIN_PASSWORD"].encode(), bcrypt.gensalt(12)).decode())')
USER_HASH=$(python3 -c 'import bcrypt, os; print(bcrypt.hashpw(os.environ["USER_PASSWORD"].encode(), bcrypt.gensalt(12)).decode())')

kubectl create secret generic dex-passwords -n auth \
  --from-literal=DEX_ADMIN_PASSWORD="${ADMIN_HASH}" \
  --from-literal=DEX_USER_PASSWORD="${USER_HASH}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: dex
  namespace: auth
data:
  config.yaml: |
    issuer: http://dex.auth.svc.cluster.local:5556/dex
    storage:
      type: kubernetes
      config:
        inCluster: true
    web:
      http: 0.0.0.0:5556
    logger:
      level: "debug"
      format: text
    oauth2:
      skipApprovalScreen: true
    enablePasswordDB: true
    staticPasswords:
    - email: ${ADMIN_NAME}
      hashFromEnv: DEX_ADMIN_PASSWORD
      username: ${ADMIN_NAME%%@*}
      userID: "10000000000001"
    - email: ${USER_NAME}
      hashFromEnv: DEX_USER_PASSWORD
      username: ${USER_NAME%%@*}
      userID: "15841185641784"
    staticClients:
    - idEnv: OIDC_CLIENT_ID
      redirectURIs: ["/oauth2/callback"]
      name: 'Dex Login Application'
      secretEnv: OIDC_CLIENT_SECRET
EOF

kubectl rollout restart deployment/dex -n auth

# ==============================================================================
# Step 4: Deploy Kubeflow Central Dashboard
# ==============================================================================
echo "=================================================================="
echo "Step 4: Deploying Kubeflow Central Dashboard..."
echo "=================================================================="
kubectl apply -k applications/dashboard/overlays/istio

# ==============================================================================
# Step 5: Deploy Kubeflow Workspaces (Notebooks v2)
# ==============================================================================
echo "=================================================================="
echo "Step 5: Deploying Kubeflow Workspaces (Notebooks v2)..."
echo "=================================================================="
kubectl apply -k applications/workspaces/overlays/istio

kubectl apply --namespace kubeflow --filename "https://raw.githubusercontent.com/kubeflow/community-distribution/26.03.1/applications/workspaces/components/centraldashboard/centraldashboard-config.yaml"

# ==============================================================================
# Step 6: Deploy Kubeflow Trainer (v2)
# ==============================================================================
echo "=================================================================="
echo "Step 6: Deploying Kubeflow Trainer (v2)..."
echo "=================================================================="
kubectl apply -k applications/trainer/overlays --server-side --force-conflicts || true
echo "Waiting for Trainer CRDs to be established..."
kubectl wait --for=condition=Established crd/clustertrainingruntimes.trainer.kubeflow.org --timeout=60s
kubectl wait --for=condition=Established crd/trainingruntimes.trainer.kubeflow.org --timeout=60s
kubectl wait --for=condition=Established crd/trainjobs.trainer.kubeflow.org --timeout=60s
kubectl apply -k applications/trainer/overlays --server-side --force-conflicts

# ==============================================================================
# Step 7: Deploy Profiles for Admin & Standard User
# ==============================================================================
echo "=================================================================="
echo "Step 7: Deploying Profiles for Admin & Standard User..."
echo "=================================================================="
echo "Waiting for profiles-deployment to be ready before creating Profiles..."
kubectl rollout status deployment/profiles-deployment -n kubeflow --timeout=180s

kubectl apply -f - <<EOF
apiVersion: kubeflow.org/v1beta1
kind: Profile
metadata:
  name: ${ADMIN_NAMESPACE}
spec:
  owner:
    kind: User
    name: ${ADMIN_NAME}
---
apiVersion: kubeflow.org/v1beta1
kind: Profile
metadata:
  name: ${USER_NAMESPACE}
spec:
  owner:
    kind: User
    name: ${USER_NAME}
EOF

# ==============================================================================
# Step 8: Register Default WorkspaceKind & Grant Admin Cluster Access
# ==============================================================================
echo "=================================================================="
echo "Step 8: Registering Default WorkspaceKind & Granting Admin Access..."
echo "=================================================================="
echo "Waiting for workspaces-controller webhook to be ready..."
kubectl rollout status deployment/workspaces-controller -n kubeflow-workspaces --timeout=180s

kubectl apply -f applications/workspaces/upstream/controller/samples/jupyterlab_v1beta1_workspacekind.yaml
if kubectl get wsk jupyterlab -o jsonpath='{.spec.filterRules}' 2>/dev/null | grep -q '.'; then
  kubectl patch wsk jupyterlab --type='json' -p='[{"op": "remove", "path": "/spec/filterRules"}]'
fi

kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeflow-workspaces-cluster-admin
rules:
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["storage.k8s.io"]
  resources: ["storageclasses"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["kubeflow.org"]
  resources: ["workspacekinds", "workspacekinds/status", "workspaces", "workspaces/status"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["trainer.kubeflow.org"]
  resources: ["clustertrainingruntimes"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeflow-workspaces-cluster-admin-${ADMIN_NAME%%@*}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeflow-workspaces-cluster-admin
subjects:
- kind: User
  name: ${ADMIN_NAME}
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeflow-admin-${ADMIN_NAME%%@*}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeflow-admin
subjects:
- kind: User
  name: ${ADMIN_NAME}
  apiGroup: rbac.authorization.k8s.io
EOF

# ==============================================================================
# Step 9: Verify All Rollouts
# ==============================================================================
echo "=================================================================="
echo "Step 9: Verifying All Rollouts..."
echo "=================================================================="
kubectl rollout status deployment/istiod -n istio-system --timeout=180s
kubectl rollout status daemonset/istio-cni-node -n kube-system --timeout=180s

kubectl rollout status deployment/dex -n auth --timeout=180s
kubectl rollout status deployment/oauth2-proxy -n oauth2-proxy --timeout=180s

kubectl rollout status deployment/dashboard -n kubeflow --timeout=180s
kubectl rollout status deployment/profiles-deployment -n kubeflow --timeout=180s

kubectl rollout status deployment/workspaces-controller -n kubeflow-workspaces --timeout=180s
kubectl rollout status deployment/workspaces-backend -n kubeflow-workspaces --timeout=180s
kubectl rollout status deployment/workspaces-frontend -n kubeflow-workspaces --timeout=180s

kubectl rollout status deployment/kubeflow-trainer-controller-manager -n kubeflow-system --timeout=180s
kubectl rollout status deployment/jobset-controller-manager -n kubeflow-system --timeout=180s

echo "Waiting for User Namespace ServiceAccounts (default-editor) to be created..."
for ns in "${ADMIN_NAMESPACE}" "${USER_NAMESPACE}"; do
  for i in {1..30}; do
    if kubectl get serviceaccount default-editor -n "${ns}" >/dev/null 2>&1; then
      echo "ServiceAccount default-editor found in namespace ${ns}."
      break
    fi
    echo "Waiting for ServiceAccount default-editor in namespace ${ns} (${i}/30)..."
    sleep 2
  done
  kubectl get serviceaccount default-editor -n "${ns}"
done

# ==============================================================================
# 4. Exposing & Accessing the Central Dashboard
# ==============================================================================
echo "=================================================================="
echo "Exposing & Accessing the Central Dashboard..."
echo "=================================================================="
EXPOSE_MODE="${EXPOSE_MODE:-loadbalancer}"

if [ "${EXPOSE_MODE}" = "loadbalancer" ]; then
  echo "Patching istio-ingressgateway service to type LoadBalancer..."
  kubectl patch svc istio-ingressgateway -n istio-system -p '{"spec": {"type": "LoadBalancer"}}'

  echo "Waiting for external LoadBalancer IP to be provisioned..."
  EXTERNAL_IP=""
  for i in {1..60}; do
    EXTERNAL_IP=$(kubectl get svc istio-ingressgateway -n istio-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [ -n "${EXTERNAL_IP}" ]; then
      break
    fi
    sleep 5
  done

  if [ -n "${EXTERNAL_IP}" ]; then
    echo "=================================================================="
    echo "Kubeflow Central Dashboard is exposed via Public LoadBalancer!"
    echo "External URL: http://${EXTERNAL_IP}/"
    echo "NOTE: Use http:// (not https://) when accessing in your browser."
    echo "=================================================================="
  else
    echo "WARNING: Timed out waiting for LoadBalancer IP. Check status with:"
    echo "kubectl get svc istio-ingressgateway -n istio-system"
  fi
fi

echo ""
echo "Option A (Local Port-Forwarding):"
echo "  kubectl port-forward svc/istio-ingressgateway -n istio-system 8085:80"
echo "  Then open: http://localhost:8085/"
echo ""
echo "Credentials:"
echo "  Admin User:    ${ADMIN_NAME} / ${ADMIN_PASSWORD} (Namespace: ${ADMIN_NAMESPACE})"
echo "  Standard User: ${USER_NAME} / ${USER_PASSWORD} (Namespace: ${USER_NAMESPACE})"
