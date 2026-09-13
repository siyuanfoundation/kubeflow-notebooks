# Deploy Notebooks on an existing GKE cluster

For a new installation in your own project, use the [customer setup guide](USER_GUIDE.md).
This document preserves the pilot-specific commands and dated deployment evidence.

This codelab deploys unchanged upstream Notebooks behind GKE Gateway, Google-managed
HTTPS, IAP, and the integration access proxy. Run commands from the repository root
unless a step says otherwise. This is an experimental deployment, not a supported
production installer. All integration files are under `gke/`.

## 1. Select and inspect the target

Prerequisites: authenticated `gcloud`, `kubectl`, Docker, Go, and `jq`. The operator
needs permission to manage the target cluster and its dedicated cloud resources.
Do not print access tokens, OAuth secrets, or Kubernetes Secret contents.

```sh
export PROJECT=ipv6-project-379110
export CLUSTER=kubeflow-notebooks
export LOCATION=us-central1-c
export REGION=us-central1
export CONTEXT=gke_${PROJECT}_${LOCATION}_${CLUSTER}
gcloud container clusters describe "$CLUSTER" --project="$PROJECT" \
  --zone="$LOCATION" --format='yaml(networkConfig,ipAllocationPolicy,addonsConfig,workloadIdentityConfig)'
kubectl --context="$CONTEXT" get namespaces,gatewayclasses
gcloud artifacts repositories list --project="$PROJECT"
gcloud certificate-manager maps list --project="$PROJECT"
gcloud certificate-manager certificates list --project="$PROJECT"
gcloud dns managed-zones list --project="$PROJECT"
```

If the context is missing, obtain it explicitly:

```sh
gcloud container clusters get-credentials "$CLUSTER" \
  --project="$PROJECT" --zone="$LOCATION"
```

Expected: a VPC-native cluster with `ADVANCED_DATAPATH`, HTTP load balancing,
and Workload Identity. Dataplane V2 enforces NetworkPolicy independently of the
legacy `networkPolicyConfig.disabled` field. Do not modify a different cluster or
reuse certificates, DNS records, addresses, or IAM bindings owned by another app.

## 2. Enable the managed Gateway controller

```sh
gcloud container clusters update "$CLUSTER" --project="$PROJECT" \
  --zone="$LOCATION" --gateway-api=standard --quiet
kubectl --context="$CONTEXT" wait \
  gatewayclass/gke-l7-global-external-managed \
  --for=condition=Accepted --timeout=10m
kubectl --context="$CONTEXT" get crd \
  gateways.gateway.networking.k8s.io \
  httproutes.gateway.networking.k8s.io \
  gcpbackendpolicies.networking.gke.io \
  healthcheckpolicies.networking.gke.io
```

If the GatewayClass does not exist yet, the wait command can return NotFound;
check the cluster update operation and rerun the check after reconciliation.
Do not replace GKE-managed Gateway CRDs with an upstream experimental bundle.

## 3. Build and publish the four images

These are pilot images, not a scanned release. Use a unique tag for each build;
deployments use the registry digest, never the tag. Controller and backend use
unchanged upstream Dockerfiles with different build contexts.

```sh
export REGISTRY=${REGION}-docker.pkg.dev/${PROJECT}/kubeflow-notebooks
export TAG=pilot-20260911
docker build --platform=linux/amd64 -t "$REGISTRY/gke-access-proxy:$TAG" gke
docker build --platform=linux/amd64 -f gke/frontend.Dockerfile \
  -t "$REGISTRY/gke-frontend:$TAG" .
docker build --platform=linux/amd64 -f workspaces/controller/Dockerfile \
  -t "$REGISTRY/gke-controller:$TAG" workspaces/controller
docker build --platform=linux/amd64 -f workspaces/backend/Dockerfile \
  -t "$REGISTRY/gke-backend:$TAG" workspaces
gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
for component in access-proxy frontend controller backend; do
  docker push "$REGISTRY/gke-$component:$TAG"
  gcloud artifacts docker images describe "$REGISTRY/gke-$component:$TAG" \
    --project="$PROJECT" --format='value(image_summary.fully_qualified_digest)'
done
```

Expected: four successful builds and four immutable references containing
`@sha256:`. Do not use the local Docker image ID as the registry manifest digest.
The frontend sets `DEPLOYMENT_MODE=standalone` and `PUBLIC_PATH=/workspaces/`.

The initial frontend build succeeded with Node 22 but upstream declares Node 20
and npm 11.10. npm reported 70 advisories (4 low, 32 moderate, 32 high, 2 critical).
These counts include the build dependency tree and do not establish runtime
exploitability. Triage them and resolve the toolchain mismatch before a supported
release; do not run `npm audit fix` against upstream as part of installation.
The final frontend image contains static assets and nginx, not the Node builder.

## 4. Prepare Google login

An OAuth client identifies this application to Google's login service. Its client
ID is public; its client secret is a credential used by IAP. Neither grants a user
access. IAP IAM grants admission, and Kubernetes RBAC separately grants notebook
permissions. The initial pilot user is `aojea@google.com`; do not grant everyone
in the organization access.

