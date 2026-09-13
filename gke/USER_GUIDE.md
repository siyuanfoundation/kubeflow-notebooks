# Set up Kubeflow Notebooks on GKE

This guide is for an administrator installing Notebooks in their own Google Cloud
project. It uses GKE Gateway, Certificate Manager, Identity-Aware Proxy (IAP), and
the integration access proxy, with Istio disabled. Users sign in with Google and
run notebooks on GKE; notebook home directories use GKE persistent disks.

This is an experimental, trusted single-user installation, not a supported
production installer or a hostile multi-tenant isolation guarantee. Start with
one trusted account in `team-a`. Do not enroll a university or organization until
the [security limits](#security-and-operational-limits) are addressed. The dated
[codelab record](CODELAB.md#2026-09-12-working-single-user-pilot) describes the
deployment tested by the maintainers, not resources that customers should reuse.

Guide checks on 2026-09-12: shell examples passed syntax checking; the exact
NEG/backend discovery recipe found the pilot backend; rendering with a different
customer account removed the maintainer User bindings and passed server-side
dry run. A fresh installation in a second customer's project has not been run.

## Before you start

You need:

- A dedicated existing GKE cluster in your project. The tested version is
  `1.35.7-gke.1222000`, with VPC-native networking, Dataplane V2, Workload Identity
  Federation for GKE, HTTP load balancing, and the PD CSI driver enabled.
- An owned DNS name and permission to publish its A record. A third-party
  IP-derived DNS service is suitable only for an explicitly accepted experiment.
- A trusted Google account for the first notebook user. It need not be the
  operator's account or have Google Cloud administration permissions.
- Bash, authenticated `gcloud`, the GKE auth plugin, `kubectl`, Docker with amd64
  build support, Go 1.25 or later, `jq`, `curl`, and `sha256sum`.
- A reviewed checkout containing this integration and its compatible upstream
  sources. Keep the source revision, build records, and generated plans. Git
  revision metadata alone does not capture uncommitted source changes.

The administrator needs permissions to enable project APIs, update the selected
cluster, push and let cluster nodes pull Artifact Registry images, manage the
dedicated address/certificate resources, configure OAuth, set this application's
IAP IAM policy, and install cluster-scoped Kubernetes resources including RBAC and
admission policy. Follow your organization's IAM process; do not grant notebook
users Owner, cluster-admin, or project-wide IAP access to make this guide work.

The cluster, external load balancer, reserved address, image storage, and notebook
disks incur charges. Stop workspaces when unused. Stopping a workspace does not
remove the load balancer, PVCs, or disks.

## 1. Choose your deployment values

Run commands from the repository root. Substitute your own values before running:

```sh
set -euo pipefail
export PROJECT=YOUR_PROJECT_ID
export CLUSTER=YOUR_CLUSTER_NAME
export LOCATION=YOUR_CLUSTER_ZONE_OR_REGION
export REGION=YOUR_ARTIFACT_REGISTRY_REGION
export NOTEBOOK_HOST=notebooks.YOUR_DOMAIN
export PILOT_USER=YOUR_GOOGLE_ACCOUNT_EMAIL
export REPOSITORY=notebooks
export ADDRESS_NAME=notebooks-gke-global
export CERTIFICATE_NAME=notebooks-gke
export CERTIFICATE_MAP=notebooks-gke
export CONTEXT=gke_${PROJECT}_${LOCATION}_${CLUSTER}
export REGISTRY=${REGION}-docker.pkg.dev/${PROJECT}/${REPOSITORY}
export TAG=pilot-$(date -u +%Y%m%d%H%M%S)
```

The system namespace is `kubeflow-workspaces`; this first installation uses tenant
namespace `team-a`. Resource names above must be dedicated to this installation.
Inspect existing resources before creating or adopting anything. Do not alter
another application's resources or force server-side apply ownership conflicts.

```sh
gcloud container clusters describe "$CLUSTER" --project="$PROJECT" \
  --location="$LOCATION" \
  --format='yaml(status,networkConfig,ipAllocationPolicy,addonsConfig,workloadIdentityConfig,privateClusterConfig,controlPlaneEndpointsConfig)'
gcloud container clusters get-credentials "$CLUSTER" --project="$PROJECT" \
  --location="$LOCATION"
kubectl --context="$CONTEXT" get namespaces,gatewayclasses
gcloud artifacts repositories list --project="$PROJECT" --location="$REGION"
gcloud compute addresses list --project="$PROJECT"
gcloud certificate-manager maps list --project="$PROJECT"
```

Require `ADVANCED_DATAPATH` and VPC-native networking. Dataplane V2 enforces
NetworkPolicy even if the legacy `networkPolicyConfig.disabled` field is true.
Record the cluster's verified private control-plane endpoint as
`CONTROL_PLANE_CIDR=IP_ADDRESS/32`. Do not guess it or use `0.0.0.0/0`.
The integration also permits only `kube-system` Konnectivity agent pods to reach
the controller webhook on TCP 9443; the direct control-plane IP alone was
insufficient on the tested cluster.

## 2. Enable the platform components

Enable the APIs required by this installation according to your project policy:

```sh
gcloud services enable container.googleapis.com compute.googleapis.com \
  artifactregistry.googleapis.com certificatemanager.googleapis.com \
  iap.googleapis.com --project="$PROJECT"
gcloud container clusters update "$CLUSTER" --project="$PROJECT" \
  --location="$LOCATION" --gateway-api=standard --quiet
kubectl --context="$CONTEXT" wait gatewayclass/gke-l7-global-external-managed \
  --for=condition=Accepted --timeout=10m
kubectl --context="$CONTEXT" get crd \
  gateways.gateway.networking.k8s.io httproutes.gateway.networking.k8s.io \
  gcpbackendpolicies.networking.gke.io healthcheckpolicies.networking.gke.io
```

If the GatewayClass is not present yet, check the GKE operation and retry after
reconciliation. Do not install a replacement upstream Gateway CRD bundle.

If your dedicated registry does not already exist, create it:

```sh
gcloud artifacts repositories create "$REPOSITORY" --project="$PROJECT" \
  --location="$REGION" --repository-format=docker
```

Inspect for an existing cert-manager installation. If absent, install this tested
version for the internal admission webhook, not public HTTPS:

```sh
mkdir -p gke/bin
curl -fsSL https://github.com/cert-manager/cert-manager/releases/download/v1.21.2/cert-manager.yaml \
  -o gke/bin/cert-manager-v1.21.2.yaml
printf '%s  %s\n' e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f \
  gke/bin/cert-manager-v1.21.2.yaml | sha256sum --check
kubectl --context="$CONTEXT" apply --server-side \
  --field-manager=notebooks-gke-platform -f gke/bin/cert-manager-v1.21.2.yaml
for component in cert-manager cert-manager-webhook cert-manager-cainjector; do
  kubectl --context="$CONTEXT" -n cert-manager rollout status \
    deployment/"$component" --timeout=5m
done
```

Do not replace or uninstall a shared cert-manager deployment.

## 3. Build your application images

```sh
make -C gke test
gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
docker build --platform=linux/amd64 -t "$REGISTRY/gke-access-proxy:$TAG" gke
docker build --platform=linux/amd64 -f gke/frontend.Dockerfile \
  -t "$REGISTRY/gke-frontend:$TAG" .
docker build --platform=linux/amd64 -f workspaces/controller/Dockerfile \
  -t "$REGISTRY/gke-controller:$TAG" workspaces/controller
docker build --platform=linux/amd64 -f workspaces/backend/Dockerfile \
  -t "$REGISTRY/gke-backend:$TAG" workspaces
for component in access-proxy frontend controller backend; do
  docker push "$REGISTRY/gke-$component:$TAG"
done
```

Use registry manifest digests, not tags or local Docker image IDs. The next step
queries these digests automatically. Review image vulnerabilities and base-image
provenance before use beyond a pilot. The frontend's build toolchain and dependency
advisories remain release-review items; do not run an unreviewed upstream upgrade
as part of installation.

## 4. Configure Google login

This is an administrator task performed once per application, not a student or
notebook-user task. OAuth establishes identity; IAP IAM grants admission;
Kubernetes RBAC separately grants notebook permissions.

| Accounts that will sign in | OAuth choice |
| --- | --- |
| Google Workspace/Cloud Identity users within the GCP project's organization | Google-managed OAuth; no client secret needed |
| Google accounts outside that organization | Dedicated custom OAuth client |
| Non-Google identity provider | Not implemented by this guide; requires a separately reviewed federation design |

Check the project's parent organization. A project IAM grant or matching-looking
email domain does not establish eligibility for Google-managed OAuth.

For managed OAuth, set both values empty:

```sh
export IAP_CLIENT_ID=
export IAP_SECRET_NAME=
```

For custom OAuth:

1. Open Google Auth Platform in **your selected project**. Inspect existing
   Branding and Audience configuration before changing anything shared.
2. For external pilot accounts, use External/Testing and add the exact first user
   as a test user, following organization policy. Do not publish broadly.
3. Create a dedicated **Web application** client named **Notebooks GKE**.
4. Download its credential JSON to a private location outside the repository and
   restrict access to the operator. Never paste its contents in chat or commit it.
5. Add the exact authorized redirect URI, substituting its public client ID:
   `https://iap.googleapis.com/v1/oauth/clientIds/CLIENT_ID:handleRedirect`.
   Do not use the notebook URL as the callback. Save and verify the callback.

```sh
export OAUTH_FILE=/absolute/private/path/oauth-client.json
chmod 600 "$OAUTH_FILE"
jq -e --arg project "$PROJECT" \
  '.web.project_id == $project and (.web.client_secret | type == "string" and length > 0)' \
  "$OAUTH_FILE" >/dev/null
export IAP_CLIENT_ID=$(jq -er '.web.client_id' "$OAUTH_FILE")
export IAP_SECRET_NAME=iap-oauth
```

Only the public client ID and Secret name go in deployment configuration. The
Secret is created after the namespace exists in step 7. Never supply only one of
the two values, disable IAP, or manually modify the GKE-owned backend to bypass
its policy. OAuth and edge updates can take several minutes to propagate.

Reference: [IAP OAuth configuration](https://docs.cloud.google.com/iap/docs/custom-oauth-configuration).

## 5. Configure the address, DNS, and public certificate

After confirming these resource names are unused or owned by this installation:

```sh
gcloud compute addresses create "$ADDRESS_NAME" --global --ip-version=IPV4 \
  --network-tier=PREMIUM --project="$PROJECT"
export ADDRESS=$(gcloud compute addresses describe "$ADDRESS_NAME" \
  --global --project="$PROJECT" --format='value(address)')
```

Publish an A record mapping `NOTEBOOK_HOST` to `ADDRESS` in your DNS provider.
Do not point an unrelated existing AAAA record at another endpoint. Verify public
DNS resolves to the reserved address before continuing.

```sh
gcloud certificate-manager certificates create "$CERTIFICATE_NAME" \
  --domains="$NOTEBOOK_HOST" --project="$PROJECT"
gcloud certificate-manager maps create "$CERTIFICATE_MAP" --project="$PROJECT"
gcloud certificate-manager maps entries create notebooks \
  --map="$CERTIFICATE_MAP" --certificates="$CERTIFICATE_NAME" \
  --hostname="$NOTEBOOK_HOST" --project="$PROJECT"
```

This certificate uses load-balancer authorization, not a DNS challenge. It can
remain PROVISIONING until step 7 creates the HTTPS Gateway and attaches the map.
Do not wait for ACTIVE before installing the Gateway, and do not bypass TLS
verification to work around certificate failures.

## 6. Generate the deployment configuration

Set `CONTROL_PLANE_CIDR` from the verified endpoint in step 1. These commands write
only non-secret configuration. Keep it and rendered plans as private installation
records; do not copy the maintainers' ignored local configuration.

```sh
export CONTROL_PLANE_CIDR=YOUR_VERIFIED_PRIVATE_CONTROL_PLANE_IP/32
PROXY_IMAGE=$(gcloud artifacts docker images describe "$REGISTRY/gke-access-proxy:$TAG" \
  --project="$PROJECT" --format='value(image_summary.fully_qualified_digest)')
FRONTEND_IMAGE=$(gcloud artifacts docker images describe "$REGISTRY/gke-frontend:$TAG" \
  --project="$PROJECT" --format='value(image_summary.fully_qualified_digest)')
CONTROLLER_IMAGE=$(gcloud artifacts docker images describe "$REGISTRY/gke-controller:$TAG" \
  --project="$PROJECT" --format='value(image_summary.fully_qualified_digest)')
BACKEND_IMAGE=$(gcloud artifacts docker images describe "$REGISTRY/gke-backend:$TAG" \
  --project="$PROJECT" --format='value(image_summary.fully_qualified_digest)')
jq -n --arg cidr "$CONTROL_PLANE_CIDR" --arg host "$NOTEBOOK_HOST" \
  --arg certificateMap "$CERTIFICATE_MAP" --arg addressName "$ADDRESS_NAME" \
  --arg client "$IAP_CLIENT_ID" --arg secret "$IAP_SECRET_NAME" \
  --arg proxy "$PROXY_IMAGE" --arg frontend "$FRONTEND_IMAGE" \
  --arg controller "$CONTROLLER_IMAGE" --arg backend "$BACKEND_IMAGE" \
  '{controlPlaneCIDR:$cidr,hostname:$host,certificateMap:$certificateMap,
    addressName:$addressName,iapClientID:$client,iapSecretName:$secret,
    iapAudience:"",tenants:["team-a"],
    images:{proxy:$proxy,frontend:$frontend,controller:$controller,backend:$backend}}' \
  > gke/deployment.local.json
make -C gke plan CONFIG=deployment.local.json OUTPUT=rendered/bootstrap
```

The renderer rejects invalid image references and configuration. Plans are
immutable snapshots: choose a new output directory when retrying or changing
configuration, and substitute that path in subsequent commands.

## 7. Install with authentication closed

```sh
kubectl --context="$CONTEXT" apply --server-side --field-manager=notebooks-gke \
  -f gke/rendered/bootstrap/namespaces.json
kubectl --context="$CONTEXT" apply --server-side --field-manager=notebooks-gke \
  -f gke/rendered/bootstrap/isolation.json
```

Review existing additive NetworkPolicies: another allow rule can widen access.
For custom OAuth only, create the dedicated Secret without displaying the value:

```sh
jq -e '.web.client_secret | type == "string" and length > 0' "$OAUTH_FILE" >/dev/null
jq -jr '.web.client_secret' "$OAUTH_FILE" | \
  kubectl --context="$CONTEXT" -n kubeflow-workspaces create secret generic "$IAP_SECRET_NAME" \
    --from-file=client_secret=/dev/stdin
```

Creation deliberately refuses to overwrite a Secret. Use an approved credential
rotation procedure if it already exists. Then install CRDs before their instances:

```sh
jq '{apiVersion,kind,items:[.items[]|select(.kind=="CustomResourceDefinition")]}' \
  gke/rendered/bootstrap/applications.json | \
  kubectl --context="$CONTEXT" apply --server-side --field-manager=notebooks-gke -f -
kubectl --context="$CONTEXT" wait --for=condition=Established --timeout=2m \
  crd/workspaces.kubeflow.org crd/workspacekinds.kubeflow.org
kubectl --context="$CONTEXT" apply --server-side --field-manager=notebooks-gke \
  -f gke/rendered/bootstrap/applications.json
kubectl --context="$CONTEXT" apply --server-side --field-manager=notebooks-gke \
  -f gke/rendered/bootstrap/edge.json
```

An empty IAP audience intentionally makes the proxy fail startup. Do not disable
authentication or change readiness checks to make this bootstrap look healthy.

## 8. Discover the exact audience and make the proxy ready

The audience contains a numeric Google backend-service ID created by GKE. Match
the proxy Service's network endpoint group (NEG); never select the first backend
service in the project.

```sh
NEG_NAME=$(kubectl --context="$CONTEXT" -n kubeflow-workspaces get service gke-access-proxy \
  -o json | jq -er '.metadata.annotations["cloud.google.com/neg-status"] | fromjson | .network_endpoint_groups["8080"]')
MATCHED_BACKEND=$(gcloud compute backend-services list --global --project="$PROJECT" \
  --format='json(name,id,backends,iap.enabled)' | jq -ce --arg neg "$NEG_NAME" \
  '[.[] | select(any(.backends[]?; .group | endswith("/networkEndpointGroups/"+$neg)))]
   | if length==1 then .[0] else error("Expected exactly one proxy backend") end')
export BACKEND_SERVICE=$(jq -er '.name' <<< "$MATCHED_BACKEND")
BACKEND_ID=$(jq -er '.id' <<< "$MATCHED_BACKEND")
jq -e '.iap.enabled == true' <<< "$MATCHED_BACKEND" >/dev/null
PROJECT_NUMBER=$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')
export IAP_AUDIENCE=/projects/${PROJECT_NUMBER}/global/backendServices/${BACKEND_ID}
jq --arg audience "$IAP_AUDIENCE" '.iapAudience=$audience' \
  gke/deployment.local.json > gke/rendered/deployment.ready.json
make -C gke plan CONFIG=rendered/deployment.ready.json OUTPUT=rendered/ready
kubectl --context="$CONTEXT" apply --server-side --field-manager=notebooks-gke \
  -f gke/rendered/ready/applications.json
kubectl --context="$CONTEXT" -n kubeflow-workspaces rollout restart deployment/gke-access-proxy
for component in workspaces-controller workspaces-backend workspaces-frontend gke-access-proxy; do
  kubectl --context="$CONTEXT" -n kubeflow-workspaces rollout status deployment/"$component" --timeout=5m
done
```

If GKE has not attached the NEG or policy yet, inspect status and repeat discovery
after reconciliation. Backend recreation requires rediscovering the audience.
ConfigMap changes require a proxy restart because its configuration is read at
startup.

Require Gateway Programmed, HTTPRoute Accepted/ResolvedRefs, both backend and
health-check policies Attached, webhook Certificate Ready with CA injection, and
public Certificate Manager certificate ACTIVE:

```sh
kubectl --context="$CONTEXT" -n kubeflow-workspaces get \
  gateway,httproute,gcpbackendpolicy,healthcheckpolicy,certificate
gcloud certificate-manager certificates describe "$CERTIFICATE_NAME" \
  --project="$PROJECT" --format='yaml(managed)'
gcloud compute backend-services get-health "$BACKEND_SERVICE" \
  --global --project="$PROJECT"
curl --silent --show-error --output /dev/null --write-out 'HTTP %{http_code}\n' \
  "https://$NOTEBOOK_HOST/workspaces/"
```

An anonymous request must reach Google login or denial, not notebook content.
Never use `--insecure`. Healthy endpoints alone do not prove authorization.

## 9. Admit your first user and install the notebook template

Grant admission only to this application's backend:

```sh
gcloud iap web add-iam-policy-binding --project="$PROJECT" \
  --resource-type=backend-services --service="$BACKEND_SERVICE" \
  --member="user:$PILOT_USER" --role=roles/iap.httpsResourceAccessor --condition=None
```

The repository pilot bindings contain the maintainers' account. **Do not apply
them directly for a customer.** Render a customer plan that replaces all User
subjects with your chosen account. `kubectl create --dry-run=client` below only
converts the rendered manifests to JSON objects; `jq -s` assembles a List.
Neither command installs resources.

```sh
kubectl kustomize --load-restrictor=LoadRestrictionsNone gke/manifests/pilot | \
  kubectl --context="$CONTEXT" create --dry-run=client -f - -o json | \
  jq -s --arg user "$PILOT_USER" \
    '{apiVersion:"v1",kind:"List",items:map(if .kind=="RoleBinding" or .kind=="ClusterRoleBinding"
      then .subjects |= map(if .kind=="User" then .name=$user else . end)
      else . end)}' > gke/rendered/ready/customer-pilot.json
jq -e --arg user "$PILOT_USER" \
  '[.items[].subjects[]? | select(.kind=="User") | .name] | length>0 and all(.==$user)' \
  gke/rendered/ready/customer-pilot.json >/dev/null
kubectl --context="$CONTEXT" apply --server-side --field-manager=notebooks-gke-pilot \
  -f gke/manifests/pilot/admission.yaml
kubectl --context="$CONTEXT" get validatingadmissionpolicy notebooks-gke-pilot-workspaces \
  -o json | jq '.status'
kubectl --context="$CONTEXT" apply --server-side --dry-run=server \
  --field-manager=notebooks-gke-pilot -f gke/rendered/ready/customer-pilot.json
kubectl --context="$CONTEXT" apply --server-side --field-manager=notebooks-gke-pilot \
  -f gke/rendered/ready/customer-pilot.json
```

Require the admission policy's observed generation to match and no type-checking
warnings before granting access. The live server dry run exercises the webhook,
not just YAML parsing. Do not force conflicts or bypass admission on failure.

This installs narrow workspace/PVC permissions, read-only kind/storage discovery,
a tokenless notebook ServiceAccount, compute/storage quota, restricted Pod
Security, and admission restrictions against Secret mounts, user pod metadata,
and unreviewed WorkspaceKinds. It grants neither PVC deletion nor administrative
access. The dedicated `notebooks-gke-rwo` class opts into upstream's storage UI
with `notebooks.kubeflow.org/can-use=true` and uses GKE balanced PDs with Retain.

## 10. Run a notebook

1. Visit `https://NOTEBOOK_HOST/workspaces/` and sign in as the admitted user.
2. Select `team-a`, then **Create workspace**.
3. Choose **JupyterLab Notebook**, **JupyterLab SciPy**, and **Pilot CPU**.
4. Enter a workspace name and attach a new home volume: `notebooks-gke-rwo`,
   ReadWriteOnce, 10 GiB, read-write. Do not attach Secrets or extra volumes.
5. Submit, wait for Running, then choose **Connect > JupyterLab**. Permit the
   application popup or open the connection URL directly:
   `https://NOTEBOOK_HOST/workspace/connect/team-a/WORKSPACE_NAME/jupyterlab/lab`.
6. Run a Python cell such as `print(6 * 7)`, save the notebook, and run `id -u` in
   a JupyterLab terminal. The expected notebook UID is `1000`.
7. Stop and start the workspace, then verify the saved file remains. Stop ends
   kernel/terminal processes; it does not preserve process memory.

Do not confuse the UI's current `kubeflow-user` placeholder with the verified
identity. The backend's workspace audit record carries the Google email. Known
UI and activity/culling gaps are listed in the codelab acceptance record.

## Enroll additional users

Admission and authorization are separate layers by design: the IAP binding only
admits an identity to the application, and Kubernetes RBAC decides what it may
do. Keep the IAP layer coarse so it is configured once, and enroll users
day-to-day in RBAC only.

Bind IAP to a Google Group instead of individual users, then enrollment on the
Google side becomes group membership managed outside IAM:

```sh
export USERS_GROUP=notebooks-users@YOUR_DOMAIN
gcloud iap web add-iam-policy-binding --project="$PROJECT" \
  --resource-type=backend-services --service="$BACKEND_SERVICE" \
  --member="group:$USERS_GROUP" --role=roles/iap.httpsResourceAccessor --condition=None
```

The group must exist in your organization before binding. After verifying group
members can sign in, remove any earlier per-user bindings so the group is the
single admission list.

Per user, add a `User` subject to the tenant RoleBinding and the discovery
ClusterRoleBinding:

```sh
export NEW_USER=someone@YOUR_DOMAIN
kubectl --context="$CONTEXT" -n team-a patch rolebinding notebooks-gke-pilot --type=json \
  -p '[{"op":"add","path":"/subjects/-","value":{"kind":"User","apiGroup":"rbac.authorization.k8s.io","name":"'"$NEW_USER"'"}}]'
kubectl --context="$CONTEXT" patch clusterrolebinding notebooks-gke-pilot-discovery --type=json \
  -p '[{"op":"add","path":"/subjects/-","value":{"kind":"User","apiGroup":"rbac.authorization.k8s.io","name":"'"$NEW_USER"'"}}]'
```

Know what you are granting:

- Users sharing a tenant namespace can reach **each other's** workspaces: the
  workspace check is namespace-scoped RBAC, not per-owner. Separate tenants need
  their own namespace, bindings, quota, and an admission-policy update.
- The tenant ResourceQuota is shared; two users will feel the pilot's limits.
- Do not use `Group` subjects in RoleBindings yet: the access proxy's
  `SubjectAccessReview` carries only the verified email, so group-based RBAC
  does not take effect through the proxy even where GKE Google Groups for RBAC
  is enabled. Group-aware authorization is recorded as future work in
  [DESIGN.md](DESIGN.md#future-work).
- Revocation is the mirror image and must cover **both layers plus desktop
  grants**: remove the RoleBinding subject, remove the group/IAM member, and
  revoke the user's desktop connections — removing IAP admission alone does not
  invalidate previously issued desktop tokens.

## VS Code Jupyter extension

The optional desktop endpoint accepts standard Jupyter connection tokens. It uses
Microsoft's unmodified Jupyter extension (`ms-toolsai.jupyter`), not a custom
extension or an administrator tunnel. IAP continues to protect browser access and
token issuance. The separate desktop backend verifies Kubernetes-minted tokens
itself; it has no route to dashboard APIs or token issuance.

### Enable the desktop endpoint

Choose a second, dedicated DNS name, such as `connect.YOUR_DOMAIN`, pointing at
the same reserved Gateway address. Create a Certificate Manager certificate and
map entry for that name in the existing installation-owned map. Do not reuse
another application's names or alter the existing browser certificate.

```sh
export DESKTOP_HOST=connect.YOUR_DOMAIN
export DESKTOP_CERTIFICATE=notebooks-desktop
gcloud certificate-manager certificates create "$DESKTOP_CERTIFICATE" \
  --domains="$DESKTOP_HOST" --project="$PROJECT"
gcloud certificate-manager maps entries create notebooks-desktop \
  --map="$CERTIFICATE_MAP" --certificates="$DESKTOP_CERTIFICATE" \
  --hostname="$DESKTOP_HOST" --project="$PROJECT"
jq --arg host "$DESKTOP_HOST" '.desktopHostname=$host' \
  gke/rendered/deployment.ready.json > gke/rendered/deployment.desktop.json
make -C gke plan CONFIG=rendered/deployment.desktop.json OUTPUT=rendered/desktop
for stage in namespaces isolation applications edge; do
  kubectl --context="$CONTEXT" apply --server-side --field-manager=notebooks-gke \
    -f "gke/rendered/desktop/$stage.json"
done
kubectl --context="$CONTEXT" -n kubeflow-workspaces rollout restart deployment/gke-access-proxy
kubectl --context="$CONTEXT" -n kubeflow-workspaces rollout status deployment/gke-access-proxy --timeout=5m
kubectl --context="$CONTEXT" -n kubeflow-workspaces wait gateway/notebooks \
  --for=condition=Programmed --timeout=10m
gcloud certificate-manager certificates describe "$DESKTOP_CERTIFICATE" \
  --project="$PROJECT" --format='yaml(managed)'
```

Build a proxy image containing this feature before rendering if upgrading an older
installation. The desktop hostname must differ from the browser hostname. Require
the new certificate to be ACTIVE, the desktop policy Attached, and the new backend
healthy. Its IAP setting is false by design, while the browser backend remains
IAP-enabled. Desktop access logging must be disabled because Jupyter WebSocket
URLs carry tokens. Verify this in the effective Google backend configuration;
also review any separately enabled logging/WAF/proxy products for query redaction.

### Connect from VS Code

1. Open `https://NOTEBOOK_HOST/workspaces/connections` and sign in with Google.
2. Select your running workspace, its port (normally `jupyterlab`), and a requested
  duration. Click **Generate connection** and check the displayed expiry, then
  **Copy URL**. Kubernetes may shorten the request. The token is returned once, not
   retained as plaintext in Kubernetes. Treat the entire URL as a password.
3. In VS Code, open a notebook and choose **Select Kernel > Select Another
   Kernel > Existing Jupyter Server**. Paste the copied URL including `?token=`.
4. Select the remote Python kernel and run the validation notebook linked below.
   Do not add `/lab` or `/api` to the generated server URL.
5. After use, click **Revoke** on the connection page and remove the saved server
   entry in VS Code. Generate a new connection after expiry; there is no automatic
   token renewal in this first version.

Do not paste connection URLs into chat, logs, tickets, or source files. No Google
password, Google OAuth client secret, `gcloud`, or Kubernetes credential is needed
by the notebook user. If a password prompt appears, cancel it and check that you
used the generated desktop URL, not the IAP-protected browser hostname.

### Configure connection lifetimes

Administrators set these integer values in the deployment JSON:

| Field | Default | Meaning |
| --- | --- | --- |
| `connectionTokenDefaultSeconds` | `86400` (24 hours) | Initially selected duration; also used when an API request omits `durationSeconds` or sets it to zero |
| `connectionTokenMaxSeconds` | `604800` (7 days) | Largest duration users may request |

The policy must satisfy `600 <= default <= maximum <= 2592000` seconds
(10 minutes through 30 days). Omitted or zero configuration fields select the
defaults, not unlimited validity. A larger maximum requires an explicit
administrator choice; invalid policies fail rendering and proxy startup. Invalid
user durations return HTTP 400 without creating a grant. The duration menu uses
the policy reported by the server, including custom default and maximum values.

For example, to use a 24-hour default and seven-day maximum for an existing desktop
deployment:

```sh
jq '.connectionTokenDefaultSeconds=86400 | .connectionTokenMaxSeconds=604800' \
  gke/rendered/deployment.desktop.json > gke/rendered/deployment.lifetimes.json
make -C gke plan CONFIG=rendered/deployment.lifetimes.json OUTPUT=rendered/lifetimes
kubectl --context="$CONTEXT" apply --server-side --field-manager=notebooks-gke \
  -f gke/rendered/lifetimes/applications.json
kubectl --context="$CONTEXT" -n kubeflow-workspaces rollout restart deployment/gke-access-proxy
kubectl --context="$CONTEXT" -n kubeflow-workspaces rollout status deployment/gke-access-proxy --timeout=5m
```

Use a proxy image built with lifetime-policy support when upgrading. For direct
executable configuration, the corresponding environment variables are
`CONNECTION_TOKEN_DEFAULT_SECONDS` and `CONNECTION_TOKEN_MAX_SECONDS`, or flags
`--connection-token-default-seconds` and `--connection-token-max-seconds`.
Changing the policy affects new grants only. Existing signed tokens and grant
records keep their original expiry; revoke them explicitly if necessary.

**GKE can impose a shorter lifetime.** In the tested cluster, TokenRequest granted
24 hours when asked for one day, but shortened both seven-day and 30-day requests
to **172800 seconds (48 hours)**. This is observed cluster behavior, not a universal
promise about all GKE configurations. The proxy records and returns the earlier
of the requested deadline and Kubernetes' actual expiration. Always use the
displayed expiry; increasing the configured maximum cannot override the issuer.
Operators can repeat the credential-redacting probe with:

```sh
bash gke/scripts/test-connection-tokens.sh "$CONTEXT" kubeflow-workspaces
```

The probe needs operator permission to create/delete a temporary ServiceAccount,
request its tokens, and submit TokenReviews. It reports expiry metadata, discards
token values, and cleans up its temporary files and account.

### Long-running kernels and replacement URLs

Token lifetime is not kernel lifetime. Grant expiry/revocation closes authorized
desktop connections; it does not issue a kernel shutdown or stop the workspace.
The pod, Jupyter idle policies, workspace culling, and resource availability can
still stop a kernel independently. A long-lived token also does not guarantee an
uninterrupted WebSocket: Gateway connection limits and network interruptions are
separate. This change does not alter the existing backend timeout configuration.

Before the displayed expiry, generate a replacement URL for the same workspace.
In VS Code add that URL as an existing server and select the existing kernel when
available, rather than starting a new kernel and assuming variables persist. Keep
the old grant until the replacement is working, then revoke it. There is no
automatic renewal or sliding expiry in this version. On the tested GKE cluster,
weeks-long work requires credential replacement at least every 48 hours.

A live public HTTP/WebSocket test reconnected with a replacement token to the
same kernel ID after revoking the old token and read an existing variable as 42.
The old token returned 403. This verifies server-side state preservation, not
automatic VS Code renewal. Multi-day wall-clock operation and the VS Code
existing-kernel selection workflow across token expiry remain validation gates.

### Token scope and revocation

Each connection has its own ServiceAccount in the protected
`notebooks-connections` namespace, no RBAC grants, and no workload token mount.
TokenRequest uses only the desktop audience and requests the selected duration
within administrator limits. The grant expires at the earlier of the requested
deadline and Kubernetes' returned expiration. The
ServiceAccount stores the original user and exact workspace UID/port, not the
token. Tokens do not grant workspace creation or general Kubernetes API access.

After validation, the desktop proxy strips the connection token and Referer before
forwarding and supplies a matching per-request XSRF cookie/header pair for Jupyter.
Jupyter's XSRF check remains enabled. The browser listener still requires its
original IAP identity and exact Origin; token-authenticated desktop requests may
omit Origin, while a supplied different origin is rejected.

Every desktop request requires TokenReview with the expected audience, an
authoritative ServiceAccount UID/expiry lookup, and the original user's current
workspace authorization. The original workspace UID must still match. Deleting
and recreating a workspace or connection does not restore an old grant.

Revoke deletes the grant ServiceAccount. GKE was observed to accept a deleted
token briefly through TokenReview; the extra authoritative lookup rejects it
independently of that cache. Active desktop WebSockets are checked every 15 seconds
with a 10-second validation timeout and are closed on revocation, expiry, lookup
failure, or lost workspace access. In-flight non-WebSocket requests have an expiry
deadline but are not continuously reauthorized. Browser WebSockets retain their
previous handshake-only behavior.

Expired grants are cleaned up periodically. Each user is limited to ten active
grants at issuance time; concurrent issuance can race that check, while a namespace
quota bounds total records. Issuance and request rates are bounded per proxy
replica. Removing an IAP admission grant alone does not invalidate a previously
issued desktop token: revoke the connection or remove workspace RBAC as well.

### Validation status

The original direct browser-host URL was tested with Jupyter extension 2025.9.1
and failed at IAP authentication. On 2026-09-12, the new generated desktop URL
successfully selected a remote Python kernel in that unmodified VS Code extension.
Cell execution reported the actual GKE pod `ws-gke-pilot-2qdwp-0`, interpreter
`/opt/conda/bin/python`, and result `42`. A separate public token-based HTTP and
WebSocket test created a kernel, executed `DESKTOP_KERNEL_OK 42`, revoked the
grant, observed the active WebSocket close, and received 403 on further requests.
Anonymous desktop discovery returned 401 over verified TLS. Browser IAP remained
enabled, and only the desktop backend had IAP and access logging disabled.

The implementation also has automated
audience, grant/workspace UID, expiry, RBAC, HTTP routing, credential-stripping,
and live local WebSocket-revocation tests. A GKE probe confirmed that the dedicated
audience cannot authenticate to the Kubernetes API and that authoritative grant
checks reject deleted/recreated records despite cached TokenReview results.
The configurable-duration API/UI and actual GKE lifetime limits were tested, as
was same-kernel reconnection using a replacement token. Full-duration wall-clock
expiry, VS Code interrupt/restart/reconnect after revocation, and scale/load
behavior have not been validated live. See the codelab's latest
checkpoint for the deployment record.

Use [vscode_validation.ipynb](vscode_validation.ipynb) when an authenticated server
connection is available. Run its remote-environment check before other cells and
compare the reported hostname with the actual workspace pod. Local execution,
`kubectl exec`, browser execution, and administrator port-forwarding are not
evidence that the VS Code extension can use the public user-facing endpoint.
The notebook document remains in the VS Code workspace; Python file I/O runs
against the remote notebook filesystem. These are separate storage locations.

For diagnostics, record the selected server's origin/path, extension versions,
and sanitized errors from **View > Output > Jupyter**. With Remote SSH, identify
which extension host makes the connection and verify that host can reach the
endpoint. Never publish a raw server URL containing a token, request headers,
cookies, or OAuth redirects with state/code parameters.

References: [VS Code remote notebooks](https://code.visualstudio.com/docs/datascience/jupyter-notebooks#_connect-to-a-remote-jupyter-server)
and [Kubernetes ServiceAccount tokens](https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/).

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Google login works but IAP says Access Denied | Exact backend grant, organization membership for managed OAuth, and custom OAuth propagation; do not broaden IAM blindly |
| OAuth redirect mismatch | Exact `iap.googleapis.com` callback with this client ID |
| Certificate stays PROVISIONING | Public DNS, HTTPS Gateway address, attached certificate map, and authorization failure reason |
| Proxy crashes before audience discovery | Expected fail-closed bootstrap; complete step 8 |
| Webhook admission times out | Default-deny and Konnectivity-agent TCP 9443 allowance; certificate readiness alone does not prove connectivity |
| No namespace appears | The verified email needs `list workspaces` in `team-a`; IAP access does not grant RBAC |
| Storage classes are disabled | Use the dedicated class with `notebooks.kubeflow.org/can-use=true` |
| Notebook remains Pending | Node resources, image pull permissions, PVC provisioning, quota, Pod Security, and events |
| Start dialog suggests a redirect to `undefined` | Known UI issue; plain Start retains current options; do not accept an undefined update |
| Browser tab takes too long to restore after restart | Check pod readiness and file APIs; foreground layout restoration has not been fully validated |

Inspect conditions and error messages without printing Secrets, access tokens,
OAuth state values, or cookies. Keep notebook data when investigating failures.

## Security and operational limits

The pilot has verified browser login, kernel and terminal WebSockets, file
persistence across pause/resume, selected cross-tenant/forged-header denials,
admission/RBAC restrictions, and direct ingress isolation from an unrelated pod.
Repeat these positive and negative checks in each customer's environment; the
maintainers' test is not a certification of your deployment.

Notebook HTML/JavaScript shares an origin with the application API. Do not invite
untrusted users on the assumption that origin checks isolate their content.
Namespace-wide permissions also imply trust among users sharing a namespace.
Student enrollment, per-user namespaces, Google group-to-RBAC synchronization,
and non-Google federation are not implemented by this single-user guide.

RBAC is checked on each request/WebSocket handshake; existing WebSockets are not
terminated on revocation. Live revocation, deletion/recreation races, broad
filename/encoding compatibility, automatic culling, upgrades, and uninstall need
further validation. Review images and dependencies before wider use.

For upgrades, retain previous configuration/plans, build new pinned images, review
the newly rendered resources, and repeat acceptance tests. Never reapply a stale
isolation plan that removes the Konnectivity allowance. Preserve PVCs.

For removal, first back up files and inventory ownership. Remove the single
backend's IAP grant, then the dedicated HTTPRoute/Gateway, and wait for GKE to
remove its load balancer. Delete only installation-owned edge resources afterward.
Do not delete shared cert-manager, the cluster, registry, CRDs, or tenant namespaces
as automatic cleanup. Retain means notebook disks can survive PVC deletion and
continue billing; release them only after explicit data-deletion approval.
