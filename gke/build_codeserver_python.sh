#!/usr/bin/env bash
# ==============================================================================
# Build and Push Custom Kubeflow Code-Server Python Image (with Gemini Code Assist)
# Target Registry: ${REGION}-docker.pkg.dev/${PROJECT_ID}/kubeflow-repo
#
# Reference:
#   https://github.com/kubeflow/notebooks/tree/notebooks-v1/components/example-notebook-servers/codeserver-python
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTEXT_DIR="${SCRIPT_DIR}/codeserver-python"
MANIFESTS_DIR="${SCRIPT_DIR}/manifests"

# ==============================================================================
# 1. Default Configuration & Environment Variables
# ==============================================================================
export PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
export REGION="${REGION:-us-west1}"
export REPO_NAME="${REPO_NAME:-kubeflow-repo}"
export IMAGE_NAME="${IMAGE_NAME:-codeserver-python}"
export IMAGE_TAG="${IMAGE_TAG:-gemini}"

# Default configuration
VARIANT="all"
BUILD_MODE="fast"
USE_CLOUD_BUILD=false
PUSH_IMAGE=true
REGISTER_WSK=false

show_usage() {
  cat <<EOF
Usage: $(basename "$0") [options]

Build and push custom Kubeflow VS Code (codeserver-python) images (CPU, GPU, and TPU)
with Gemini Code Assist, numpy, pandas, and accelerator libraries to Google Artifact Registry:
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/\${IMAGE_NAME}:\${IMAGE_TAG}-cpu
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/\${IMAGE_NAME}:\${IMAGE_TAG}-gpu
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/\${IMAGE_NAME}:\${IMAGE_TAG}-tpu

Environment Variables:
  PROJECT_ID   Google Cloud Project ID (default: current gcloud project)
  REGION       Google Cloud Region (default: us-west1)
  REPO_NAME    Artifact Registry repository name (default: kubeflow-repo)
  IMAGE_NAME   Docker image name (default: codeserver-python)
  IMAGE_TAG    Docker image tag prefix (default: gemini)

Options:
  --variant <cpu|gpu|tpu|all> Build specific accelerator variant or all 3 (default: all)
  --fast                     Fast layer build extending upstream codeserver-python:v1.11.0 (default)
  --full                     Full build from upstream codeserver:v1.11.0 (installs Conda/Python from scratch, CPU only)
  --cloud-build              Use Google Cloud Build (gcloud builds submit) instead of local Docker
  --no-push                  Build locally only; do not push to Artifact Registry
  --register-workspacekind   Apply/update the 'codeserver' WorkspaceKind and GKE ComputeClasses in the current Kubernetes cluster
  -h, --help                 Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --variant)
      VARIANT="$2"
      shift 2
      ;;
    --fast)
      BUILD_MODE="fast"
      shift
      ;;
    --full)
      BUILD_MODE="full"
      shift
      ;;
    --cloud-build)
      USE_CLOUD_BUILD=true
      shift
      ;;
    --no-push)
      PUSH_IMAGE=false
      shift
      ;;
    --register-workspacekind)
      REGISTER_WSK=true
      shift
      ;;
    -h|--help)
      show_usage
      exit 0
      ;;
    *)
      echo "ERROR: Unknown option: $1" >&2
      show_usage
      exit 1
      ;;
  esac
done

if [[ -z "${PROJECT_ID}" ]]; then
  echo "ERROR: PROJECT_ID is not set and could not be determined from 'gcloud config get-value project'." >&2
  echo "Please export PROJECT_ID=<your-gcp-project-id> and re-run." >&2
  exit 1
fi

case "${VARIANT}" in
  all)
    VARIANTS=("cpu" "gpu" "tpu")
    ;;
  cpu|gpu|tpu)
    VARIANTS=("${VARIANT}")
    ;;
  *)
    echo "ERROR: Invalid variant '${VARIANT}'. Allowed: cpu, gpu, tpu, all" >&2
    exit 1
    ;;
esac

echo "=================================================================="
echo "Custom Kubeflow Code-Server Python Image Builder"
echo "=================================================================="
echo "Project ID:       ${PROJECT_ID}"
echo "Region:           ${REGION}"
echo "Repository:       ${REPO_NAME}"
echo "Image Name:       ${IMAGE_NAME}"
echo "Image Tag Prefix: ${IMAGE_TAG}"
echo "Variants:         ${VARIANTS[*]}"
echo "Build Mode:       ${BUILD_MODE}"
echo "Build Backend:    $( [[ "${USE_CLOUD_BUILD}" == "true" ]] && echo "Google Cloud Build" || echo "Local Docker" )"
echo "=================================================================="

# ==============================================================================
# 2. Ensure Artifact Registry Repository Exists & GKE Nodes Have Pull Access
# ==============================================================================
if [[ "${PUSH_IMAGE}" == "true" ]]; then
  echo "Checking Artifact Registry repository '${REPO_NAME}' in ${REGION}..."
  if ! gcloud artifacts repositories describe "${REPO_NAME}" \
      --location="${REGION}" \
      --project="${PROJECT_ID}" >/dev/null 2>&1; then
    echo "Artifact Registry repository '${REPO_NAME}' not found. Creating it..."
    gcloud artifacts repositories create "${REPO_NAME}" \
      --repository-format=docker \
      --location="${REGION}" \
      --description="Kubeflow custom notebook server images" \
      --project="${PROJECT_ID}"
  else
    echo "Artifact Registry repository '${REPO_NAME}' already exists."
  fi

  # Grant GKE node ServiceAccount (Compute Engine default SA) Artifact Registry Reader role
  PROJECT_NUMBER="$(gcloud projects describe "${PROJECT_ID}" --format="value(projectNumber)" 2>/dev/null || true)"
  if [[ -n "${PROJECT_NUMBER}" ]]; then
    COMPUTE_SA="${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"
    echo "Ensuring GKE node service account (${COMPUTE_SA}) has roles/artifactregistry.reader on '${REPO_NAME}'..."
    gcloud artifacts repositories add-iam-policy-binding "${REPO_NAME}" \
      --location="${REGION}" \
      --project="${PROJECT_ID}" \
      --member="serviceAccount:${COMPUTE_SA}" \
      --role="roles/artifactregistry.reader" >/dev/null
  fi
