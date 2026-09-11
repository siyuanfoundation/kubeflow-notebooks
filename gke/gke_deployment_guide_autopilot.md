# Deploying Kubeflow Workspaces & Trainer on GKE (Without Patches)

This guide walks you through deploying **Kubeflow Workspaces (Notebooks v2)** and **Kubeflow Trainer (v2)** on Google Kubernetes Engine (GKE) directly using the official, unmodified manifests from [`kubeflow-community-distribution`](https://github.com/kubeflow/community-distribution) **without creating custom overlays, patch files, or modifying code in git**.

It configures static logins (Dex + OAuth2-Proxy) parameterized by shell environment variables (`USER_NAME`, `USER_PASSWORD`, and `USER_NAMESPACE`), and provides instructions on adding new users using standard `kubectl` workflows.

---

## 1. Prerequisites

### Cluster Connection
Ensure your shell is authenticated to your GKE cluster:
```bash
gcloud container clusters get-credentials $CLUSTER_NAME --region $REGION --project $PROJECT_ID
kubectl cluster-info
```

### Tools Required
- `kubectl` (v1.28+)
- `kustomize` (v5.0+)
- `python3` with `bcrypt` (for generating password hashes)

### User & Namespace Configuration
Define your custom user credentials and preferred workspace namespace:
```bash
export USER_NAME="user@example.com"                  # Your login username / email
export USER_PASSWORD="12341234"                      # Your login password
export USER_NAMESPACE="kubeflow-user-example-com"    # Your dedicated workspace namespace
```
*(You can change these values to whatever you prefer; all subsequent steps reference these environment variables.)*

### GKE StorageClass Configuration
Kubeflow Workspaces requires the StorageClass to be labeled with `notebooks.kubeflow.org/can-use=true` so it appears in the Workspaces UI and can dynamically provision persistent disks:
```bash
kubectl label storageclass standard-rwo "notebooks.kubeflow.org/can-use=true" --overwrite=true
kubectl annotate storageclass standard-rwo \
  "notebooks.kubeflow.org/display-name=Standard RWO (Persistent Disk)" \
  "notebooks.kubeflow.org/description=Compute Engine persistent disk storage on GKE." \
  --overwrite=true
```

---

## 2. Step-by-Step Deployment (Pure Upstream Manifests)

All commands are executed from the root of [`kubeflow-community-distribution`](https://github.com/kubeflow/community-distribution):
```bash
git clone https://github.com/kubeflow/community-distribution.git

cd community-distribution
```

### Step 1: Deploy Cert-Manager
Cert-Manager generates TLS certificates for the admission webhooks used by Workspaces, Trainer, and Dashboard.
```bash
# 1. Apply cert-manager manifests
kubectl apply -k common/cert-manager/base
kubectl apply -k common/cert-manager/overlays/kubeflow

# 2. GKE Autopilot Note:
# Upstream cert-manager uses `--leader-election-namespace=kube-system`.
# On GKE Autopilot, GKE Warden strictly forbids non-system service accounts from creating
# or updating leases in `kube-system`. As a result, `cert-manager-cainjector` and `cert-manager`
# fail leader election and cannot populate the caBundle into the admission webhooks (causing
# "x509: certificate signed by unknown authority" errors).
# Configure leader election in `cert-manager` namespace:
kubectl apply -f - <<'EOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: cert-manager-cainjector:leaderelection
  namespace: cert-manager
rules:
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "update", "patch", "create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cert-manager-cainjector:leaderelection
  namespace: cert-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: cert-manager-cainjector:leaderelection
subjects:
- kind: ServiceAccount
  name: cert-manager-cainjector
  namespace: cert-manager
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: cert-manager:leaderelection
  namespace: cert-manager
rules:
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "update", "patch", "create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cert-manager:leaderelection
  namespace: cert-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: cert-manager:leaderelection
subjects:
- kind: ServiceAccount
  name: cert-manager
  namespace: cert-manager
EOF

kubectl patch deployment cert-manager-cainjector -n cert-manager --type='json' \
  -p='[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--v=2", "--leader-election-namespace=cert-manager"]}]'

kubectl patch deployment cert-manager -n cert-manager --type='json' \
  -p='[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--v=2","--cluster-resource-namespace=$(POD_NAMESPACE)","--leader-election-namespace=cert-manager","--acme-http01-solver-image=quay.io/jetstack/cert-manager-acmesolver:v1.21.1","--max-concurrent-challenges=60"]}]'

# 3. Wait for cert-manager components to be ready
kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=180s
kubectl -n cert-manager rollout status deployment/cert-manager-cainjector --timeout=180s
kubectl -n cert-manager rollout status deployment/cert-manager --timeout=180s
```

---

### Step 2: Deploy Core Istio Infrastructure & Roles
Deploy the Istio CRDs, namespaces, ingress gateways, and Kubeflow RBAC roles:
```bash
# 1. Namespaces and Istio CRDs
# (Deploying `common/kubeflow-namespace/base` here ensures `kubeflow` and `kubeflow-system`
# namespaces exist before Istio applies sidecar prune rules into namespace `kubeflow`)
kubectl apply -k common/istio/istio-crds/base
kubectl apply -k common/istio/istio-namespace/base
kubectl apply -k common/kubeflow-namespace/base

# 2. Istio Install for GKE
# Note for GKE Autopilot: GKE Warden will return forbidden errors for `serviceaccounts "istio-cni"`,
# `configmaps "istio-cni-config"`, and `daemonsets.apps "istio-cni-node"` in `kube-system`.
# These errors are completely safe to ignore on Autopilot because Autopilot manages CNI natively;
# all required resources in `istio-system` (including `istiod` and `istio-ingressgateway`) succeed.
kubectl apply -k common/istio/istio-install/overlays/gke --server-side --force-conflicts || \
kubectl apply -k common/istio/istio-install/overlays/oauth2-proxy --server-side --force-conflicts || true

# 3. Kubeflow Core Roles and Istio Mesh Configuration
kubectl apply -k common/kubeflow-roles/base
kubectl apply -k common/istio/kubeflow-istio-resources/base

# 4. GKE Autopilot Note on Sidecar Injection:
# Upstream Kubeflow manifests label `kubeflow` and `kubeflow-system` with `istio-injection: enabled`.
# Because GKE Autopilot manages node networking and forbids third-party CNI daemons in `kube-system`,
# Istio CNI is not installed. If sidecars are injected, pods crash during startup in the `istio-validation`
# init container (waiting for iptables rules).
# Kubeflow controllers and web UIs do not need sidecars because `istio-ingressgateway` routes HTTP traffic
# directly to their Service ClusterIPs. Remove sidecar injection from these namespaces:
kubectl label namespace kubeflow istio-injection- --overwrite 2>/dev/null || true
kubectl label namespace kubeflow-system istio-injection- --overwrite 2>/dev/null || true
```

---

### Step 3: Deploy Authentication (Dex & OAuth2-Proxy)
Deploy Dex (OIDC provider) and OAuth2-Proxy, configuring Dex with your `$USER_NAME` and `$USER_PASSWORD`:

```bash
# 1. Deploy base Dex and OAuth2-Proxy manifests
kubectl apply -k common/dex/overlays/oauth2-proxy
kubectl apply -k common/oauth2-proxy/overlays/m2m-dex-only

# 2. Generate bcrypt hash for $USER_PASSWORD
USER_HASH=$(python3 -c 'import bcrypt, os; print(bcrypt.hashpw(os.environ["USER_PASSWORD"].encode(), bcrypt.gensalt(12)).decode())')

# 3. Update the dex-passwords Secret in namespace auth
kubectl create secret generic dex-passwords -n auth \
  --from-literal=DEX_USER_PASSWORD="${USER_HASH}" \
  --dry-run=client -o yaml | kubectl apply -f -

# 4. Configure Dex ConfigMap with $USER_NAME
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

# 5. Restart Dex to pick up the custom credentials
kubectl rollout restart deployment/dex -n auth
```

---

### Step 4: Deploy Kubeflow Central Dashboard
Deploy the Central Dashboard UI:
```bash
kubectl apply -k applications/dashboard/overlays/istio

# GKE Autopilot Note:
# Upstream Dashboard and Profiles Deployment templates hardcode `sidecar.istio.io/inject: "true"`.
# On Autopilot without Istio CNI, disable sidecar injection on these deployments so they start cleanly:
kubectl patch deployment dashboard -n kubeflow --type='json' \
  -p='[{"op": "replace", "path": "/spec/template/metadata/labels/sidecar.istio.io~1inject", "value": "false"}]'
kubectl patch deployment profiles-deployment -n kubeflow --type='json' \
  -p='[{"op": "replace", "path": "/spec/template/metadata/labels/sidecar.istio.io~1inject", "value": "false"}]'
```

---

### Step 5: Deploy Kubeflow Workspaces (Notebooks v2)
Deploy the Workspaces controller, backend, and frontend:
```bash
kubectl apply -k applications/workspaces/overlays/istio

# Add the Kubeflow Workspaces tab to the Central Dashboard
kubectl apply --namespace kubeflow --filename "https://raw.githubusercontent.com/kubeflow/community-distribution/26.03.1/applications/workspaces/components/centraldashboard/centraldashboard-config.yaml"

# GKE Autopilot Note:
# 1. Remove sidecar injection from `kubeflow-workspaces` namespace:
kubectl label namespace kubeflow-workspaces istio-injection- --overwrite 2>/dev/null || true

# 2. Disable mTLS on Workspaces DestinationRules so Istio Ingress Gateway connects via standard HTTP:
kubectl patch destinationrule workspaces-backend -n kubeflow-workspaces --type='json' \
  -p='[{"op": "replace", "path": "/spec/trafficPolicy/tls/mode", "value": "DISABLE"}]'
kubectl patch destinationrule workspaces-frontend -n kubeflow-workspaces --type='json' \
  -p='[{"op": "replace", "path": "/spec/trafficPolicy/tls/mode", "value": "DISABLE"}]'
```

---

### Step 6: Deploy Kubeflow Trainer (v2)
Deploy the Kubeflow Trainer controller manager, JobSet controller manager, and cluster training runtimes:
```bash
# Note: `--server-side --force-conflicts` is required because Trainer and JobSet CRDs exceed
# the 256KB client-side apply annotation limit.
# On GKE Autopilot, GKE Warden forbids binding roles to `system:authenticated`, so the
# `kubeflow-trainer-view-cluster-runtimes` and `kubeflow-trainer-public` bindings will be rejected.
# These errors are safe to ignore on Autopilot (`|| true`) as user permissions are managed via Profiles.
kubectl apply -k applications/trainer/overlays --server-side --force-conflicts || true
```

---

### Step 7: Deploy the User Profile (Custom Namespace)
Create the `Profile` resource using `${USER_NAMESPACE}` and `${USER_NAME}`.
The Profile controller automatically creates the namespace, binds the `default-editor` ServiceAccount, configures `kubeflow-edit` permissions for Workspaces and Trainer, and creates the Istio `AuthorizationPolicy`:

```bash
kubectl apply -f - <<EOF
apiVersion: kubeflow.org/v1beta1
kind: Profile
metadata:
  name: ${USER_NAMESPACE}
spec:
  owner:
    kind: User
    name: ${USER_NAME}
EOF
```

---

### Step 8: Register Default WorkspaceKind & Grant Cluster Read Access
1. Register the upstream JupyterLab template so users can create JupyterLab workspaces:
```bash
kubectl apply -f applications/workspaces/upstream/controller/samples/jupyterlab_v1beta1_workspacekind.yaml
```
> [!NOTE]
> `WorkspaceKind` is a **cluster-scoped** resource (like `ClusterTrainingRuntime`), not a namespaced resource. Applying it with `-n <namespace>` will apply it cluster-wide.

> [!IMPORTANT]
> **Remove Sample `filterRules`**: The upstream sample file contains an example filter rule (`scope: WORKSPACE_KIND`) that hides JupyterLab in any namespace that is not labeled `workspace_team=team_1`. To make it visible in user namespaces, remove the sample filter rule:
> ```bash
> kubectl patch wsk jupyterlab --type='json' -p='[{"op": "remove", "path": "/spec/filterRules"}]'
> ```

2. **Grant User Access to Cluster-Scoped Runtimes & Resources**:
On GKE Autopilot, the Profile controller only creates namespace-scoped RoleBindings. The `workspaces-backend` and Central Dashboard require cluster-level read permissions for `namespaces`, `storageclasses`, `workspacekinds`, and `workspaces` (to calculate active counts). Without these, the UI displays `"you are not authorized to access this resource"`.

Apply this `ClusterRole` and bind it to your user:
```bash
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeflow-user-cluster-access
rules:
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["storage.k8s.io"]
  resources: ["storageclasses"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["kubeflow.org"]
  resources: ["workspacekinds", "workspacekinds/status", "workspaces", "workspaces/status"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["trainer.kubeflow.org"]
  resources: ["clustertrainingruntimes"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeflow-user-cluster-access-${USER_NAME%%@*}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeflow-user-cluster-access
subjects:
- kind: User
  name: ${USER_NAME}
  apiGroup: rbac.authorization.k8s.io
EOF
```

---

### Step 9: Verify All Rollouts
Confirm that all core controllers and services are running:
```bash
# Networking & Auth
kubectl rollout status deployment/istiod -n istio-system --timeout=180s
kubectl rollout status deployment/dex -n auth --timeout=180s
kubectl rollout status deployment/oauth2-proxy -n oauth2-proxy --timeout=180s

# Dashboard & Profile Controller
kubectl rollout status deployment/dashboard -n kubeflow --timeout=180s
kubectl rollout status deployment/profiles-deployment -n kubeflow --timeout=180s

# Workspaces (Notebooks v2)
kubectl rollout status deployment/workspaces-controller -n kubeflow-workspaces --timeout=180s
kubectl rollout status deployment/workspaces-backend -n kubeflow-workspaces --timeout=180s
kubectl rollout status deployment/workspaces-frontend -n kubeflow-workspaces --timeout=180s

# Kubeflow Trainer (v2)
kubectl rollout status deployment/kubeflow-trainer-controller-manager -n kubeflow-system --timeout=180s
kubectl rollout status deployment/jobset-controller-manager -n kubeflow-system --timeout=180s

# User Namespace ServiceAccount
kubectl get serviceaccount default-editor -n ${USER_NAMESPACE}
```

---

## 3. Adding New Users (Multi-User Setup)

To add additional team members (e.g., `alice@example.com`):

1. **Generate a bcrypt hash for the new user**:
   ```bash
   ALICE_HASH=$(python3 -c 'import bcrypt; print(bcrypt.hashpw(b"<ALICE_PASSWORD>", bcrypt.gensalt(12)).decode())')
   echo "${ALICE_HASH}"
   ```

2. **Append the new user to `ConfigMap/dex`**:
   ```bash
   kubectl edit configmap dex -n auth
   ```
   Under `staticPasswords`, append the new user entry:
   ```yaml
   staticPasswords:
   - email: ${USER_NAME}
     hashFromEnv: DEX_USER_PASSWORD
     username: ${USER_NAME%%@*}
     userID: "15841185641784"
   - email: alice@example.com
     hash: "<PASTE_ALICE_BCRYPT_HASH>"
     username: alice
     userID: "28492048201948"  # Any unique numeric or UUID string
   ```
   Restart Dex:
   ```bash
   kubectl rollout restart deployment/dex -n auth
   ```

3. **Provision Alice's Isolated Namespace**:
   Apply a `Profile` for Alice:
   ```bash
   kubectl apply -f - <<EOF
   apiVersion: kubeflow.org/v1beta1
   kind: Profile
   metadata:
     name: kubeflow-user-alice-example-com
   spec:
     owner:
       kind: User
       name: alice@example.com
   EOF
   ```
   The Profile controller immediately creates:
   - Namespace `kubeflow-user-alice-example-com`
   - ServiceAccount `default-editor`
   - RoleBindings granting `kubeflow-edit` (full access to Workspaces and Trainer)
   - Istio `AuthorizationPolicy` ensuring only requests authenticated as `alice@example.com` can access workloads in this namespace.

4. **Grant Alice Read Access to WorkspaceKinds & Trainer Runtimes**:
   ```bash
   kubectl create clusterrolebinding kubeflow-workspaces-view-kinds-alice \
     --clusterrole=kubeflow-workspaces-view-kinds \
     --user=alice@example.com
   kubectl create clusterrolebinding kubeflow-trainer-view-runtimes-alice \
     --clusterrole=kubeflow-trainer-view-cluster-runtimes \
     --user=alice@example.com
   ```

---

## 4. Exposing & Accessing the Central Dashboard

Choose one of the following options to access the dashboard:

### Option A: Local Port-Forwarding (Recommended)
Port-forward the Ingress Gateway to your local workstation on a dedicated port (e.g., `8085` to avoid common port `8080` conflicts):
```bash
kubectl port-forward svc/istio-ingressgateway -n istio-system 8085:80
```
Navigate in your browser to:
```
http://localhost:8085/
```
*(This is the most reliable method when working from corporate or development workstations, bypassing external network load balancer routing, corporate proxy drops, and HTTPS auto-upgrade issues.)*

### Option B: Public GKE LoadBalancer IP
The `istio-ingressgateway` Service is provisioned as `type: LoadBalancer`.

> [!IMPORTANT]
> **Why `EXTERNAL-IP took too long to respond` occurs:**
> 1. **Browser HTTPS Auto-Upgrade**: Modern browsers (Chrome, Safari) automatically default to `https://35.252.161.97/`. Because the default Kubeflow Gateway only has an HTTP listener on port 80 without a TLS certificate on port 443, connections to port 443 will hang and time out. You **must** explicitly type the `http://` scheme: `http://35.252.161.97/`.
> 2. **Corporate & Cloud Egress Firewalls**: On corporate networks (e.g. Google Corp / BeyondCorp) or within GCP VPCs, direct outbound connections on unencrypted port 80 to raw external IPs are often blocked or dropped by corporate proxies.
> 3. **GKE Autopilot Traffic Routing**: Set `externalTrafficPolicy: Local` so traffic routes directly to the node hosting the ingress gateway:
>    ```bash
>    kubectl patch svc istio-ingressgateway -n istio-system -p '{"spec":{"externalTrafficPolicy":"Local"}}'
>    ```

Get the external IP:
```bash
kubectl get svc istio-ingressgateway -n istio-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
```
Open in browser (be sure to include `http://`):
```
http://<EXTERNAL-IP>/
```

---

## 5. Using & Verifying Workspaces (Notebooks v2)

1. Open the Kubeflow Central Dashboard in your browser and sign in via Dex using `$USER_NAME` and `$USER_PASSWORD`.
2. Select your user namespace (`$USER_NAMESPACE`) from the dropdown in the top-left.
3. In the left navigation, click **Notebooks v2 -> Workspaces** (or visit `http://<INGRESS_URL>/workspaces/`).
4. Click **New Workspace**:
   - Choose `JupyterLab Notebook`
   - Select hardware specs and set storage class to `standard-rwo`
   - Click **Create Workspace**
5. Once the state is `Running`, click **Connect** to open JupyterLab.

Alternatively, spawn a workspace directly via YAML:
```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: workspace-pvc
  namespace: ${USER_NAMESPACE}
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: standard-rwo
  resources:
    requests:
      storage: 10Gi
---
apiVersion: kubeflow.org/v1beta1
kind: Workspace
metadata:
  name: test-workspace
  namespace: ${USER_NAMESPACE}
spec:
  paused: false
  kind: "jupyterlab"
  podTemplate:
    volumes:
      home: "workspace-pvc"
    options:
      imageConfig: "jupyter-scipy:v1.10.0"
      podConfig: "tiny_cpu"
EOF
```

Access the notebook directly via:
```
http://<INGRESS_URL>/workspace/connect/${USER_NAMESPACE}/test-workspace/jupyterlab/
```

---

## 6. Using & Verifying Kubeflow Trainer (v2)

### Check Available Runtimes
```bash
kubectl get clustertrainingruntime
```
*Output includes `torch-distributed`, `deepspeed-distributed`, `jax-distributed`, `torchtune`, etc.*

### Submit a TrainJob
Run a PyTorch training job in your user namespace:
```bash
kubectl apply -f - <<EOF
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainJob
metadata:
  name: torch-demo-job
  namespace: ${USER_NAMESPACE}
spec:
  runtimeRef:
    name: torch-distributed
  trainer:
    image: pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime
    command:
      - python3
      - -c
      - |
        import torch
        print("=" * 50)
        print("🚀 Kubeflow Trainer Job Executing on GKE!")
        print(f"PyTorch Version: {torch.__version__}")
        print(f"CUDA Available:  {torch.cuda.is_available()}")
        print("✅ Training step completed successfully.")
        print("=" * 50)
EOF
```

### Inspect the Job & Logs
```bash
kubectl get trainjob -n ${USER_NAMESPACE}
kubectl logs -n ${USER_NAMESPACE} -l trainer.kubeflow.org/trainjob-ancestor-step=trainer -c node
```

---

## 7. Teardown & Cleanup

To remove the components:
```bash
cd kubeflow-community-distribution

# 1. Delete user workloads and profile
kubectl delete workspaces --all -n ${USER_NAMESPACE} --ignore-not-found
kubectl delete trainjobs --all -n ${USER_NAMESPACE} --ignore-not-found
kubectl delete profile ${USER_NAMESPACE} --ignore-not-found
kubectl delete workspacekinds --all --ignore-not-found

# 2. Delete components in reverse order
kubectl delete -k applications/trainer/overlays --ignore-not-found
kubectl delete -k applications/workspaces/overlays/istio --ignore-not-found
kubectl delete -k applications/dashboard/overlays/istio --ignore-not-found
kubectl delete -k common/oauth2-proxy/overlays/m2m-dex-only --ignore-not-found
kubectl delete -k common/dex/overlays/oauth2-proxy --ignore-not-found
kubectl delete -k common/istio/kubeflow-istio-resources/base --ignore-not-found
kubectl delete -k common/kubeflow-roles/base --ignore-not-found
kubectl delete -k common/kubeflow-namespace/base --ignore-not-found
kubectl delete -k common/istio/istio-install/overlays/gke --ignore-not-found
kubectl delete -k common/istio/istio-namespace/base --ignore-not-found
kubectl delete -k common/istio/istio-crds/base --ignore-not-found
kubectl delete -k common/cert-manager/overlays/kubeflow --ignore-not-found
kubectl delete -k common/cert-manager/base --ignore-not-found

# 3. Clean up system namespaces if needed
kubectl delete namespace kubeflow-workspaces kubeflow-system kubeflow oauth2-proxy auth istio-system --ignore-not-found
```
