#!/usr/bin/env bash
set -euo pipefail

# --- Required / Configurable Environment Variables ---
: "${PROJECT_ID:?Set PROJECT_ID (e.g., export PROJECT_ID=my-gcp-project)}"
REGION="${REGION:-asia-south1}"
SERVICE_NAME="${SERVICE_NAME:-semantic-ai-gateway}"
REPO_NAME="${REPO_NAME:-ai-gateway-repo}"
IMAGE_TAG="${IMAGE_TAG:-$(git rev-parse --short HEAD 2>/dev/null || echo 'latest')}"

# Secret Manager secret names (create these once before running)
GEMINI_SECRET_NAME="${GEMINI_SECRET_NAME:-gemini-api-key}"
MONGO_SECRET_NAME="${MONGO_SECRET_NAME:-mongo-atlas-uri}"

# Redis endpoint (e.g., Memorystore IP:6379 or Upstash / Redis Cloud host:port)
: "${REDIS_ADDR:?Set REDIS_ADDR (e.g., export REDIS_ADDR=10.x.x.x:6379)}"

IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${SERVICE_NAME}:${IMAGE_TAG}"

echo "==> Enabling required GCP APIs..."
gcloud services enable \
  run.googleapis.com \
  artifactregistry.googleapis.com \
  secretmanager.googleapis.com \
  --project="${PROJECT_ID}"

echo "==> Ensuring Artifact Registry repository '${REPO_NAME}' exists..."
if ! gcloud artifacts repositories describe "${REPO_NAME}" \
  --location="${REGION}" \
  --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud artifacts repositories create "${REPO_NAME}" \
    --repository-format=docker \
    --location="${REGION}" \
    --description="Container images for Semantic AI Gateway" \
    --project="${PROJECT_ID}"
fi

echo "==> Building and pushing container image: ${IMAGE_URI}"
gcloud builds submit . \
  --tag="${IMAGE_URI}" \
  --project="${PROJECT_ID}"

echo "==> Deploying '${SERVICE_NAME}' to Cloud Run (${REGION})..."
gcloud run deploy "${SERVICE_NAME}" \
  --image="${IMAGE_URI}" \
  --region="${REGION}" \
  --platform=managed \
  --allow-unauthenticated \
  --port=8080 \
  --cpu=1 \
  --memory=512Mi \
  --concurrency=80 \
  --min-instances=0 \
  --max-instances=10 \
  --timeout=60s \
  --set-env-vars="REDIS_ADDR=${REDIS_ADDR}" \
  --set-secrets="GEMINI_API_KEY=${GEMINI_SECRET_NAME}:latest,MONGO_URI=${MONGO_SECRET_NAME}:latest" \
  --project="${PROJECT_ID}"

SERVICE_URL="$(gcloud run services describe "${SERVICE_NAME}" \
  --region="${REGION}" \
  --project="${PROJECT_ID}" \
  --format='value(status.url)')"

echo "==> Deployment complete!"
echo "Service URL: ${SERVICE_URL}"
echo "Health check: curl -sf ${SERVICE_URL}/healthz"
