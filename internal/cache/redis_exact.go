package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"semantic-ai-gateway/internal/domain"

	"github.com/redis/go-redis/v9"
)

var tpmLuaScript = redis.NewScript(`
local current = tonumber(redis.call("GET", KEYS[1]) or "0")
local estimated = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])

if current + estimated > limit then
    return {0, limit - current}
end

local updated = redis.call("INCRBY", KEYS[1], estimated)
if updated == estimated then
    redis.call("EXPIRE", KEYS[1], 65)
end
return {1, limit - updated}
`)

type RedisCache struct {
	client   *redis.Client
	ttl      time.Duration
	tpmLimit int
}

func NewRedisCache(addr string, ttl time.Duration, tpmLimit int) *RedisCache {
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		PoolSize:     50,
		MinIdleConns: 10,
	})
	return &RedisCache{
		client:   rdb,
		ttl:      ttl,
		tpmLimit: tpmLimit,
	}
}

func (r *RedisCache) AllowTPM(ctx context.Context, tenantID string, estimatedTokens int) (bool, error) {
	minuteWindow := time.Now().UTC().Format("200601021504")
	key := fmt.Sprintf("ratelimit:tpm:%s:%s", tenantID, minuteWindow)

	res, err := tpmLuaScript.Run(ctx, r.client, []string{key}, estimatedTokens, r.tpmLimit).Slice()
	if err != nil {
		return false, err
	}
	allowed, ok := res[0].(int64)
	return ok && allowed == 1, nil
}

func (r *RedisCache) Get(ctx context.Context, tenantID, hashKey string) (*domain.CompletionResponse, bool) {
	key := fmt.Sprintf("cache:exact:%s:%s", tenantID, hashKey)
	data, err := r.client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}

	var resp domain.CompletionResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, false
	}
	return &resp, true
}

func (r *RedisCache) Set(ctx context.Context, tenantID, hashKey string, resp *domain.CompletionResponse) error {
	key := fmt.Sprintf("cache:exact:%s:%s", tenantID, hashKey)
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, key, data, r.ttl).Err()
}

func (r *RedisCache) Close() error {
	return r.client.Close()
}
