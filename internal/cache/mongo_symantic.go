package cache

import (
	"context"
	"time"

	"semantic-ai-gateway/internal/domain"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type MongoSemanticCache struct {
	cacheColl     *mongo.Collection
	telemetryColl *mongo.Collection
	ttl           time.Duration
}

func NewMongoSemanticCache(client *mongo.Client, dbName string, ttl time.Duration) *MongoSemanticCache {
	db := client.Database(dbName)
	return &MongoSemanticCache{
		cacheColl:     db.Collection("semantic_cache"),
		telemetryColl: db.Collection("usage_telemetry"),
		ttl:           ttl,
	}
}

type vectorSearchResult struct {
	ResponseText string  `bson:"response_text"`
	Model        string  `bson:"model"`
	InputTokens  int     `bson:"input_tokens"`
	OutputTokens int     `bson:"output_tokens"`
	Score        float64 `bson:"score"`
}

func (m *MongoSemanticCache) Search(ctx context.Context, tenantID, model string, vector []float32, threshold float64) (*domain.CompletionResponse, float64, bool) {
	if len(vector) == 0 {
		return nil, 0, false
	}

	pipeline := mongo.Pipeline{
		{{Key: "$vectorSearch", Value: bson.D{
			{Key: "index", Value: "vector_idx"},
			{Key: "path", Value: "embedding"},
			{Key: "queryVector", Value: vector},
			{Key: "numCandidates", Value: 50},
			{Key: "limit", Value: 1},
			{Key: "filter", Value: bson.D{
				{Key: "tenant_id", Value: tenantID},
				{Key: "model", Value: model},
			}},
		}}},
		{{Key: "$project", Value: bson.D{
			{Key: "response_text", Value: 1},
			{Key: "model", Value: 1},
			{Key: "input_tokens", Value: "$token_usage.input_tokens"},
			{Key: "output_tokens", Value: "$token_usage.output_tokens"},
			{Key: "score", Value: bson.D{{Key: "$meta", Value: "vectorSearchScore"}}},
		}}},
	}

	cursor, err := m.cacheColl.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, 0, false
	}
	defer cursor.Close(ctx)

	var results []vectorSearchResult
	if err := cursor.All(ctx, &results); err != nil || len(results) == 0 {
		return nil, 0, false
	}

	top := results[0]
	if top.Score < threshold {
		return nil, top.Score, false
	}

	return &domain.CompletionResponse{
		ResponseText: top.ResponseText,
		Model:        top.Model,
		InputTokens:  top.InputTokens,
		OutputTokens: top.OutputTokens,
	}, top.Score, true
}

func (m *MongoSemanticCache) Save(ctx context.Context, req domain.PromptRequest, hashKey string, vector []float32, resp *domain.CompletionResponse) error {
	now := time.Now().UTC()
	filter := bson.D{
		{Key: "tenant_id", Value: req.TenantID},
		{Key: "model", Value: req.Model},
		{Key: "prompt_hash", Value: hashKey},
	}

	update := bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "prompt_text", Value: req.Prompt},
			{Key: "embedding", Value: vector},
			{Key: "response_text", Value: resp.ResponseText},
			{Key: "token_usage", Value: bson.D{
				{Key: "input_tokens", Value: resp.InputTokens},
				{Key: "output_tokens", Value: resp.OutputTokens},
			}},
			{Key: "created_at", Value: now},
			{Key: "expires_at", Value: now.Add(m.ttl)},
		}},
		{Key: "$inc", Value: bson.D{{Key: "hit_count", Value: 1}}},
	}

	opts := options.UpdateOne().SetUpsert(true)
	_, err := m.cacheColl.UpdateOne(ctx, filter, update, opts)
	return err
}

func (m *MongoSemanticCache) RecordTelemetry(ctx context.Context, req domain.PromptRequest, tier string, score float64, latencyMs int64, resp *domain.CompletionResponse) error {
	tokensSaved := 0
	if tier != "MISS" && resp != nil {
		tokensSaved = resp.InputTokens + resp.OutputTokens
	}

	doc := bson.D{
		{Key: "timestamp", Value: time.Now().UTC()},
		{Key: "meta", Value: bson.D{
			{Key: "tenant_id", Value: req.TenantID},
			{Key: "model", Value: req.Model},
			{Key: "cache_tier", Value: tier},
		}},
		{Key: "latency_ms", Value: latencyMs},
		{Key: "similarity_score", Value: score},
		{Key: "tokens_saved", Value: tokensSaved},
		{Key: "estimated_cost_saved_usd", Value: float64(tokensSaved) * 0.00000025},
	}

	_, err := m.telemetryColl.InsertOne(ctx, doc)
	return err
}
