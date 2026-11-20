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

### Deploy to GCP Cloud Run
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

## Key Engineering Highlights

- **Atomic Rate Limiting Without Race Conditions:** Uses a single Redis `EVAL` script to read, validate against TPM limits, increment, and set key expiry (`65s`) in one round-trip.
- **Thundering-Herd Protection:** `singleflight.Group` collapses duplicate concurrent requests on the same cache key into one upstream execution while tagging coalesced responses (`coalesced: true`).
- **Bounded Backpressure & Graceful Shutdown:** Non-blocking channel enqueueing with explicit drop logging prevents slow downstream storage from stalling HTTP handlers; `SIGTERM`/`SIGINT` drains the HTTP server and worker pool cleanly.
- **Data Lifecycle Hygiene:** Automatic TTL expiration on both Redis exact keys and MongoDB `semantic_cache` / `usage_telemetry` collections.