fi

# ==============================================================================
# 3. Build & Push Images
# ==============================================================================
BUILT_IMAGES=()

if [[ "${USE_CLOUD_BUILD}" != "true" && "${PUSH_IMAGE}" == "true" ]]; then
  echo "Configuring Docker authentication for ${REGION}-docker.pkg.dev..."
  gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
fi

for v in "${VARIANTS[@]}"; do
  case "$v" in
    cpu)
      if [[ "${BUILD_MODE}" == "full" ]]; then
        V_DOCKERFILE="Dockerfile"
      else
        V_DOCKERFILE="Dockerfile.fast"
      fi
      V_TAG="${IMAGE_TAG}-cpu"
      V_DESC="CPU (Standard)"
      ;;
    gpu)
      V_DOCKERFILE="Dockerfile.fast.gpu"
      V_TAG="${IMAGE_TAG}-gpu"
      V_DESC="CUDA GPU"
      ;;
    tpu)
      V_DOCKERFILE="Dockerfile.fast.tpu"
      V_TAG="${IMAGE_TAG}-tpu"
      V_DESC="TPU"
      ;;
  esac

  V_IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${V_TAG}"

  echo "=================================================================="
  echo "Building [${V_DESC}] variant -> ${V_IMAGE_URI}"
  echo "Dockerfile: ${CONTEXT_DIR}/${V_DOCKERFILE}"
  echo "=================================================================="

  if [[ "${USE_CLOUD_BUILD}" == "true" ]]; then
    TMP_BUILD_DIR="$(mktemp -d)"
    cp -r "${CONTEXT_DIR}/." "${TMP_BUILD_DIR}/"
    cp "${CONTEXT_DIR}/${V_DOCKERFILE}" "${TMP_BUILD_DIR}/Dockerfile"

    gcloud builds submit "${TMP_BUILD_DIR}" \
      --tag="${V_IMAGE_URI}" \
      --project="${PROJECT_ID}"
    rm -rf "${TMP_BUILD_DIR}"
  else
    docker build \
      --platform linux/amd64 \
      -f "${CONTEXT_DIR}/${V_DOCKERFILE}" \
      -t "${V_IMAGE_URI}" \
      "${CONTEXT_DIR}"

    if [[ "${PUSH_IMAGE}" == "true" ]]; then
      echo "Pushing ${V_IMAGE_URI}..."
      docker push "${V_IMAGE_URI}"
    fi

    # Also tag the untagged base image for the CPU variant as a default fallback
    if [[ "$v" == "cpu" ]]; then
      DEFAULT_IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${IMAGE_TAG}"
      docker tag "${V_IMAGE_URI}" "${DEFAULT_IMAGE_URI}"
      if [[ "${PUSH_IMAGE}" == "true" ]]; then
        docker push "${DEFAULT_IMAGE_URI}"
      fi
    fi
  fi

  BUILT_IMAGES+=("${V_IMAGE_URI}")
done

echo "=================================================================="
echo "Successfully built $( [[ "${PUSH_IMAGE}" == "true" ]] && echo "and pushed " )images:"
for img in "${BUILT_IMAGES[@]}"; do
  echo "  - ${img}"
done
echo "=================================================================="

# ==============================================================================
# 4. Register / Update WorkspaceKind & ComputeClasses in Kubernetes Cluster (Optional)
# ==============================================================================
if [[ "${REGISTER_WSK}" == "true" ]]; then
  echo "=================================================================="
  echo "Registering WorkspaceKind 'codeserver' in Kubernetes cluster..."
  echo "=================================================================="
  envsubst < "${CONTEXT_DIR}/workspacekind.yaml" | kubectl apply -f -
  echo "WorkspaceKind 'codeserver' applied successfully."

  if [[ -d "${MANIFESTS_DIR}" ]]; then
    echo "=================================================================="
    echo "Applying GKE ComputeClass manifests (GPU and TPU)..."
    echo "=================================================================="
    kubectl apply -f "${MANIFESTS_DIR}/"
    echo "ComputeClass manifests applied successfully."
  fi
else
  echo ""
  echo "To register or update this image and ComputeClasses in Kubeflow Workspaces (Notebooks v2) on your GKE cluster, run:"
  echo "  PROJECT_ID=${PROJECT_ID} REGION=${REGION} REPO_NAME=${REPO_NAME} IMAGE_NAME=${IMAGE_NAME} IMAGE_TAG=${IMAGE_TAG} \\"
  echo "    envsubst < ${CONTEXT_DIR}/workspacekind.yaml | kubectl apply -f -"
  if [[ -d "${MANIFESTS_DIR}" ]]; then
    echo "  kubectl apply -f ${MANIFESTS_DIR}/"
  fi
  echo "Or re-run this script with --register-workspacekind"
fi
