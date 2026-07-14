# Step-by-Step Guide: Deploying Kubeflow Workspaces to GKE

This guide explains how to build and deploy your local Kubeflow Workspaces v2 code changes onto your GKE cluster. It configures the deployment for **standalone mode** (bypassing full Kubeflow OIDC AuthService) by using Istio ingress gateway header injection.

---

## 1. Prerequisites & Registry Configuration

Ensure your environment is set up and targeting your GKE cluster.

```bash
# 1. Verify kubectl is targeting your GKE cluster
kubectl cluster-info

# 2. Define environment variables
export REGISTRY="us-west1-docker.pkg.dev/sizhang-gke-dev/sizhang-repo"
export TAG="local-gke-dev"
```

Ensure you have authenticated to the GKE Artifact Registry in your shell:
```bash
gcloud auth configure-docker us-west1-docker.pkg.dev
```

---

## 2. Build and Push Container Images

Build and push the three workspaces components using your local source code and GKE registry destination.

```bash
# Build and Push Controller
cd workspaces/controller
make docker-build docker-push REGISTRY=${REGISTRY} TAG=${TAG} IMG=${REGISTRY}/workspaces-controller:${TAG}

# Build and Push Backend
cd ../backend
make docker-build docker-push REGISTRY=${REGISTRY} TAG=${TAG} IMG=${REGISTRY}/workspaces-backend:${TAG}

# Build and Push Frontend
cd ../frontend
make docker-build docker-push REGISTRY=${REGISTRY} TAG=${TAG} IMG=${REGISTRY}/workspaces-frontend:${TAG}

# Return to root directory
cd ../..

# Build and Push Jupyter AI Notebook
docker build -t "${REGISTRY}/jupyter-ai-notebook:${TAG}" -f gke/jupyter-ai.Dockerfile gke/
docker push "${REGISTRY}/jupyter-ai-notebook:${TAG}"

```

---

## 3. Install Core Infrastructure (Cert-Manager & Istio)

If Cert-Manager or Istio are not already running in the cluster, install them using the provided scripts.

```bash
# 1. Install Cert-Manager
./developing/scripts/setup-cert-manager.sh

# 2. Install Istio
./developing/scripts/setup-istio.sh

# 3. Apply Gateway and Telemetry resources
kubectl apply -k developing/manifests/istio-gateway
```

---

## 4. Deploy Kubeflow Workspaces Controllers

Deploy the workspaces controller, backend, and frontend using the Makefile targets. The targets will automatically update the image references in the manifests using Kustomize overlays and apply them to the cluster.

```bash
# Deploy Workspaces Controller
cd workspaces/controller
make deploy REGISTRY=${REGISTRY} TAG=${TAG} IMG=${REGISTRY}/workspaces-controller:${TAG}

# Deploy Workspaces Backend
cd ../backend
make deploy REGISTRY=${REGISTRY} TAG=${TAG} IMG=${REGISTRY}/workspaces-backend:${TAG}

# Deploy Workspaces Frontend
cd ../frontend
make deploy REGISTRY=${REGISTRY} TAG=${TAG} IMG=${REGISTRY}/workspaces-frontend:${TAG}

# Return to root directory
cd ../..
```

Verify that all workloads are running:
```bash
kubectl get pods -n kubeflow-workspaces
```

---

## 5. Configure Standalone Authentication & RBAC

Since you are running this without the full Kubeflow OIDC AuthService proxy, the backend expects user headers (`kubeflow-userid` and `kubeflow-groups`) that are normally injected by the proxy.

To enable standalone development/testing access:

### Step 5.1: Bind the 'admin' user to cluster-admin
Deploy the following binding to make the `"admin"` user a cluster administrator for testing:

```bash
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
```

### Step 5.2: Patch Istio VirtualService to Inject User Headers
Run the following patch command to configure Istio to automatically inject the `kubeflow-userid: admin` and `kubeflow-groups: admin` headers on all backend requests coming through the ingress gateway.

```bash
kubectl patch virtualservice workspaces-backend -n kubeflow-workspaces --type=json \
  -p='[{"op": "add", "path": "/spec/http/0/headers", "value": {"request": {"set": {"kubeflow-userid": "admin", "kubeflow-groups": "admin"}}}}]'
```

### Step 5.3: Label GKE Storage Classes for Workspaces
To make storage classes selectable in the Workspaces UI creation form, you must apply the label `notebooks.kubeflow.org/can-use=true` to each class. Apply it to the GKE default classes (`standard-rwo` and `standard`):

```bash
kubectl label storageclass standard-rwo notebooks.kubeflow.org/can-use=true --overwrite
kubectl label storageclass standard notebooks.kubeflow.org/can-use=true --overwrite
```

