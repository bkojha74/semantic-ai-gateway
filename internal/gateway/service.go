package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"semantic-ai-gateway/internal/domain"

	"golang.org/x/sync/singleflight"
)

type writeBackJob struct {
	req          domain.PromptRequest
	hashKey      string
	vector       []float32
	resp         *domain.CompletionResponse
	persistMongo bool
}

type telemetryJob struct {
	req       domain.PromptRequest
	tier      string
	score     float64
	latencyMs int64
	resp      *domain.CompletionResponse
}

type Service struct {
	redisCache    domain.RedisStore
	semanticCache domain.SemanticCache
	llm           domain.LLMClient
	sfGroup       singleflight.Group

	writeCh   chan writeBackJob
	metricsCh chan telemetryJob
	wg        sync.WaitGroup
}

func NewService(rc domain.RedisStore, sc domain.SemanticCache, llm domain.LLMClient, workers int) *Service {
	s := &Service{
		redisCache:    rc,
		semanticCache: sc,
		llm:           llm,
		writeCh:       make(chan writeBackJob, 1024),
		metricsCh:     make(chan telemetryJob, 2048),
	}

	// Dedicated write-back workers drain writeCh.
	for i := 0; i < workers; i++ {
		s.wg.Add(1)
		go s.writeWorker()
	}
	// Dedicated telemetry workers drain metricsCh.
	for i := 0; i < workers; i++ {
		s.wg.Add(1)
		go s.metricsWorker()
	}
	return s
}

func (s *Service) Generate(ctx context.Context, req domain.PromptRequest) (*domain.CompletionResponse, error) {
	start := time.Now()
	if req.Model == "" {
		req.Model = "gemini-3.1-flash-lite"
	}
	if req.Threshold <= 0 {
		req.Threshold = 0.92
	}

	// 1. Atomic Redis Lua TPM Check
	allowed, err := s.redisCache.AllowTPM(ctx, req.TenantID, req.EstimatedTokens())
	if err != nil {
		slog.Warn("rate limiter redis error, failing open", "err", err)
	} else if !allowed {
		return nil, domain.ErrRateLimitExceeded
	}

	// 2. Singleflight Request Coalescing
	hashKey := req.CacheKey()
	val, err, shared := s.sfGroup.Do(hashKey, func() (any, error) {
		// Tier 1: Exact Match in Redis
		if cached, ok := s.redisCache.Get(ctx, req.TenantID, hashKey); ok {
			out := *cached
			out.CacheTier = "EXACT_HIT"
			out.SimilarityScore = 1.0
			s.enqueueTelemetry(req, "EXACT_HIT", 1.0, time.Since(start).Milliseconds(), &out)
			return &out, nil
		}

		// Tier 2: Semantic Vector Lookup in MongoDB Atlas
		vec, embedErr := s.llm.Embed(ctx, req.Prompt)
		if embedErr == nil {
			if match, score, ok := s.semanticCache.Search(ctx, req.TenantID, req.Model, vec, req.Threshold); ok {
				out := *match
				out.CacheTier = "SEMANTIC_HIT"
				out.SimilarityScore = score
				// Backfill Tier-1 Redis so identical prompts hit L1 next time
				s.enqueueWriteBack(req, hashKey, vec, &out, false)
				s.enqueueTelemetry(req, "SEMANTIC_HIT", score, time.Since(start).Milliseconds(), &out)
				return &out, nil
			}
		} else {
			slog.Warn("embedding generation failed, falling back to upstream LLM", "err", embedErr)
		}

		// Cache Miss: Call Gemini Upstream
		resp, err := s.llm.Generate(ctx, req)
		if err != nil {
			return nil, err
		}
		resp.CacheTier = "MISS"
		resp.SimilarityScore = 0.0

		// Async populate Redis + MongoDB Vector Cache.
		// Only request a Mongo write when we actually have a valid embedding.
		s.enqueueWriteBack(req, hashKey, vec, resp, len(vec) > 0)
		s.enqueueTelemetry(req, "MISS", 0.0, time.Since(start).Milliseconds(), resp)
		return resp, nil
	})

	if err != nil {
		return nil, err
	}

	baseResp := val.(*domain.CompletionResponse)
	finalResp := *baseResp
	finalResp.Coalesced = shared
	finalResp.LatencyMs = time.Since(start).Milliseconds()
	return &finalResp, nil
}

func (s *Service) enqueueWriteBack(req domain.PromptRequest, hashKey string, vec []float32, resp *domain.CompletionResponse, persistMongo bool) {
	select {
	case s.writeCh <- writeBackJob{req: req, hashKey: hashKey, vector: vec, resp: resp, persistMongo: persistMongo}:
	default:
		slog.Warn("write-back buffer full, dropping cache write", "tenant", req.TenantID)
	}
}

func (s *Service) enqueueTelemetry(req domain.PromptRequest, tier string, score float64, latencyMs int64, resp *domain.CompletionResponse) {
	select {
	case s.metricsCh <- telemetryJob{req: req, tier: tier, score: score, latencyMs: latencyMs, resp: resp}:
	default:
		slog.Warn("telemetry buffer full, dropping metric", "tenant", req.TenantID)
	}
}

// writeWorker drains the write-back channel only. Closing writeCh causes it to exit
// after processing all buffered jobs.
func (s *Service) writeWorker() {
	defer s.wg.Done()
	for job := range s.writeCh {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.redisCache.Set(ctx, job.req.TenantID, job.hashKey, job.resp)
		if job.persistMongo && len(job.vector) > 0 {
			_ = s.semanticCache.Save(ctx, job.req, job.hashKey, job.vector, job.resp)
		}
		cancel()
	}
}

// metricsWorker drains the telemetry channel only. Closing metricsCh causes it to exit
// after processing all buffered jobs.
func (s *Service) metricsWorker() {
	defer s.wg.Done()
	for mJob := range s.metricsCh {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.semanticCache.RecordTelemetry(ctx, mJob.req, mJob.tier, mJob.score, mJob.latencyMs, mJob.resp)
		cancel()
	}
}

// Shutdown drains both worker channels in order and waits for all goroutines to exit.
// writeCh is closed first so write-back workers finish before telemetry workers stop.
func (s *Service) Shutdown() {
	close(s.writeCh)
	close(s.metricsCh)
	s.wg.Wait()
}
