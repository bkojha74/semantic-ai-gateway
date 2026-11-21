# Semantic AI Gateway (`semantic-ai-gateway`)

A high-concurrency **Semantic API Gateway & Token-Aware AI Cache** built in **Go** for Gemini (`google.golang.org/genai`), combining **Redis** atomic token rate limiting and exact caching, **MongoDB Atlas Vector Search** (`go.mongodb.org/mongo-driver/v2`) semantic similarity caching, **`singleflight` request coalescing**, and **GCP Cloud Run** deployment.

---

## Why This Project Exists

LLM inference introduces two production bottlenecks: **tail latency** (often 600ms–2,000ms+ per request) and **unpredictable token spend** when users or services ask exact or semantically equivalent questions repeatedly.

`semantic-ai-gateway` acts as a drop-in gateway in front of Gemini models that:
1. **Enforces per-tenant Tokens-Per-Minute (TPM) budgets** atomically in Redis using a Lua script before invoking embeddings or generation.
2. **Coalesces thundering-herd concurrent requests** via `golang.org/x/sync/singleflight`.
3. **Serves repeated queries fast** through a two-tier cache:
   - **Tier 1 (Exact Match — Redis):** SHA-256 hash lookup (`< 2ms`).
   - **Tier 2 (Semantic Match — MongoDB Atlas `$vectorSearch`):** Cosine similarity search on `text-embedding-004` embeddings (`~15–25ms` when similarity meets the threshold, e.g. `>= 0.92`), with automatic Redis backfill.
4. **Keeps the hot path non-blocking** by offloading cache persistence and time-series telemetry writes to a bounded Go worker pool.

---

## Architecture Overview

```text
Client ──► POST /v1/generate ──► Go Gateway (Cloud Run)
                                     │
                 ┌───────────────────┴───────────────────┐
                 ▼                                       ▼
     Redis Lua TPM Limiter                 singleflight Coalescer
       (Tokens / Minute)                   (Deduplicates In-Flight)
                                                 │
                         ┌───────────────────────┴───────────────────────┐
                         ▼ (Hit)                                         ▼ (Miss)
                  Tier-1 Redis Exact                             Embed Prompt (text-embedding-004)
                   (< 2ms JSON)                                          │
                                                                         ▼
                                                              Tier-2 MongoDB $vectorSearch
                                                                (score >= threshold?)
                                                                 │                 │
                                                           Yes (Hit)          No (Miss)
                                                                 │                 │
                                                                 ▼                 ▼
                                                           Backfill L1      Gemini GenerateContent
                                                                 │                 │
                                                                 └────────┬────────┘
                                                                          ▼
                                                            Async Worker Pool (Channels)
                                                            ├─► Redis Set (TTL)
                                                            ├─► MongoDB semantic_cache (TTL)
                                                            └─► MongoDB usage_telemetry (Time-Series)
```

---

## Project Layout

```text
semantic-ai-gateway/
├── cmd/
│   └── gateway/
│       └── main.go                  # Gemini adapter, HTTP server, graceful shutdown
├── internal/
│   ├── cache/
│   │   ├── redis_exact.go           # Redis client, Lua TPM limiter, Tier-1 exact cache
│   │   └── mongo_semantic.go        # MongoDB v2 $vectorSearch, TTL upsert, telemetry
│   ├── domain/
│   │   └── models.go                # Request/response models, cache key hashing, interfaces
│   └── gateway/
│       ├── service.go               # Orchestration pipeline & bounded async worker pool
│       └── service_test.go          # Table-driven unit tests + singleflight concurrency test
├── deployments/
│   ├── Dockerfile                   # Multi-stage build -> distroless nonroot runtime
│   └── docker-compose.yml           # Local stack: Redis 7.4 + mongodb-atlas-local + Go gateway
├── scripts/
│   ├── init_mongo.js                # Collections, TTL indexes, Atlas Vector Search & time-series setup
│   ├── loadtest.js                  # k6 benchmark script tracking hit/miss latency & tiers
│   └── deploy_cloudrun.sh           # Artifact Registry + Secret Manager + Cloud Run deployment
├── go.mod
└── go.sum
```

---

## Local Quick Start

### 1. Prerequisites
- Go 1.24+
- Docker & Docker Compose
- A `GEMINI_API_KEY`

### 2. Start Redis & MongoDB Atlas Local
```bash
export GEMINI_API_KEY="your-gemini-api-key"
docker compose -f deployments/docker-compose.yml up -d --build
```

### 3. Run Unit Tests (with Race Detector)
```bash
go test -v -race ./...
```

