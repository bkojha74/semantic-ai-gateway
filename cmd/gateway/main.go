package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"google.golang.org/genai"

	"semantic-ai-gateway/internal/cache"
	"semantic-ai-gateway/internal/config"
	"semantic-ai-gateway/internal/domain"
	"semantic-ai-gateway/internal/gateway"
)

type GeminiAdapter struct {
	client         *genai.Client
	embeddingModel string
}

func NewGeminiAdapter(ctx context.Context, apiKey, embeddingModel string) (*GeminiAdapter, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}
	return &GeminiAdapter{client: client, embeddingModel: embeddingModel}, nil
}

func (g *GeminiAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	dim := int32(768)
	model := g.embeddingModel
	if model == "" {
		model = "gemini-embedding-001"
	}
	res, err := g.client.Models.EmbedContent(ctx, model, genai.Text(text), &genai.EmbedContentConfig{
		OutputDimensionality: &dim,
	})
	if err != nil || len(res.Embeddings) == 0 {
		return nil, err
	}
	return res.Embeddings[0].Values, nil
}

func (g *GeminiAdapter) Generate(ctx context.Context, req domain.PromptRequest) (*domain.CompletionResponse, error) {
	candidates := []string{req.Model}
	for _, fallback := range []string{"gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-3.7-flash"} {
		if fallback != req.Model {
			candidates = append(candidates, fallback)
		}
	}

	var lastErr error
	for _, modelName := range candidates {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		attemptCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		res, err := g.client.Models.GenerateContent(attemptCtx, modelName, genai.Text(req.Prompt), nil)
		cancel()

		if err == nil {
			inTokens, outTokens := 0, 0
			if res.UsageMetadata != nil {
				inTokens = int(res.UsageMetadata.PromptTokenCount)
				outTokens = int(res.UsageMetadata.CandidatesTokenCount)
			}

			return &domain.CompletionResponse{
				ResponseText: res.Text(),
				Model:        modelName,
				InputTokens:  inTokens,
				OutputTokens: outTokens,
			}, nil
		}

		lastErr = err
		slog.Warn("upstream model generation failed, trying next candidate", "model", modelName, "err", err)
	}

	return nil, lastErr
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := config.Load()
	if cfg.GeminiAPIKey == "" {
		slog.Error("GEMINI_API_KEY is required (set it in .env or as environment variable)")
		os.Exit(1)
	}

	// 1. Connect MongoDB v2
	mongoClient, err := mongo.Connect(options.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		slog.Error("failed to connect to MongoDB", "err", err)
		os.Exit(1)
	}
	defer func() { _ = mongoClient.Disconnect(context.Background()) }()

	// 2. Initialize Caches & Gemini Client
	redisCache := cache.NewRedisCache(cfg.RedisAddr, 1*time.Hour, 100000)
	defer func() { _ = redisCache.Close() }()

	semanticCache := cache.NewMongoSemanticCache(mongoClient, "ai_gateway", 7*24*time.Hour)
	llmClient, err := NewGeminiAdapter(ctx, cfg.GeminiAPIKey, cfg.EmbeddingModel)
	if err != nil {
		slog.Error("failed to initialize Gemini client", "err", err)
		os.Exit(1)
	}

	go func() {
		listCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		page, err := llmClient.client.Models.List(listCtx, nil)
		if err == nil {
			var names []string
			for _, m := range page.Items {
				names = append(names, m.Name)
			}
			slog.Info("connected to Gemini API", "available_models", names)
		} else {
			slog.Warn("could not list Gemini models on startup", "err", err)
		}
	}()

	svc := gateway.NewService(redisCache, semanticCache, llmClient, 4)

	// 3. Register HTTP Routes
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("POST /v1/generate", func(w http.ResponseWriter, r *http.Request) {
		var req domain.PromptRequest
		bodyBytes, _ := io.ReadAll(r.Body)
		slog.Debug("raw request body", "body", string(bodyBytes), "remote", r.RemoteAddr)
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			slog.Warn("failed to decode request body", "err", err, "body_prefix", string(bodyBytes[:min(len(bodyBytes), 32)]), "remote", r.RemoteAddr)
			http.Error(w, `{"error":"invalid JSON payload"}`, http.StatusBadRequest)
			return
		}
		if req.TenantID == "" {
			req.TenantID = r.Header.Get("X-Tenant-ID")
		}
		if req.TenantID == "" || req.Prompt == "" {
			http.Error(w, `{"error":"tenant_id and prompt are required"}`, http.StatusBadRequest)
			return
		}

		resp, err := svc.Generate(r.Context(), req)
		if err != nil {
			if errors.Is(err, domain.ErrRateLimitExceeded) {
				w.Header().Set("Retry-After", "60")
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
			slog.Error("generation error", "err", err)
			http.Error(w, `{"error":"upstream generation failed"}`, http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache-Tier", resp.CacheTier)
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
	}

	go func() {
		slog.Info("Semantic AI Gateway listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server failure", "err", err)
		}
	}()

	<-ctx.Done()
	slog.Info("Shutting down gateway...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	svc.Shutdown()
	slog.Info("Gateway stopped cleanly")
}
