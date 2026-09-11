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

# ==============================================================================
# 1. Default Configuration & Environment Variables
# ==============================================================================
export PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
export REGION="${REGION:-us-west1}"
export REPO_NAME="${REPO_NAME:-kubeflow-repo}"
export IMAGE_NAME="${IMAGE_NAME:-codeserver-python}"
export IMAGE_TAG="${IMAGE_TAG:-gemini}"

# Default build mode: fast (extends upstream codeserver-python:v1.11.0)
# Use --full to build from upstream codeserver:v1.11.0 (full Miniforge/Conda install)
DOCKERFILE="Dockerfile.fast"
USE_CLOUD_BUILD=false
PUSH_IMAGE=true
REGISTER_WSK=false

show_usage() {
  cat <<EOF
Usage: $(basename "$0") [options]

Build and push a custom Kubeflow VS Code (codeserver-python) image with Gemini
Code Assist, numpy, and pandas to Google Artifact Registry:
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/\${IMAGE_NAME}:\${IMAGE_TAG}

Environment Variables:
  PROJECT_ID   Google Cloud Project ID (default: current gcloud project)
  REGION       Google Cloud Region (default: us-west1)
  REPO_NAME    Artifact Registry repository name (default: kubeflow-repo)
  IMAGE_NAME   Docker image name (default: codeserver-python)
  IMAGE_TAG    Docker image tag (default: gemini)

Options:
  --fast                     Fast layer build extending upstream codeserver-python:v1.11.0 (default)
  --full                     Full build from upstream codeserver:v1.11.0 (installs Conda/Python from scratch)
  --cloud-build              Use Google Cloud Build (gcloud builds submit) instead of local Docker
  --no-push                  Build locally only; do not push to Artifact Registry
  --register-workspacekind   Apply/update the 'codeserver' WorkspaceKind in the current Kubernetes cluster
  -h, --help                 Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --fast)
      DOCKERFILE="Dockerfile.fast"
      shift
      ;;
    --full)
      DOCKERFILE="Dockerfile"
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

IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${IMAGE_TAG}"

echo "=================================================================="
echo "Custom Kubeflow Code-Server Python Image Builder (Gemini Code Assist)"
echo "=================================================================="
echo "Project ID:       ${PROJECT_ID}"
echo "Region:           ${REGION}"
echo "Repository:       ${REPO_NAME}"
echo "Target Image URI: ${IMAGE_URI}"
echo "Dockerfile:       ${CONTEXT_DIR}/${DOCKERFILE}"
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
# 3. Build & Push Image
# ==============================================================================
if [[ "${USE_CLOUD_BUILD}" == "true" ]]; then
  echo "=================================================================="
  echo "Submitting build to Google Cloud Build..."
  echo "=================================================================="
  TMP_BUILD_DIR="$(mktemp -d)"
  trap 'rm -rf "${TMP_BUILD_DIR}"' EXIT
  cp -r "${CONTEXT_DIR}/." "${TMP_BUILD_DIR}/"
  cp "${CONTEXT_DIR}/${DOCKERFILE}" "${TMP_BUILD_DIR}/Dockerfile"

  gcloud builds submit "${TMP_BUILD_DIR}" \
    --tag="${IMAGE_URI}" \
    --project="${PROJECT_ID}"
else
  echo "=================================================================="
  echo "Building image locally with Docker..."
  echo "=================================================================="
  if [[ "${PUSH_IMAGE}" == "true" ]]; then
    echo "Configuring Docker authentication for ${REGION}-docker.pkg.dev..."
    gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
  fi

  docker build \
    --platform linux/amd64 \
    -f "${CONTEXT_DIR}/${DOCKERFILE}" \
    -t "${IMAGE_URI}" \
    "${CONTEXT_DIR}"

  if [[ "${PUSH_IMAGE}" == "true" ]]; then
    echo "=================================================================="
    echo "Pushing image to ${IMAGE_URI}..."
    echo "=================================================================="
    docker push "${IMAGE_URI}"
  fi
fi

echo "=================================================================="
echo "Successfully built $( [[ "${PUSH_IMAGE}" == "true" ]] && echo "and pushed " )image:"
echo "  ${IMAGE_URI}"
echo "=================================================================="

# ==============================================================================
# 4. Register / Update WorkspaceKind in Kubernetes Cluster (Optional)
# ==============================================================================
if [[ "${REGISTER_WSK}" == "true" ]]; then
  echo "=================================================================="
  echo "Registering WorkspaceKind 'codeserver' in Kubernetes cluster..."
  echo "=================================================================="
  envsubst < "${CONTEXT_DIR}/workspacekind.yaml" | kubectl apply -f -
  echo "WorkspaceKind 'codeserver' applied with image: ${IMAGE_URI}"
else
  echo ""
  echo "To register or update this image in Kubeflow Workspaces (Notebooks v2) on your GKE cluster, run:"
  echo "  PROJECT_ID=${PROJECT_ID} REGION=${REGION} REPO_NAME=${REPO_NAME} IMAGE_NAME=${IMAGE_NAME} IMAGE_TAG=${IMAGE_TAG} \\"
  echo "    envsubst < ${CONTEXT_DIR}/workspacekind.yaml | kubectl apply -f -"
  echo "Or re-run this script with --register-workspacekind"
fi
