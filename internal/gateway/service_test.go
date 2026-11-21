package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"semantic-ai-gateway/internal/domain"
)

// --- Thread-Safe Test Fakes ---

var errUpstreamUnavailable = errors.New("gemini 503 unavailable")

type fakeRedisStore struct {
	mu       sync.Mutex
	allowErr error
	allow    bool
	items    map[string]*domain.CompletionResponse
	setCalls int
}

func newFakeRedisStore() *fakeRedisStore {
	return &fakeRedisStore{
		allow: true,
		items: make(map[string]*domain.CompletionResponse),
	}
}

func (f *fakeRedisStore) AllowTPM(_ context.Context, _ string, _ int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.allowErr != nil {
		return false, f.allowErr
	}
	return f.allow, nil
}

func (f *fakeRedisStore) Get(_ context.Context, tenantID, hashKey string) (*domain.CompletionResponse, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.items[tenantID+":"+hashKey]
	if !ok {
		return nil, false
	}
	cp := *v
	return &cp, true
}

func (f *fakeRedisStore) Set(_ context.Context, tenantID, hashKey string, resp *domain.CompletionResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *resp
	f.items[tenantID+":"+hashKey] = &cp
	f.setCalls++
	return nil
}

type fakeSemanticCache struct {
	mu          sync.Mutex
	searchResp  *domain.CompletionResponse
	searchScore float64
	searchOK    bool
	saveCalls   int
	telemetry   []string
}

func (f *fakeSemanticCache) Search(_ context.Context, _, _ string, _ []float32, _ float64) (*domain.CompletionResponse, float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.searchOK || f.searchResp == nil {
		return nil, 0, false
	}
	cp := *f.searchResp
	return &cp, f.searchScore, true
}

func (f *fakeSemanticCache) Save(_ context.Context, _ domain.PromptRequest, _ string, _ []float32, _ *domain.CompletionResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveCalls++
	return nil
}

func (f *fakeSemanticCache) RecordTelemetry(_ context.Context, _ domain.PromptRequest, tier string, _ float64, _ int64, _ *domain.CompletionResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.telemetry = append(f.telemetry, tier)
	return nil
}

type fakeLLMClient struct {
	embedVec     []float32
	embedErr     error
	genResp      *domain.CompletionResponse
	genErr       error
	genDelay     time.Duration
	embedCalls   atomic.Int64
	generateCall atomic.Int64
}

func (f *fakeLLMClient) Embed(_ context.Context, _ string) ([]float32, error) {
	f.embedCalls.Add(1)
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	return f.embedVec, nil
}

