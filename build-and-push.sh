#!/usr/bin/env bash
# build-and-push.sh
# Build locally, push the image to Artifact Registry, and update the Cloud Run Job.
# Usage:  ./build-and-push.sh [--tag v1.2.3]   (defaults to :latest)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REGION="us-central1"
ACCOUNT="acastellanos@recurly.com"
PROJECT="it-tools-4fc1b6dd"
REPO="slack-holiday-status"
REGISTRY_HOST="${REGION}-docker.pkg.dev"
IMAGE_REGISTRY="${REGISTRY_HOST}/${PROJECT}/${REPO}"
JOB_NAME="slack-holiday-status"
TAG="latest"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)  TAG="$2"; shift 2 ;;
    *)      echo "Unknown arg: $1"; exit 1 ;;
  esac
done

IMAGE="${IMAGE_REGISTRY}/app:${TAG}"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo " Account  : $ACCOUNT"
echo " Project  : $PROJECT"
echo " Image    : $IMAGE"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# ---- pre-flight checks ----
for cmd in docker gcloud; do
  command -v "$cmd" > /dev/null 2>&1 || { echo "❌ '$cmd' is not installed or not in PATH"; exit 1; }
done
echo "✓ Pre-flight checks passed"

# ---- ensure Docker → Artifact Registry credential helper ----
if ! grep -q "${REGISTRY_HOST}" ~/.docker/config.json 2>/dev/null; then
  echo "→ One-time: running gcloud auth configure-docker …"
  gcloud auth configure-docker "${REGISTRY_HOST}" --quiet
fi
echo "✓ Docker → Artifact Registry auth configured"

# ---- set gcloud project ----
gcloud config set project "$PROJECT" > /dev/null
echo "✓ Project set to $PROJECT"

# ---- build multi-stage image for linux/amd64 & push ----
docker buildx build \
  --platform linux/amd64 \
  -f "$SCRIPT_DIR/Dockerfile" \
  -t "$IMAGE" \
  --push \
  "$SCRIPT_DIR"

echo "✓ Image built and pushed: $IMAGE"

# ---- update Cloud Run Job if it already exists ----
if gcloud run jobs describe "$JOB_NAME" --region="$REGION" > /dev/null 2>&1; then
  gcloud run jobs update "$JOB_NAME" \
    --image="$IMAGE" \
    --region="$REGION"
  echo "✓ Cloud Run Job updated: $JOB_NAME → $IMAGE"
else
  echo "ℹ Cloud Run Job '$JOB_NAME' does not exist yet — create it with:"
  echo "  gcloud run jobs create $JOB_NAME --image=$IMAGE --region=$REGION --max-retries=3 --task-timeout=60s --set-env-vars=SLACK_USER_TOKEN=xoxp-..."
fi

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅  $IMAGE"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
