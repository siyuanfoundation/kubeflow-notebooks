#!/usr/bin/env bash
# ==============================================================================
# Build and Push Custom Kubeflow JupyterLab Image (TPU) & Spark Image to Google Artifact Registry
#
# Upstream CPU (jupyter-scipy:v1.10.0) and CUDA GPU (jupyter-pytorch-cuda-full:v1.10.0)
# images are used directly in WorkspaceKind/jupyterlab without custom rebuilds.
# This script builds the custom TPU variant (extending jupyter-scipy:v1.10.0 with
# jax[tpu], libtpu, kubeflow[spark], and google-cloud-storage) and the Spark 4.0.1
# image (Python 3.12) used by SparkConnect in distributed_tpu_example.ipynb.
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTEXT_DIR="${SCRIPT_DIR}/jupyterlab"
EXAMPLES_DIR="${SCRIPT_DIR}/examples"
MANIFESTS_DIR="${SCRIPT_DIR}/manifests"

# ==============================================================================
# 1. Default Configuration & Environment Variables
# ==============================================================================
export PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
export REGION="${REGION:-us-west1}"
export REPO_NAME="${REPO_NAME:-kubeflow-repo}"
export IMAGE_NAME="${IMAGE_NAME:-jupyterlab}"
export IMAGE_TAG="${IMAGE_TAG:-gemini}"
export USER_NAMESPACE="${USER_NAMESPACE:-kubeflow-user-example-com}"
export GCS_BUCKET="${GCS_BUCKET:-${USER_NAMESPACE}-bucket}"

# Default configuration
VARIANT="all"
USE_CLOUD_BUILD=false
PUSH_IMAGE=true
REGISTER_WSK=false

show_usage() {
  cat <<EOF
Usage: $(basename "$0") [options]

Build and push custom Kubeflow JupyterLab (CPU, GPU, TPU) and Spark images to Google Artifact Registry.
Each JupyterLab variant extends the upstream JupyterLab image with JAX, google-cloud-storage,
and kubeflow[spark] SDK dependencies.

All built images are tagged with both \${IMAGE_TAG} and 'latest':
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/\${IMAGE_NAME}:\${IMAGE_TAG}-cpu  (& :latest-cpu)
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/\${IMAGE_NAME}:\${IMAGE_TAG}-gpu  (& :latest-gpu)
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/\${IMAGE_NAME}:\${IMAGE_TAG}-tpu  (& :latest-tpu)
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/spark-py312:\${IMAGE_TAG}          (& :latest)

Environment Variables:
  PROJECT_ID     Google Cloud Project ID (default: current gcloud project)
  REGION         Google Cloud Region (default: us-west1)
  REPO_NAME      Artifact Registry repository name (default: kubeflow-repo)
  IMAGE_NAME     Docker image name (default: jupyterlab)
  IMAGE_TAG      Docker image tag prefix (default: gemini)
  USER_NAMESPACE Kubeflow user namespace (default: kubeflow-user-example-com)
  GCS_BUCKET     Shared GCS bucket for notebook pipeline (default: \${USER_NAMESPACE}-bucket)

Options:
  --variant <cpu|gpu|tpu|spark|all> Build specific variant or all 4 (default: all)
  --cloud-build                     Use Google Cloud Build (gcloud builds submit) instead of local Docker
  --no-push                         Build locally only; do not push to Artifact Registry
  --register-workspacekind          Apply/update the 'jupyterlab' WorkspaceKind and GKE ComputeClasses in the current Kubernetes cluster
  -h, --help                        Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --variant)
      VARIANT="$2"
      shift 2
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
  echo "ERROR: PROJECT_ID is not set and could not be determined from gcloud config." >&2
  echo "Please run: export PROJECT_ID=your-gcp-project-id" >&2
  exit 1
fi

case "${VARIANT}" in
  all)
    VARIANTS=("cpu" "gpu" "tpu" "spark")
    ;;
  cpu|gpu|tpu|spark)
    VARIANTS=("${VARIANT}")
    ;;
  *)
    echo "ERROR: Invalid variant '${VARIANT}'. Allowed: cpu, gpu, tpu, spark, all" >&2
    exit 1
    ;;
esac

echo "=================================================================="
echo "Build Configuration:"
echo "  PROJECT_ID:       ${PROJECT_ID}"
echo "  REGION:           ${REGION}"
echo "  REPO_NAME:        ${REPO_NAME}"
echo "  IMAGE_NAME:       ${IMAGE_NAME}"
echo "  IMAGE_TAG:        ${IMAGE_TAG}"
echo "  GCS_BUCKET:       ${GCS_BUCKET}"
echo "  Variants:         ${VARIANTS[*]}"
echo "  Use Cloud Build:  ${USE_CLOUD_BUILD}"
echo "  Push Image:       ${PUSH_IMAGE}"
echo "  Register WSK:     ${REGISTER_WSK}"
echo "=================================================================="

# ==============================================================================
# 2. Ensure Artifact Registry Repository Exists & Configure Docker Auth
# ==============================================================================
if [[ "${PUSH_IMAGE}" == "true" || "${USE_CLOUD_BUILD}" == "true" ]]; then
  echo "Checking Artifact Registry repository '${REPO_NAME}' in ${REGION}..."
  if ! gcloud artifacts repositories describe "${REPO_NAME}" \
      --location="${REGION}" \
      --project="${PROJECT_ID}" >/dev/null 2>&1; then
    echo "Creating Artifact Registry Docker repository '${REPO_NAME}' in ${REGION}..."
    gcloud artifacts repositories create "${REPO_NAME}" \
      --repository-format=docker \
      --location="${REGION}" \
      --description="Kubeflow Notebook custom images" \
      --project="${PROJECT_ID}"
  else
    echo "Artifact Registry repository '${REPO_NAME}' already exists."
  fi

  if [[ "${USE_CLOUD_BUILD}" == "false" ]]; then
    echo "Configuring Docker authentication for ${REGION}-docker.pkg.dev..."
    gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
  fi