---


## 6. Configure Workspace Templates (WorkspaceKinds)

Apply the default notebook image templates (`WorkspaceKind`) so the dashboard has options to spawn JupyterLab, VS Code, and RStudio.

```bash
# Register the image-source configmaps
kubectl apply -k workspaces/controller/manifests/kustomize/samples/common

# Register JupyterLab, Code Server (VS Code) and RStudio templates
kubectl apply -f workspaces/controller/manifests/kustomize/samples/jupyterlab_v1beta1_workspacekind.yaml
kubectl apply -f workspaces/controller/manifests/kustomize/samples/codeserver_v1beta1_workspacekind.yaml
kubectl apply -f workspaces/controller/manifests/kustomize/samples/rstudio_v1beta1_workspacekind.yaml
```

### GKE PodSnapshot Templates (Optional, GKE only)
If you are deploying on GKE and want to use the stateful pause/resume functionality:
Ensure your GKE cluster has a `gVisor` node pool configured, and apply the pod snapshot storage configuration along with the snapshot-enabled JupyterLab template:

```bash
# Apply the storage config for pod snapshots (update the GCS bucket inside if needed)
kubectl apply -f gke/pod-snapshot-storage-config.yaml

# Apply the jupyterlab-snapshot WorkspaceKind template (replacing the placeholder with your built image)
sed "s|JUPYTER_AI_IMAGE_PLACEHOLDER|${REGISTRY}/jupyter-ai-notebook:${TAG}|g" gke/jupyterlab_snapshot_workspacekind.yaml | kubectl apply -f -
```

---

## 7. Access the Dashboard and Launch a Notebook

### Step 7.1: Retrieve the External Ingress IP
Get the public LoadBalancer IP of the Istio Ingress Gateway:

```bash
kubectl get svc istio-ingressgateway -n istio-system
```

Look for the `EXTERNAL-IP` value.

### Step 7.2: Open the Dashboard in your Browser
Navigate to the dashboard URL in your browser:
```
https://<EXTERNAL-IP>/workspaces/
```

> **Note on TLS Warnings:** Because the ingress gateway uses a self-signed development certificate, your browser will display a TLS/SSL security warning. You can safely bypass this warning (e.g., click "Advanced" -> "Proceed to ...") to access the dashboard.

### Step 7.3: Create and Connect to a Notebook
1. Select a namespace from the dropdown in the dashboard (e.g. `default`).
2. Click **Create Workspace**.
3. Select **JupyterLab** as the workspace type.
4. Fill in the name (e.g., `my-notebook`) and select your storage/PVC options.
5. Click **Create**.
6. Once the workspace transitions to `Running` (green checkmark), click **Connect** to launch the JupyterLab workspace in your browser.

---

## 8. Connecting Local IDE (VS Code) to Remote Kernel

If you want to connect your local VS Code to the remote Jupyter kernel running in the GKE cluster:

### Step 8.1: Port-Forward the Workspace Service
Establish a port-forward from your local machine to the workspace service. You can use the workspace name (e.g., `my-notebook`) to find and forward the service:

```bash
# Replace 'my-notebook' and 'default' with your workspace name and namespace
kubectl port-forward svc/$(kubectl get svc -l notebooks.kubeflow.org/workspace-name=my-notebook -n default -o jsonpath='{.items[0].metadata.name}') 8888:8888 -n default
```

### Step 8.2: Connect from VS Code
1. Open your local notebook (`.ipynb` file) in VS Code.
2. Click on the **Kernel Selector** in the top-right corner of the notebook editor.
3. Select **Select Another Kernel...** -> **Existing Jupyter Server...**.
4. Enter the connection URL using the workspace path prefix:
   `http://127.0.0.1:8888/workspace/connect/<namespace>/<workspace-name>/jupyterlab/`
   *(e.g., `http://127.0.0.1:8888/workspace/connect/default/my-notebook/jupyterlab/`)*
5. Press `Enter` and select the remote kernel (e.g., `Python 3 (ipykernel)`) from the list.

### Step 8.3: Connecting Directly via External Ingress IP (Alternative)
You can also connect directly using the external IP of the Istio Ingress Gateway (retrieved in Step 7.1) without setting up port-forwarding.

1. Use the HTTPS URL with the external IP:
   `https://<EXTERNAL-IP>/workspace/connect/<namespace>/<workspace-name>/jupyterlab/`
   *(e.g., `https://35.252.83.236/workspace/connect/default/my-notebook/jupyterlab/`)*
   
   > [!IMPORTANT]
   > The URL **must** end with a trailing slash `/`. Without it, the Istio VirtualService routing will fail with a `404 Not Found` error.