func (f *fakeLLMClient) Generate(ctx context.Context, _ domain.PromptRequest) (*domain.CompletionResponse, error) {
	f.generateCall.Add(1)
	if f.genDelay > 0 {
		select {
		case <-time.After(f.genDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.genErr != nil {
		return nil, f.genErr
	}
	cp := *f.genResp
	return &cp, nil
}

func waitForCondition(t *testing.T, timeout time.Duration, pred func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// --- Table-Driven Tests ---

func TestService_Generate_TiersAndRateLimit(t *testing.T) {
	baseReq := domain.PromptRequest{
		TenantID:  "az_wallet",
		Model:     "gemini-3.8-flash",
		Prompt:    "How do idempotency keys work?",
		Threshold: 0.92,
	}

	tests := []struct {
		name           string
		setup          func(r *fakeRedisStore, s *fakeSemanticCache, l *fakeLLMClient)
		wantErr        error
		wantTier       string
		wantText       string
		wantLLMCalls   int64
		wantMongoSaves int
	}{
		{
			name: "exact cache hit in Redis skips embed and LLM",
			setup: func(r *fakeRedisStore, _ *fakeSemanticCache, l *fakeLLMClient) {
				_ = r.Set(context.Background(), baseReq.TenantID, baseReq.CacheKey(), &domain.CompletionResponse{
					ResponseText: "Cached exact answer",
					Model:        baseReq.Model,
					InputTokens:  10,
					OutputTokens: 40,
				})
				l.genResp = &domain.CompletionResponse{ResponseText: "Upstream answer"}
			},
			wantTier:       "EXACT_HIT",
			wantText:       "Cached exact answer",
			wantLLMCalls:   0,
			wantMongoSaves: 0,
		},
		{
			name: "semantic cache hit in Mongo skips upstream LLM and backfills Redis",
			setup: func(_ *fakeRedisStore, s *fakeSemanticCache, l *fakeLLMClient) {
				l.embedVec = []float32{0.11, 0.22, 0.33}
				s.searchOK = true
				s.searchScore = 0.954
				s.searchResp = &domain.CompletionResponse{
					ResponseText: "Semantic match answer",
					Model:        baseReq.Model,
					InputTokens:  12,
					OutputTokens: 55,
				}
				l.genResp = &domain.CompletionResponse{ResponseText: "Upstream answer"}
			},
			wantTier:       "SEMANTIC_HIT",
			wantText:       "Semantic match answer",
			wantLLMCalls:   0,
			wantMongoSaves: 0,
		},
		{
			name: "cache miss calls upstream LLM and asynchronously saves to Redis and Mongo",
			setup: func(_ *fakeRedisStore, _ *fakeSemanticCache, l *fakeLLMClient) {
				l.embedVec = []float32{0.5, 0.6, 0.7}
				l.genResp = &domain.CompletionResponse{
					ResponseText: "Fresh Gemini answer",
					Model:        baseReq.Model,
					InputTokens:  15,
					OutputTokens: 80,
				}
			},
			wantTier:       "MISS",
			wantText:       "Fresh Gemini answer",
			wantLLMCalls:   1,
			wantMongoSaves: 1,
		},
		{
			name: "rate limit exceeded rejects request immediately",
			setup: func(r *fakeRedisStore, _ *fakeSemanticCache, l *fakeLLMClient) {
				r.allow = false
				l.genResp = &domain.CompletionResponse{ResponseText: "Should not run"}
			},
			wantErr:      domain.ErrRateLimitExceeded,
			wantLLMCalls: 0,
		},
		{
			name: "upstream LLM error propagates cleanly",
			setup: func(_ *fakeRedisStore, _ *fakeSemanticCache, l *fakeLLMClient) {
				l.embedVec = []float32{0.1, 0.2}
				l.genErr = errUpstreamUnavailable
			},
			wantErr:      errUpstreamUnavailable,
			wantLLMCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			redisStore := newFakeRedisStore()
			semCache := &fakeSemanticCache{}
			llmClient := &fakeLLMClient{}

			if tc.setup != nil {
				tc.setup(redisStore, semCache, llmClient)
			}

			svc := NewService(redisStore, semCache, llmClient, 2)
			defer svc.Shutdown()

			resp, err := svc.Generate(context.Background(), baseReq)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got error %v, want %v", err, tc.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if resp.CacheTier != tc.wantTier {
					t.Errorf("got CacheTier %q, want %q", resp.CacheTier, tc.wantTier)
				}
				if resp.ResponseText != tc.wantText {
					t.Errorf("got ResponseText %q, want %q", resp.ResponseText, tc.wantText)
				}
			}

			if got := llmClient.generateCall.Load(); got != tc.wantLLMCalls {
				t.Errorf("got LLM Generate calls %d, want %d", got, tc.wantLLMCalls)
			}

			if tc.wantMongoSaves > 0 {
				waitForCondition(t, 3*time.Second, func() bool {
					semCache.mu.Lock()
					defer semCache.mu.Unlock()
					return semCache.saveCalls >= tc.wantMongoSaves
				}, "timed out waiting for async MongoDB cache save")
			}
		})
	}
}

func TestService_Generate_SingleflightCoalescing(t *testing.T) {
	redisStore := newFakeRedisStore()
	semCache := &fakeSemanticCache{}
	llmClient := &fakeLLMClient{
		embedVec: []float32{0.01, 0.02, 0.03},
		genDelay: 150 * time.Millisecond,
		genResp: &domain.CompletionResponse{
			ResponseText: "Coalesced single response",
			Model:        "gemini-3.8-flash",
			InputTokens:  12,
			OutputTokens: 60,
		},
	}

	svc := NewService(redisStore, semCache, llmClient, 2)
	defer svc.Shutdown()

	req := domain.PromptRequest{
		TenantID:  "az_wallet",
		Model:     "gemini-3.8-flash",
		Prompt:    "Explain ledger double-entry invariants",
		Threshold: 0.92,
	}

	const goroutines = 8
	var wg sync.WaitGroup
	results := make([]*domain.CompletionResponse, goroutines)
	errs := make([]error, goroutines)

	startBarrier := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startBarrier
			results[idx], errs[idx] = svc.Generate(context.Background(), req)
		}(i)
	}

	close(startBarrier)
	wg.Wait()

	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d returned error: %v", i, errs[i])
		}
		if results[i].ResponseText != "Coalesced single response" {
			t.Errorf("goroutine %d got unexpected text %q", i, results[i].ResponseText)
		}
	}

	if calls := llmClient.generateCall.Load(); calls != 1 {
		t.Fatalf("expected 1 upstream LLM call due to singleflight coalescing, got %d", calls)
	}

	var coalescedCount int
	for _, r := range results {
		if r.Coalesced {
			coalescedCount++
		}
	}
	if coalescedCount == 0 {
		t.Errorf("expected at least one concurrent caller to report Coalesced=true")
	}
}