### 4. Test Exact & Semantic Cache Tiers
```bash
# Cold Miss (calls Gemini API)
curl -s -X POST http://localhost:8080/v1/generate \
  -H "Content-Type: application/json" \
  -d '{
    "tenant_id": "demo_tenant",
    "model": "gemini-2.5-flash",
    "prompt": "How do idempotency keys prevent duplicate charges in payment APIs?",
    "threshold": 0.92
  }' | jq .

# Semantic Hit (rephrased prompt matched via MongoDB $vectorSearch)
curl -s -X POST http://localhost:8080/v1/generate \
  -H "Content-Type: application/json" \
  -d '{
    "tenant_id": "demo_tenant",
    "model": "gemini-2.5-flash",
    "prompt": "Explain how an idempotency key stops duplicate payments in a payment service.",
    "threshold": 0.92
  }' | jq .
```

---

## Benchmarking & GCP Cloud Run Deployment

### Load Testing with `k6`
```bash
k6 run -e GATEWAY_URL=http://localhost:8080 scripts/loadtest.js
```

### Deploy to GCP Cloud Run (Manual Script)
```bash
export PROJECT_ID="your-gcp-project-id"
export REDIS_ADDR="your-redis-host:6379"

printf "%s" "$GEMINI_API_KEY" | gcloud secrets create gemini-api-key --data-file=- --project="$PROJECT_ID"
printf "%s" "mongodb+srv://<user>:<pass>@<cluster>.mongodb.net/?retryWrites=true&w=majority" | \
  gcloud secrets create mongo-atlas-uri --data-file=- --project="$PROJECT_ID"

chmod +x scripts/deploy_cloudrun.sh
./scripts/deploy_cloudrun.sh
```

---

## Automated CI/CD Pipeline (GitHub Actions)

This repository includes a production-grade CI/CD pipeline using **GitHub Actions**:

### 1. Continuous Integration (`.github/workflows/ci.yml`)
Triggers on pull requests and pushes to `main`:
- **Code Quality & Formatting:** Validates `gofmt` compliance, runs `go vet ./...`, and checks linting with `golangci-lint`.
- **Unit & Concurrency Tests:** Runs the test suite with Go's race detector (`-race`) and generates atomic code coverage reports.
- **Vulnerability Scanning:** Runs official Go `govulncheck` to catch known CVEs across all direct and transitive dependencies.
- **Container Build Validation:** Tests multi-stage Docker build against [`deployments/Dockerfile`](deployments/Dockerfile) using Buildx cache.

### 2. Continuous Delivery / Deployment (`.github/workflows/cd.yml`)
Triggers automatically on merge to `main` (or via manual `workflow_dispatch` trigger):
1. Runs the full CI validation job.
2. Authenticates to Google Cloud via Workload Identity Federation (WIF) or Service Account key.
3. Automatically ensures the Google Artifact Registry repository exists.
4. Builds, tags (Git commit SHA + `latest`), and pushes the production container image.
5. Deploys the service to **GCP Cloud Run** with automatic secret injection (`GEMINI_API_KEY`, `MONGO_URI`) and environment variable configuration (`REDIS_ADDR`).
6. Verifies the deployment with an automated health check against `/healthz`.

### Required GitHub Secrets & Variables

Configure these in **GitHub Settings -> Secrets and variables -> Actions**:

| Secret / Variable | Type | Description |
|---|---|---|
| `GCP_PROJECT_ID` | Secret | Google Cloud Project ID (e.g., `my-cloud-project`) |
| `REDIS_ADDR` | Secret | Redis endpoint (e.g., Memorystore IP:6379 or Upstash host:port) |
| `GCP_WIF_PROVIDER` | Secret | Workload Identity Provider resource name *(recommended)* |
| `GCP_WIF_SERVICE_ACCOUNT` | Secret | Workload Identity Service Account email *(recommended)* |
| `GCP_SA_KEY` | Secret | Service Account JSON key *(alternative fallback if not using WIF)* |
| `GCP_REGION` | Variable | Cloud Run deployment region (defaults to `asia-south1`) |
| `GCP_SERVICE_NAME` | Variable | Cloud Run service name (defaults to `semantic-ai-gateway`) |
| `GCP_REPO_NAME` | Variable | Artifact Registry repo name (defaults to `ai-gateway-repo`) |

---

## Key Engineering Highlights

- **Atomic Rate Limiting Without Race Conditions:** Uses a single Redis `EVAL` script to read, validate against TPM limits, increment, and set key expiry (`65s`) in one round-trip.
- **Thundering-Herd Protection:** `singleflight.Group` collapses duplicate concurrent requests on the same cache key into one upstream execution while tagging coalesced responses (`coalesced: true`).
- **Bounded Backpressure & Graceful Shutdown:** Non-blocking channel enqueueing with explicit drop logging prevents slow downstream storage from stalling HTTP handlers; `SIGTERM`/`SIGINT` drains the HTTP server and worker pool cleanly.
- **Data Lifecycle Hygiene:** Automatic TTL expiration on both Redis exact keys and MongoDB `semantic_cache` / `usage_telemetry` collections.