### Troubleshooting SSL & Connection Issues (Direct Connection)
Since the GKE ingress uses a self-signed certificate, VS Code will likely fail to connect initially with certificate validation errors (e.g., `"unable to verify the first certificate was not issued by a trusted certificate authority"`).

To resolve this:

1.  **Configure VS Code Settings:**
    Add the following settings to your VS Code `settings.json` (Workspace or User settings):
    ```json
    "jupyter.allowUnauthorizedRemoteConnection": true,
    "http.proxyStrictSSL": false
    ```
    *After applying these settings, reload your VS Code window (`Developer: Reload Window` from Command Palette).*

2.  **Global Workaround (If settings fail):**
    If the above settings do not resolve the issue (due to extension-level certificate handling), you can force Node.js to ignore certificate validation by launching VS Code with the `NODE_TLS_REJECT_UNAUTHORIZED` environment variable:
    
    *   Close all VS Code instances.
    *   Open your terminal and run:
        ```bash
        export NODE_TLS_REJECT_UNAUTHORIZED=0
        ```
    *   Launch VS Code from that **same terminal** (e.g., run `code` or `code .`). For Jetski, launch the IDE via `/opt/jetski-ide/jetski`.

    > [!WARNING]
    > Setting `NODE_TLS_REJECT_UNAUTHORIZED=0` disables SSL verification globally for all extensions in that VS Code session. Use it with caution.

---

## 9. Stateful Pause and Resume using GKE PodSnapshot

If you deployed the GKE PodSnapshot templates (Section 6), you can pause and resume a workspace without losing its memory state.

### Step 9.1: Deploy a Snapshot-Enabled Workspace
1. Create a `Workspace` using the `jupyterlab-snapshot` kind. A sample is provided at `gke/jupyterlab_snapshot_workspace.yaml`:
   ```bash
   kubectl apply -f gke/jupyterlab_snapshot_workspace.yaml
   ```
2. Wait for the pod to be running and write some test state to it:
   ```bash
   # Get the pod name
   kubectl get pods -l notebooks.kubeflow.org/workspace-name=jupyterlab-snapshot-workspace

   # Write a file in the container
   kubectl exec ws-jupyterlab-snapshot-workspace-<suffix>-0 -n default -c main -- bash -c 'echo "hello from snapshot" > /tmp/checkpoint_test.txt'
   ```

### Step 9.2: Pause the Workspace (Trigger Snapshot)
Pause the workspace by updating `spec.paused` to `true`:
```bash
kubectl patch workspace jupyterlab-snapshot-workspace --type merge -p '{"spec": {"paused": true}}'
```
* The controller will create a `PodSnapshotManualTrigger` which initiates a GKE PodSnapshot.
* Once the snapshot is ready, the controller scales down the pod replicas to `0`.
* You can check the snapshot status:
  ```bash
  kubectl get podsnapshots.podsnapshot.gke.io -n default
  ```

### Step 9.3: Resume the Workspace (Restore Snapshot)
Resume the workspace by updating `spec.paused` to `false`:
```bash
kubectl patch workspace jupyterlab-snapshot-workspace --type merge -p '{"spec": {"paused": false}}'
```
* The controller will inject the snapshot restore annotation to the StatefulSet template.
* GKE will restore the pod and restore its memory state from the snapshot.
* Once the pod is back to `Running` and `Ready`, the controller automatically deletes the GKE `PodSnapshot` resource to prevent stale restores.

### Step 9.4: Verify Restored State
Verify that the test file still exists and contains the expected content in the resumed pod:
```bash
# Get the new pod name (since the controller may have recreated it on a new StatefulSet)
kubectl get pods -l notebooks.kubeflow.org/workspace-name=jupyterlab-snapshot-workspace

# Read the file
kubectl exec ws-jupyterlab-snapshot-workspace-<new-suffix>-0 -n default -c main -- cat /tmp/checkpoint_test.txt
# Output should be: hello from snapshot
```

---

## 10. Cleaning Up Resources

To remove all the installed workloads, custom controllers, custom resource definitions (CRDs), Istio configurations, and namespaces from the GKE cluster, run the helper cleanup script located at the repository root:

```bash
./gke/cleanup_gke.sh
```

This script will sequentially delete:
1. All running workspaces and PersistentVolumeClaims in the `default` namespace.
2. The `workspaces-frontend`, `workspaces-backend`, and `workspaces-controller` deployments.
3. The custom CRDs and the `kubeflow-workspaces` namespace.
4. The Istio ingress gateway, Istiod control plane, and custom namespaces.
5. Cert-Manager stack deployment.