fi

# ==============================================================================
# 3. Build & Push Images
# ==============================================================================
BUILT_IMAGES=()

for v in "${VARIANTS[@]}"; do
  case "$v" in
    cpu)
      V_DOCKERFILE="Dockerfile.fast"
      V_TAG="${IMAGE_TAG}-cpu"
      V_LATEST_TAG="latest-cpu"
      V_DESC="JupyterLab CPU"
      ;;
    gpu)
      V_DOCKERFILE="Dockerfile.fast.gpu"
      V_TAG="${IMAGE_TAG}-gpu"
      V_LATEST_TAG="latest-gpu"
      V_DESC="JupyterLab CUDA GPU"
      ;;
    tpu)
      V_DOCKERFILE="Dockerfile.fast.tpu"
      V_TAG="${IMAGE_TAG}-tpu"
      V_LATEST_TAG="latest-tpu"
      V_DESC="JupyterLab TPU"
      ;;
    spark)
      V_DOCKERFILE="Dockerfile.spark"
      V_TAG="${IMAGE_TAG}"
      V_LATEST_TAG="latest"
      V_DESC="Spark 4.0.1 (Python 3.12)"
      ;;
  esac

  if [[ "$v" == "spark" ]]; then
    V_IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/spark-py312:${V_TAG}"
    V_LATEST_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/spark-py312:${V_LATEST_TAG}"
  else
    V_IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${V_TAG}"
    V_LATEST_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${V_LATEST_TAG}"
  fi

  echo "=================================================================="
  echo "Building [${V_DESC}] variant -> ${V_IMAGE_URI} (and ${V_LATEST_URI})"
  echo "Dockerfile: ${CONTEXT_DIR}/${V_DOCKERFILE}"
  echo "=================================================================="

  TMP_BUILD_DIR="$(mktemp -d)"
  cp -r "${CONTEXT_DIR}/." "${TMP_BUILD_DIR}/"
  cp -r "${EXAMPLES_DIR}" "${TMP_BUILD_DIR}/examples"
  cp "${CONTEXT_DIR}/${V_DOCKERFILE}" "${TMP_BUILD_DIR}/Dockerfile"

  if [[ "${USE_CLOUD_BUILD}" == "true" ]]; then
    gcloud builds submit "${TMP_BUILD_DIR}" \
      --tag="${V_IMAGE_URI}" \
      --project="${PROJECT_ID}"
    rm -rf "${TMP_BUILD_DIR}"
    if [[ "${V_IMAGE_URI}" != "${V_LATEST_URI}" ]]; then
      gcloud container images add-tag "${V_IMAGE_URI}" "${V_LATEST_URI}" --quiet
    fi
    if [[ "$v" == "cpu" ]]; then
      DEFAULT_IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${IMAGE_TAG}"
      DEFAULT_LATEST_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:latest"
      gcloud container images add-tag "${V_IMAGE_URI}" "${DEFAULT_IMAGE_URI}" --quiet
      gcloud container images add-tag "${V_IMAGE_URI}" "${DEFAULT_LATEST_URI}" --quiet
    fi
  else
    docker build \
      --platform linux/amd64 \
      -f "${TMP_BUILD_DIR}/Dockerfile" \
      -t "${V_IMAGE_URI}" \
      -t "${V_LATEST_URI}" \
      "${TMP_BUILD_DIR}"
    rm -rf "${TMP_BUILD_DIR}"

    if [[ "${PUSH_IMAGE}" == "true" ]]; then
      echo "Pushing ${V_IMAGE_URI}..."
      docker push "${V_IMAGE_URI}"
      if [[ "${V_IMAGE_URI}" != "${V_LATEST_URI}" ]]; then
        echo "Pushing ${V_LATEST_URI}..."
        docker push "${V_LATEST_URI}"
      fi
    fi

    # Also tag the untagged base image for the CPU variant as a default fallback
    if [[ "$v" == "cpu" ]]; then
      DEFAULT_IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${IMAGE_TAG}"
      DEFAULT_LATEST_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:latest"
      docker tag "${V_IMAGE_URI}" "${DEFAULT_IMAGE_URI}"
      docker tag "${V_IMAGE_URI}" "${DEFAULT_LATEST_URI}"
      if [[ "${PUSH_IMAGE}" == "true" ]]; then
        docker push "${DEFAULT_IMAGE_URI}"
        docker push "${DEFAULT_LATEST_URI}"
      fi
    fi
  fi

  BUILT_IMAGES+=("${V_IMAGE_URI}" "${V_LATEST_URI}")
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
  echo "Registering WorkspaceKind 'jupyterlab' in Kubernetes cluster..."
  echo "=================================================================="
  envsubst < "${CONTEXT_DIR}/workspacekind.yaml" | kubectl apply -f -
  echo "WorkspaceKind 'jupyterlab' applied successfully."

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
  echo "  PROJECT_ID=${PROJECT_ID} REGION=${REGION} REPO_NAME=${REPO_NAME} IMAGE_NAME=${IMAGE_NAME} GCS_BUCKET=${GCS_BUCKET} \\"
  echo "    envsubst < ${CONTEXT_DIR}/workspacekind.yaml | kubectl apply -f -"
  if [[ -d "${MANIFESTS_DIR}" ]]; then
    echo "  kubectl apply -f ${MANIFESTS_DIR}/"
  fi
  echo "Or re-run this script with --register-workspacekind"
fi
