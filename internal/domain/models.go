package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var ErrRateLimitExceeded = errors.New("rate limit exceeded: token budget exhausted")

type RedisStore interface {
	AllowTPM(ctx context.Context, tenantID string, estimatedTokens int) (bool, error)
	Get(ctx context.Context, tenantID, hashKey string) (*CompletionResponse, bool)
	Set(ctx context.Context, tenantID, hashKey string, resp *CompletionResponse) error
}

type PromptRequest struct {
	TenantID  string  `json:"tenant_id"`
	Model     string  `json:"model"`
	Prompt    string  `json:"prompt"`
	Threshold float64 `json:"threshold"`
}

func (r PromptRequest) NormalizedPrompt() string {
	return strings.ToLower(strings.TrimSpace(r.Prompt))
}

func (r PromptRequest) CacheKey() string {
	raw := fmt.Sprintf("%s:%s:%s", r.TenantID, r.Model, r.NormalizedPrompt())
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

func (r PromptRequest) EstimatedTokens() int {
	est := len(r.Prompt) / 4
	if est < 1 {
		return 1
	}
	return est
}

type CompletionResponse struct {
	ResponseText    string  `json:"response_text"`
	Model           string  `json:"model"`
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	CacheTier       string  `json:"cache_tier"` // EXACT_HIT | SEMANTIC_HIT | MISS
	SimilarityScore float64 `json:"similarity_score"`
	Coalesced       bool    `json:"coalesced"`
	LatencyMs       int64   `json:"latency_ms"`
}

// ExactCache is the read/write interface for the L1 exact-match cache.
type ExactCache interface {
	Get(ctx context.Context, tenantID, hashKey string) (*CompletionResponse, bool)
	Set(ctx context.Context, tenantID, hashKey string, resp *CompletionResponse) error
}

type SemanticCache interface {
	Search(ctx context.Context, tenantID, model string, vector []float32, threshold float64) (*CompletionResponse, float64, bool)
	Save(ctx context.Context, req PromptRequest, hashKey string, vector []float32, resp *CompletionResponse) error
	RecordTelemetry(ctx context.Context, req PromptRequest, tier string, score float64, latencyMs int64, resp *CompletionResponse) error
}

type LLMClient interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Generate(ctx context.Context, req PromptRequest) (*CompletionResponse, error)
}