For organization-internal users, use Google-managed OAuth: leave both
`iapClientID` and `iapSecretName` empty in the deployment configuration. No OAuth
client creation or Secret is needed. The installed GKE policy schema explicitly
documents this default when both fields are omitted; the renderer omits both
while keeping `iap.enabled: true`. Policy attachment and actual browser login
must still be verified. Never supply only one of the two fields.

For external users, or clusters without this capability, use a custom OAuth
client. Do not alter the GKE-owned backend service manually to bypass its policy.

This pilot requires custom OAuth: the project belongs to organization
`aojea.joonix.net` (`153692503966`), while the pilot account is `aojea@google.com`.
Google-managed OAuth only supports users within the project's organization.
An IAP IAM grant does not remove that identity-provider restriction. The live
browser test returned Access Denied even though IAM Policy Troubleshooter
reported `CAN_ACCESS`, with an allowed grant and no deny policy.

Custom OAuth operator steps, in project `ipv6-project-379110`:

1. Open [Google Auth Platform branding](https://console.cloud.google.com/auth/branding?project=ipv6-project-379110).
   If not configured, configure the app name and support/contact email. Inspect
   existing project branding before changing it, since other apps may share it.
2. Choose an Internal audience only if this project's organization includes the
   intended user. Otherwise configure External testing with the specific pilot
   user and follow any organization approval requirements. Do not publish broadly.
3. Open [OAuth clients](https://console.cloud.google.com/auth/clients?project=ipv6-project-379110),
   create a **Web application** named `Notebooks GKE pilot`, and retain the client ID.
4. Add this exact authorized redirect URI, replacing `CLIENT_ID` with that ID:
   `https://iap.googleapis.com/v1/oauth/clientIds/CLIENT_ID:handleRedirect`.
   The redirect goes to IAP, not to a notebook callback endpoint.
5. Store the client secret in a private local file outside this repository. The
   file must contain only the secret, without an added newline; restrict access
   to the operator. Never paste it in chat or place it in shell command arguments.
6. Once the system namespace exists, create the Secret from that file:

```sh
kubectl --context="$CONTEXT" -n kubeflow-workspaces create secret generic iap-oauth \
  --from-file=client_secret=/absolute/private/path/iap-client-secret
```

If the Secret already exists, verify its ownership and use the team's credential
rotation procedure rather than overwriting it. Only provide the client ID and the
Secret name to the deployment configuration. No Google-account password is needed.

References: [Gateway IAP policy](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-gateway-resources#configure_iap)
and [IAP OAuth configuration](https://docs.cloud.google.com/iap/docs/custom-oauth-configuration).

## 5. Reserve an address and request edge TLS

For this pilot, `sslip.io` can derive DNS from a public address, for example
`notebooks.203.0.113.10.sslip.io`. It is a third-party DNS dependency, not a
production domain owned by the operator. Use an owned domain for a supported
installation. Certificate issuance remains a gate, not an assumption.

```sh
gcloud services enable iap.googleapis.com --project="$PROJECT"
gcloud compute addresses create notebooks-gke-global --global \
  --ip-version=IPV4 --network-tier=PREMIUM --project="$PROJECT"
export ADDRESS=$(gcloud compute addresses describe notebooks-gke-global \
  --global --project="$PROJECT" --format='value(address)')
export HOSTNAME=notebooks.${ADDRESS}.sslip.io
getent ahostsv4 "$HOSTNAME"
gcloud certificate-manager certificates create notebooks-gke \
  --domains="$HOSTNAME" --project="$PROJECT"
gcloud certificate-manager maps create notebooks-gke --project="$PROJECT"
gcloud certificate-manager maps entries create notebooks-gke \
  --map=notebooks-gke --certificates=notebooks-gke \
  --hostname="$HOSTNAME" --project="$PROJECT"
```

Inspect existing named resources before creating; if they already exist, verify
that they belong to this installation and match its configuration. Do not replace
another deployment's resources. The reserved IP and eventual load balancer incur
charges. The previous regional `kubeflow-gateway` address is not reused or deleted.

Without `--dns-authorizations`, this certificate uses load balancer authorization.
It stays provisioning until the Gateway attaches the map to a reachable HTTPS
load balancer and DNS resolves to that load balancer. Do not wait for certificate
activation before installing the Gateway. No DNS challenge record is required.
If issuance fails due to CA policy, rate limits, or public DNS restrictions, stop
and use an owned domain; do not bypass TLS verification.

Reference: [Certificate Manager load balancer authorization](https://docs.cloud.google.com/certificate-manager/docs/deploy-google-managed-lb-auth).

## 6. Render and install in stages

First install cert-manager, only if no existing installation is present. It is
used only for the internal admission webhook. This pilot pins `v1.21.2`:

```sh
mkdir -p gke/bin
curl -fsSL https://github.com/cert-manager/cert-manager/releases/download/v1.21.2/cert-manager.yaml \
  -o gke/bin/cert-manager-v1.21.2.yaml
printf '%s  %s\n' e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f \
  gke/bin/cert-manager-v1.21.2.yaml | sha256sum --check
kubectl --context="$CONTEXT" apply --server-side \
  --field-manager=notebooks-gke-platform -f gke/bin/cert-manager-v1.21.2.yaml
for deployment in cert-manager cert-manager-webhook cert-manager-cainjector; do
  kubectl --context="$CONTEXT" -n cert-manager rollout status \
    "deployment/$deployment" --timeout=5m
done
```

Fill `gke/deployment.local.json` using `gke/deployment.example.json` as the schema:
set the hostname from step 5, certificate map `notebooks-gke`, address
`notebooks-gke-global`, the dedicated OAuth client ID and Secret name from step 4,
tenant `team-a`, and the four registry digests. Leave both OAuth fields empty only
for users eligible for Google-managed OAuth. Initially leave `iapAudience` empty.
Do not substitute a fake OAuth client ID just to advance the renderer.

```sh
make -C gke plan CONFIG=deployment.local.json OUTPUT=rendered/initial
kubectl --context="$CONTEXT" apply --server-side \
  --field-manager=notebooks-gke -f gke/rendered/initial/namespaces.json
kubectl --context="$CONTEXT" apply --server-side \
  --field-manager=notebooks-gke -f gke/rendered/initial/isolation.json
```

For custom OAuth only, create the Secret from step 4 now. Review existing additive NetworkPolicies
before continuing. Apply the application CRDs separately before dependent resources:

```sh
jq '{apiVersion,kind,items:[.items[] | select(.kind=="CustomResourceDefinition")]}' \
  gke/rendered/initial/applications.json | \
  kubectl --context="$CONTEXT" apply --server-side --field-manager=notebooks-gke -f -
kubectl --context="$CONTEXT" wait --for=condition=Established --timeout=2m \
  crd/workspaces.kubeflow.org crd/workspacekinds.kubeflow.org
kubectl --context="$CONTEXT" apply --server-side \
  --field-manager=notebooks-gke -f gke/rendered/initial/applications.json
kubectl --context="$CONTEXT" apply --server-side \
  --field-manager=notebooks-gke -f gke/rendered/initial/edge.json
```

Stop on ownership conflicts; do not use `--force-conflicts`. Empty IAP audience
causes the access proxy to exit with no ready endpoints. This bootstrap state is
intentional and must not be replaced with an authentication bypass.

## 7. Discover the audience and verify readiness

```sh
kubectl --context="$CONTEXT" -n kubeflow-workspaces get \
  gateway,httproute,gcpbackendpolicy,healthcheckpolicy
kubectl --context="$CONTEXT" -n kubeflow-workspaces get service gke-access-proxy \
  -o json | jq '.metadata.annotations["cloud.google.com/neg-status"] | fromjson'
gcloud compute backend-services list --global --project="$PROJECT" \
  --format='json(name,id,backends.group,iap.enabled)'
```

Match the Service's NEG names against the backend service's `backends[].group`.
Require exactly one matching global backend service; never pick the first service
in the project. Check that `iap.enabled` is true and the GCPBackendPolicy reports
Attached. The audience is `/projects/PROJECT_NUMBER/global/backendServices/ID`.
Obtain the numeric project number with `gcloud projects describe "$PROJECT"
--format='value(projectNumber)'`. Keep the matched backend name for the IAP grant.

Set that exact audience in the local configuration, then:

```sh
make -C gke plan CONFIG=deployment.local.json OUTPUT=rendered/ready
kubectl --context="$CONTEXT" apply --server-side \
  --field-manager=notebooks-gke -f gke/rendered/ready/applications.json
kubectl --context="$CONTEXT" -n kubeflow-workspaces rollout restart deployment/gke-access-proxy
kubectl --context="$CONTEXT" -n kubeflow-workspaces wait \
  certificate/workspaces-serving-cert --for=condition=Ready --timeout=5m
for deployment in workspaces-controller workspaces-backend workspaces-frontend gke-access-proxy; do
  kubectl --context="$CONTEXT" -n kubeflow-workspaces rollout status \
    "deployment/$deployment" --timeout=5m
done
kubectl --context="$CONTEXT" -n kubeflow-workspaces wait \
  gateway/notebooks --for=condition=Programmed --timeout=10m
gcloud certificate-manager certificates describe notebooks-gke \
  --project="$PROJECT" --format='yaml(managed)'
```

The certificate must be ACTIVE, webhook CA injection populated, route accepted
with resolved references, and policies attached. Validate an anonymous HTTPS request
without `--insecure`: it must reach IAP login or denial, never notebook/API content.

## 8. Grant the pilot user and run acceptance tests

Only after matching the backend service, grant access to that single IAP resource:

```sh
export BACKEND_SERVICE=REPLACE_WITH_VERIFIED_BACKEND_SERVICE_NAME
gcloud iap web add-iam-policy-binding --project="$PROJECT" \
  --resource-type=backend-services --service="$BACKEND_SERVICE" \
  --member=user:aojea@google.com --role=roles/iap.httpsResourceAccessor
```

Do not grant project-wide IAP access. The single-user pilot overlay in
`gke/manifests/pilot/` grants `aojea@google.com` workspace lifecycle and PVC
creation/read permissions in `team-a`, plus read-only WorkspaceKind and
StorageClass discovery. It does not grant namespace listing, Secret access,
pod or Service creation, policy modification, role management, or PVC deletion.
Do not substitute upstream `admin`/`edit` roles.

Install admission restrictions before user permissions. This overlay is specific
to the pilot user and namespace; review it before adapting to another installation.
It reuses the upstream JupyterLab sample through a patch, not a copied template.

```sh
kubectl --context="$CONTEXT" apply --server-side \
  --field-manager=notebooks-gke-pilot -f gke/manifests/pilot/admission.yaml
kubectl --context="$CONTEXT" get validatingadmissionpolicy \
  notebooks-gke-pilot-workspaces -o json | jq '.status'
set -o pipefail
kubectl kustomize --load-restrictor=LoadRestrictionsNone gke/manifests/pilot | \
  kubectl --context="$CONTEXT" apply --server-side --dry-run=server \
    --field-manager=notebooks-gke-pilot -f -
kubectl kustomize --load-restrictor=LoadRestrictionsNone gke/manifests/pilot | \
  kubectl --context="$CONTEXT" apply --server-side \
    --field-manager=notebooks-gke-pilot -f -
```

Require the admission policy's observed generation to match and no type-checking
warnings. It permits only `gke-jupyterlab` in `team-a` and rejects Secret mounts
and user-supplied pod labels/annotations. The namespace enforces restricted Pod
Security. The notebook ServiceAccount has token automount disabled and no added
permissions. ResourceQuota bounds the pilot's compute and storage use.

The dedicated StorageClass `notebooks-gke-rwo` uses the GKE PD CSI driver with
`pd-balanced`, `WaitForFirstConsumer`, and `Retain`. Its
`notebooks.kubeflow.org/can-use=true` label is required by upstream: unlabeled
GKE default classes appear disabled in the notebook volume dialog. The overlay
does not modify those managed default classes. Retained disks still incur charges
after PVC deletion and require deliberate cleanup after data preservation.

Open `https://$HOSTNAME/workspaces/`, sign in, and select `team-a`. Create a workspace
with kind **JupyterLab Notebook**, image **JupyterLab SciPy**, and **Pilot CPU**.
Name it `gke-pilot` and attach a new home volume named `gke-pilot-home`, using
`notebooks-gke-rwo`, ReadWriteOnce, 10 GiB, and read-write mounting. Do not attach
Secrets or additional volumes. Submit the summary, wait for Running, and choose
**Connect > JupyterLab**. If the browser blocks the popup, open
`https://$HOSTNAME/workspace/connect/team-a/gke-pilot/jupyterlab/lab` directly.

The pilot settings request 0.5 CPU/1 GiB and limit the notebook to 1 CPU/2 GiB.
Stopping the workspace retains its PVC but terminates kernels and terminals;
starting it again restores files, not process memory. Keep the existing pilot
workspace when resuming this codelab rather than creating a duplicate.

Before admitting more users, record results for Google login, tenant discovery,
workspace creation, JupyterLab file operations, kernel and terminal WebSockets,
denied cross-tenant requests, direct backend/workspace access from an unrelated
pod, RBAC revocation, and workspace deletion/recreation. Preserve user PVCs.
Same-origin notebook content is not isolated from the visiting user's API session;
existing WebSockets are authorized only at connection time. Use a trusted single-user
pilot until these risks and a revocation policy are resolved.

## 9. Stop or remove the pilot

No automatic cleanup is run. Inventory resources and back up notebook data first.
Remove the specific IAP grant, then delete the dedicated HTTPRoute and Gateway;
wait for GKE to remove the managed load balancer before deleting its certificate
map entry, map, certificate, and global IP. Do not manually delete active
GKE-owned load-balancer internals. Confirm forwarding rules and billing resources
are gone. Retain shared cert-manager, APIs, cluster, registry, CRDs and all user
PVCs unless their deletion is separately approved. Deleting a tenant namespace
also deletes its PVCs and may delete underlying disks.

## Deployment record

2026-09-11, initial inspection:

- Target: `ipv6-project-379110`, `kubeflow-notebooks`, `us-central1-c`.
- Kubernetes: `1.35.7-gke.1222000`; VPC-native, Dataplane V2, Workload Identity.
- No application namespaces, GatewayClasses, or cert-manager installation.
- An Artifact Registry repository named `kubeflow-notebooks` exists.
- Existing certificate maps and DNS zones belong to other applications.
- The old `kubeflow-gateway` address is regional, not suitable for this global Gateway.
- No public endpoint or user access has been enabled by this codelab yet.

### 2026-09-12 recovery checkpoint

Recovered by comparing the saved plans and local configuration with read-only
GKE, Kubernetes, and IAP queries, plus an anonymous HTTPS request. The interruption
point is step 7, after saving the discovered audience locally but before rendering
and applying the ready plan. Do not repeat resource creation in steps 2 through 6.

Verified state:

- The target cluster is RUNNING with the standard GKE Gateway controller enabled.
- All four digest-pinned images in `deployment.local.json` are deployed. Controller,
  backend, and frontend each have one ready replica. The controller has
  `USE_ISTIO=false`; backend authentication is enabled.
- cert-manager `v1.21.2` is healthy. `workspaces-serving-cert` is Ready and both
  validating webhooks have a populated CA bundle. Admission requests still need
  an end-to-end test.
- Gateway `kubeflow-workspaces/notebooks` is Programmed at `8.233.28.206`.
  HTTPRoute `notebooks` is Accepted with ResolvedRefs; GCPBackendPolicy
  `notebooks-iap` and HealthCheckPolicy `notebooks` are Attached.
- Certificate Manager certificate `notebooks-gke` is ACTIVE for
  `notebooks.8.233.28.206.sslip.io`. An anonymous GET to
  `https://notebooks.8.233.28.206.sslip.io/workspaces/` returned HTTP 302 to Google
  login with successful TLS verification. This does not prove authenticated
  application access or proxy readiness.
- Backend service `gkegw1-jb5y-kubeflow-workspace-gke-access-pro-8080-rbnyra803ozt`
  has IAP enabled and references the proxy Service's NEG,
  `k8s1-2685048a-kubeflow-workspaces-gke-access-proxy-808-49e49cf2`, in `us-central1-c`.
- The verified audience is
  `/projects/628944397724/global/backendServices/5233981629647157169`.
  It is already saved in `deployment.local.json`, but the live `gke-access-proxy`
  ConfigMap and all four saved plans (`initial`, `bootstrap-v2`, `bootstrap-v3`,
  `bootstrap-v4`) still contain an empty audience. No `rendered/ready` plan exists.
- Both proxy replicas fail startup with `IAP audience must identify one global
  backend service by project number and service ID`. This is the intentional
  fail-closed bootstrap, not evidence of an invalid discovered audience.
- System and tenant ingress policies are installed, including webhook ingress
  from the configured `controlPlaneCIDR`, `10.128.0.120/32`. NetworkPolicy
  enforcement and webhook reachability have not been verified end to end.
- The backend-scoped IAP IAM policy has no bindings. Inherited IAM access was not
  audited. `team-a` has only the proxy's Role/RoleBinding, with no tenant user
  RoleBinding. No WorkspaceKinds, Workspaces, or tenant PVCs are present.

Resume with step 7's ready-plan commands using the existing local configuration:
render a fresh plan, review it, apply its application stage, restart the proxy,
and verify both replicas and the load-balancer backend become healthy. Recheck
the NEG/backend mapping if any edge resources have been recreated.

Then complete step 8: the single-backend pilot IAP grant, reviewed least-privilege
tenant and WorkspaceKind-discovery RBAC, and a reviewed WorkspaceKind before
creating a notebook. Google login, tenant discovery, notebook creation, JupyterLab
file operations, kernel/terminal WebSockets, isolation, revocation, and lifecycle
tests remain outstanding. Preserve PVCs once notebooks are created.

This recovery did not change cluster resources, IAM, or cloud resources. The local
worktree already had uncommitted changes outside `gke/`; these were left untouched.
Image source provenance is not established by matching deployed digests to the
local configuration. Never treat an unexecuted step as a successful validation.

### 2026-09-12 validation: proxy and webhook

- Applied the `rendered/ready` application stage and restarted the access proxy.
  Both replicas became ready and both NEG endpoints reported HEALTHY.
- Granted `aojea@google.com` `roles/iap.httpsResourceAccessor` on the verified
  backend only. The same binding is returned when querying by backend name or
  numeric ID. Browser login still returned IAP Access Denied; that issue remains
  separate from Kubernetes admission and must be resolved before acceptance.
- A server-side dry run of the pilot WorkspaceKind initially failed with a
  timeout connecting to the webhook endpoint at `10.20.0.29:9443`. The controller
  was ready, but `default-deny-ingress` selected it and `gke-webhook-ingress`
  allowed only the configured direct control-plane IP, `10.128.0.120/32`.
- This cluster's API-server connections use non-host-networked Konnectivity
  agents in `kube-system`. Updated the renderer and applied only
  `gke-webhook-ingress` from the new `rendered/ready-konnectivity` plan. The rule
  additionally permits TCP 9443 from pods matching BOTH namespace
  `kube-system` and label `k8s-app=konnectivity-agent`. It does not allow the whole
  namespace, node subnet, or pod CIDR. The direct control-plane rule is retained.
- The identical server-side dry run then succeeded, including the WorkspaceKind
  admission webhook. A regression test checks the complete policy, including the
  combined selectors and port restriction. `make -C gke test` passed, including
  race-enabled tests and `go vet`.
- The restricted manifests under `manifests/pilot/` pass server-side dry run but
  are NOT installed. They define the pilot user bindings, tokenless notebook
  ServiceAccount, resource quota, restricted Pod Security, admission restrictions,
  and a digest-pinned overlay of upstream's JupyterLab sample. The overlay fixes
  the sample's null probes by providing TCP startup/readiness probes.

To repeat the non-mutating pilot validation from the repository root:

```sh
set -o pipefail
kubectl kustomize --load-restrictor=LoadRestrictionsNone gke/manifests/pilot | \
  kubectl --context="$CONTEXT" apply --server-side --dry-run=server \
    --field-manager=notebooks-gke-pilot -f -
```

Do not reapply an older isolation snapshot: it lacks the Konnectivity allowance.
Do not disable the webhook or default-deny policy to work around this failure.
Pilot policy enforcement, authenticated notebook operation, isolation tests,
revocation, and lifecycle tests still need live validation.

### 2026-09-12 OAuth configuration checkpoint

The remaining login blocker is the organization boundary described in step 4,
not a missing IAP IAM grant or the webhook NetworkPolicy. The existing project
consent configuration is already External/Testing and includes `aojea@google.com`
as a test user. Its other OAuth clients and test users were left unchanged.

The console form for a new Web application named `Notebooks GKE pilot` is
prepared, but client creation and credential download are not yet confirmed.
The operator must retain the downloaded credential JSON privately outside this
repository and provide only its file path, never its contents, to continue.
Treat the whole downloaded JSON as a secret and restrict its file permissions
to `0600`. The local deployment configuration still has empty OAuth fields;
no custom OAuth Secret or policy update has been applied.

### 2026-09-12 working single-user pilot

The earlier OAuth and installation blockers are resolved:

- Verified the operator's private credential JSON, restricted its permissions to
  `0600`, and created `kubeflow-workspaces/iap-oauth` by piping the secret directly
  into `kubectl`, without displaying it or storing it in deployment plans.
- Added and verified the IAP callback on dedicated OAuth client
  `628944397724-a2uj76f0gd8g04asj0dtmq0qeod2kdn6.apps.googleusercontent.com`.
  Updated the non-secret local configuration, rendered `ready-oauth`, and applied
  only its GCPBackendPolicy. GKE reconciled the custom client onto the backend.
  The public login redirect lagged that API state by several minutes, then changed
  to the expected client. Existing project clients and audience were unchanged.
- Google login as `aojea@google.com` reached the frontend successfully. Tenant
  discovery returned HTTP 200 with only `team-a`. Backend workspace audit data
  records that verified email as the creator, despite the UI's placeholder user.
- Installed the pilot admission, RBAC, Pod Security, quota, ServiceAccount,
  WorkspaceKind, and dedicated StorageClass. Browser creation produced
  Workspace `team-a/gke-pilot` and Bound 10 GiB PVC `gke-pilot-home`.

Observed acceptance results:

| Check | Result |
| --- | --- |
| Public TLS and unauthenticated access | Valid TLS; anonymous HTTPS returns 302 to Google login |
| Notebook HTTP | Public Jupyter status and kernelspec APIs return 200; JupyterLab renders |
| Kernel WebSocket | Executed a notebook cell through JupyterLab; output was `GKE kernel result: 42` |
| File operations | Wrote and read `gke-pilot-validation.txt`; saved executed `gke-pilot-validation.ipynb` and read its output back |
| Terminal WebSocket | Received `GKE_TERMINAL_OK` and UID `1000` over the public terminal WebSocket |
| Pause/resume | UI Stop removed the pod while the PVC stayed Bound; UI Start created a replacement pod on the same PVC/disk; both saved files and notebook output survived |
| Cross-tenant authorization | Authenticated API and notebook requests to `default` returned 403 |
| Forged identity headers | Cross-tenant API request with forged Kubeflow user/group and impersonation headers still returned 403 |
| Pilot RBAC | Required workspace/PVC actions allowed; tested Secret, pod, Service, policy, role, WorkspaceKind creation and cross-tenant actions denied |
| Admission | Allowed notebook dry run passed; Secret mounts, pod metadata, and an unreviewed kind were each rejected by the pilot admission policy |
| Direct ingress bypass | Tokenless probe in `default` reached public HTTPS (302), while direct backend/frontend/proxy Services and notebook Service/pod connections all timed out; probe was deleted afterward |
| Pod hardening | Actual notebook runs as UID 1000, without privilege escalation, with all capabilities dropped and RuntimeDefault seccomp; no projected service-account token volume |

The notebook is left Running for the operator. Its saved validation files are on
the home PVC, not in this repository. No user PVCs or disks were deleted.

This is a working trusted single-user pilot, not a production or hostile
multi-tenant security certification. RBAC revocation, already-established
WebSocket revocation, workspace deletion/recreation races, broad filename/encoding
coverage, upgrades and uninstall remain untested live. The shared-origin notebook
content risk and image/dependency release review remain open.

Visible upstream compatibility issues remain: `/workspaces/api/v1/user` returns
404 and the header shows `kubeflow-user`; a frontend font fails to decode; the
Start dialog shows a redirect to `undefined` despite no configured redirect
(plain **Start** succeeded without changing options). Activity timestamps remained
zero during this session, so automatic culling has not been validated. These
issues were not worked around by disabling authentication or editing upstream.

After resume, reopening the saved notebook in the hidden integrated browser
showed a long-loading dialog even though the notebook DOM contained its saved
output and both file APIs returned 200. Foreground layout restoration after
resume was not confirmed. The earlier rendered JupyterLab, live kernel execution,
terminal WebSocket, and storage-persistence checks passed independently.

### 2026-09-12 Kubernetes tokens and VS Code

Implemented optional `desktopHostname` support entirely under `gke/`. The
IAP-protected page `/workspaces/connections` lists the signed-in user's grants,
issues a connection URL for an authorized running workspace, and revokes grants.
Each grant is a dedicated no-RBAC ServiceAccount in `notebooks-connections`, with
TokenRequest audience `https://connect.8.233.28.206.sslip.io/desktop` and a maximum
application lifetime of one hour. The token is never stored in the grant record.

The desktop listener on port 8081 verifies TokenReview, exact audience, an uncached
ServiceAccount lookup and matching UID/expiry, the original user's current RBAC,
and the workspace UID/port. It cannot route to frontend/backend management APIs.
Tokens, IAP credentials and token-bearing Referer headers are stripped before
forwarding. Verified desktop requests receive an internal matching XSRF
cookie/header pair because Jupyter rejected token-stripped POSTs without it.
Jupyter XSRF enforcement and the browser listener's IAP/Origin checks remain enabled.

Deployment:

- Desktop host: `connect.8.233.28.206.sslip.io`, same reserved global address.
- Certificate/map entry: `notebooks-desktop` in the existing `notebooks-gke` map;
  certificate ACTIVE and public TLS verification passed after edge propagation.
- Service: `gke-desktop-proxy`, port 8081, selecting the existing proxy pods.
- Backend: `gkegw1-jb5y-kubeflow-workspac-gke-desktop-pro-8081-pyjpz3pzixx5`,
  both endpoints HEALTHY. Its IAP and load-balancer logging are disabled; browser
  backend IAP remains enabled. No customer notebook data or PVC was changed.
- Current proxy image digest:
  `sha256:f77a092376620c30dbe9ff0887acef38d9aa28379d1436036a88c504cdaeb584`.
  Latest complete snapshot: `rendered/desktop-v4`. Earlier desktop snapshots are
  superseded and lack later request-throttling/redaction/XSRF fixes.

Verification:

- `scripts/test-connection-tokens.sh` confirmed dedicated audience acceptance,
  wrong-audience rejection, and rejection of ordinary Kubernetes API authentication.
  GKE's TokenReview briefly accepted a deleted token; an authoritative grant lookup
  rejected deletion and UID comparison rejected recreation. The probe cleaned up
  its temporary ServiceAccounts and private credential files.
- Unit tests cover grant issuance/ownership, real returned expiration, wrong or
  missing audience, deleted/recreated/expired grants, workspace UID changes, RBAC
  revocation, forbidden routes, header/query conflicts, credential stripping, and
  closing an active local WebSocket on revocation.
- Browser issuance returned 201 for `team-a/gke-pilot/jupyterlab` and 403 for
  another namespace or undeclared port. The human user cannot create
  `serviceaccounts/token` in the grant namespace.
- Public unauthenticated discovery returned 401 over verified TLS. An issued token
  discovered `python3`, created a kernel (201), and executed `DESKTOP_KERNEL_OK 42`
  over WebSocket. Revocation returned 204, the active socket closed, and later
  requests with that token returned 403. The test kernel and grant were removed.
- The operator generated a fresh URL through the page, selected its remote Python
  kernel in the standard Microsoft Jupyter extension 2025.9.1, and Cell 2 of
  `vscode_validation.ipynb` executed through VS Code. It reported hostname
  `ws-gke-pilot-2qdwp-0`, Linux, `/opt/conda/bin/python`, UID 1000 assertions, no
  projected ServiceAccount token file, and result 42. Repeated execution passed.

Active desktop WebSockets are revalidated every 15 seconds with a 10-second API
timeout; lookup failures, lost access, revocation, or expiry close the socket.
In-flight ordinary HTTP is bounded by expiry but not continuously reauthorized.
Removing IAP admission alone does not revoke desktop grants. Revoke explicitly or
remove workspace RBAC. Full one-hour expiry and VS Code interrupt/restart/reconnect
after expiry or revocation still require live acceptance, as do load testing and
broader multi-user security review. The remaining browser same-origin risks and
upstream UI/culling issues are unchanged.

### 2026-09-12 configurable token lifetimes

Replaced the initial fixed one-hour lifetime with administrator-configurable
`connectionTokenDefaultSeconds` (default 86400) and `connectionTokenMaxSeconds`
(default 604800). The common policy requires 600 <= default <= maximum <= 2592000
seconds. The management API accepts optional `durationSeconds`, exposes the policy,
and rejects out-of-range requests with HTTP 400 before creating a grant. The
connection page provides policy-bounded duration choices and displays actual expiry.
Existing grants retain their original expiry, even after the policy is changed.

Live GKE TokenRequest measurements:

| Requested | Granted |
| --- | --- |
| 86400 seconds (1 day) | 86400 seconds, within one-second measurement overhead |
| 604800 seconds (7 days) | 172800 seconds (2 days) |
| 2592000 seconds (30 days) | 172800 seconds (2 days) |

Kubernetes explicitly warned that the larger requests were shortened. The probe
printed only expiry metadata and removed its temporary ServiceAccount/files.
Audience isolation and authoritative deletion/UID checks still passed. A policy
maximum above two days does not overcome this cluster's token-issuer limit.

Deployed the new proxy and ConfigMap only; Gateway policy, Workspace, and PVC
configuration were unchanged. Pilot policy is 24-hour default, seven-day requested
maximum. Image digest:
`sha256:2470150446b3e87644d851fe1b2817db5db1f56a5d1738a13632ddbc8d0c5e32`.
Latest complete plan is `rendered/desktop-lifetimes-v1`.

Verification:

- Race-enabled Go tests and vet passed, including invalid administrator policies,
  minimum/maximum/default durations, seven-/30-day requests, issuer shortening and
  over-allocation, exact-expiry denial, and unchanged existing grants. Invalid
  HTTP durations cause no Kubernetes calls. Renderer tests verify environment values.
- Live management API advertised 86400/604800. A request for 604801 returned 400;
  604800 returned 201 with an effective two-day expiry. The UI selected one day by
  default; selecting seven days displayed the actual two-day expiration. Test
  grants were revoked. The duration control fit a 390-pixel mobile viewport.
- A public token-based kernel test assigned `lifetime_value = 42`, disconnected,
  and revoked the original grant. A replacement grant accessed the same kernel ID
  and executed `print(lifetime_value)`, yielding 42. The original token returned
  403. The test kernel and both temporary grants were removed; user grants remained.

This test confirms server-side kernel state can outlive a connection credential.
It does not establish automatic VS Code renewal or weeks-long uninterrupted
operation. Full-duration expiry, VS Code selection of an existing kernel after
credential replacement, load-balancer connection limits, and idle culling remain
separate acceptance gates. No expiry extension or refresh bypass was introduced.

### 2026-09-12 shutdown recovery snapshot

Work paused at the operator's request before shutting down the laptop/session.
The current task is debugging intermittent VS Code connection failure, not adding
another authentication mechanism. Earlier successful execution remains valid
historical evidence, but the latest reported VS Code failure is unresolved.

Local state:

- Repository branch `gke`, HEAD
  `24ce51e5b19adcef194aa989e9df6bbee3b3c1f9`.
- The entire `gke/` integration is untracked. Pre-existing tracked modifications
  under `developing/` and upstream backend/controller files remain untouched.
  Do not reset, stash, restore, or commit them implicitly during recovery.
- Latest deployed image and complete plan are the configurable-lifetime versions
  recorded above: `rendered/desktop-lifetimes-v1`, image digest
  `sha256:2470150446b3e87644d851fe1b2817db5db1f56a5d1738a13632ddbc8d0c5e32`.
- No implementation changes were made during the latest connection investigation.
  Do not overwrite the operator's current notebook edits.

Last observed live state:

- Both proxy replicas ready; `team-a/gke-pilot` Running and its pod
  `ws-gke-pilot-2qdwp-0` Ready.
- Browser management URL:
  `https://notebooks.8.233.28.206.sslip.io/workspaces/connections`.
- Token-authenticated server base:
  `https://connect.8.233.28.206.sslip.io/workspace/connect/team-a/gke-pilot/jupyterlab/`.
  Use a generated URL including its private token, not this bare base URL.
- Anonymous desktop discovery promptly returned 401 with normal TLS verification.
  A newly issued ten-minute diagnostic grant returned 200 and discovered `python3`;
  that test grant was revoked afterward.
- The saved notebook's latest cell output reports remote connection failure with
  `Invalid response: 403 Forbidden`, before Python execution. Notebook diagnostics
  showed no source errors. The root-only host shown in the error is not proof of
  a missing path: extension logs contain the full correct workspace path.
- Existing grants were not expired at the initial inspection (19:55 UTC), but that
  does not prove VS Code selected one of them. Do not reuse their old expiry state
  after resuming; list current grants again.

Credential-handling event: the browser tool unexpectedly included a token URL in
its automatic accessibility snapshot even though the explicit script returned
only non-secret metadata. Grant `connection-59762caa1f81cdc8cb264f89766553b7` was
immediately revoked through the IAP management API (204), and its URL field was
cleared. Never reuse that grant. No token value is recorded in this checkpoint.
Avoid browser inspection while a user-generated credential is displayed; masked
input appearance alone did not prevent the tool snapshot from including its value.

Latest extension evidence:

- Jupyter 2025.9.1, Python extension 2026.4.0, Pylance 2026.3.1.
- Remote extension log on the Linux host:
  `~/.vscode-server/data/logs/20260912T075621/exthost1/output_logging_20260912T075622/16-Jupyter.log`.
  A new session may use a different log directory. Redact JWTs, token query
  parameters, cookies, and authorization headers before sharing log output.
- At 19:30-19:31 UTC it reported 403 for the correct `/api/kernels` path,
  password fallback, and falling back to cached kernel discovery.
- At 19:46:20, 19:54:23, and 19:54:25 it reported
  `Interrupt requested & no kernel`. No newer connection result was found in that
  log at snapshot time. A notebook tool retry returned the same 403 error with no
  completed execution.
- After instructions to enter a fresh server URL rather than select an old saved
  entry, the operator reported that VS Code was "stuck waiting". Fresh kernel
  selection and successful execution have NOT been confirmed since that report.

Resume locally from that evidence:

1. Inspect current Git/worktree state and this checkpoint; do not repeat cluster,
   certificate, OAuth, or image creation. Verify live readiness and active grants.
2. Cancel the pending notebook/kernel prompt. If the picker is still stuck,
   reload the VS Code window with the operator's agreement, then reopen the
   existing notebook. Do not edit extension SecretStorage or delete unrelated
   saved server entries programmatically.
3. Sign in on the connection page and generate a fresh grant. In the notebook
   picker choose **Select Another Kernel > Existing Jupyter Server > Enter URL**,
   not the old cached server. Give it a distinct display name when prompted.
   The server URL must include the generated token and full workspace base path.
4. Run Cell 2 only after that remote kernel is selected. If it still fails, inspect
   the current extension log and the exact rejected endpoint/time. Stale saved
   server credentials or kernel-selection state are a hypothesis, not a confirmed
   root cause. Distinguish proxy rejection from Jupyter errors before changing code.
5. Preserve all user PVCs, existing notebook kernels, and unrelated grants. The
   underlying cloud deployment was not paused or deleted for this shutdown and
   continues to incur its normal charges. Laptop shutdown does not back up kernel
   memory or guarantee that cluster-side idle/culling policies leave it running.
