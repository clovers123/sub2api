package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

type nvidiaAdaptiveThrottleCache struct {
	rdb *redis.Client
}

// NewNvidiaAdaptiveThrottleCache creates a Redis-backed adaptive-throttle cache.
// Lua scripts are preloaded on construction.
func NewNvidiaAdaptiveThrottleCache(rdb *redis.Client) service.NvidiaAdaptiveThrottleCache {
	ctx := context.Background()
	scripts := []*redis.Script{nvidiaReserveScript, nvidiaApplyScript}
	for _, s := range scripts {
		if err := s.Load(ctx, rdb).Err(); err != nil {
			log.Printf("[NvidiaAdaptiveThrottle] preload: %v", err)
		}
	}
	return &nvidiaAdaptiveThrottleCache{rdb: rdb}
}

// nvidiaAdaptiveThrottleKey builds the Redis key:
//
//	nvidia:adaptive:<accountID>:<hex(sha256(model))[:16]>
func nvidiaAdaptiveThrottleKey(accountID int64, model string) string {
	h := sha256.Sum256([]byte(model))
	return fmt.Sprintf("%s%d:%s", nvidiaAdaptiveThrottleKeyPrefix, accountID, hex.EncodeToString(h[:16]))
}

// Reserve atomically checks the send window.
func (c *nvidiaAdaptiveThrottleCache) Reserve(ctx context.Context, accountID int64, canonicalModel string) (service.NvidiaCacheReserveResult, error) {
	key := nvidiaAdaptiveThrottleKey(accountID, canonicalModel)
	nowMs := time.Now().UnixMilli()

	vals, err := nvidiaReserveScript.Run(ctx, c.rdb, []string{key}, nowMs, nvidiaThrottleTTLSec).Slice()
	if err != nil {
		return service.NvidiaCacheReserveResult{}, fmt.Errorf("nvidia adaptive throttle reserve: %w", err)
	}

	allowed, err := redisluaBool(vals, 0)
	if err != nil {
		return service.NvidiaCacheReserveResult{}, fmt.Errorf("nvidia adaptive throttle reserve allowed flag: %w", err)
	}
	retryMs, err := redisluaInt64(vals, 1)
	if err != nil {
		return service.NvidiaCacheReserveResult{}, fmt.Errorf("nvidia adaptive throttle reserve retry-ms: %w", err)
	}
	return service.NvidiaCacheReserveResult{Allowed: allowed, RetryAfterMs: retryMs}, nil
}

// Apply records a rate transition.
func (c *nvidiaAdaptiveThrottleCache) Apply(ctx context.Context, accountID int64, model string, rate service.NvidiaThrottleRate) error {
	key := nvidiaAdaptiveThrottleKey(accountID, model)
	nowMs := time.Now().UnixMilli()

	decayFlag := int64(0)
	if rate.DecaySpacing {
		decayFlag = 1
	}

	_, err := nvidiaApplyScript.Run(ctx, c.rdb, []string{key},
		nowMs,
		int64(nvidiaThrottleTTLSec),
		rate.SpacingMs,
		rate.ConsecutivePenalty,
		rate.CapacityPenaltyMs,
		int64(0), // rateBlockUntilMs — set to capacity penalty when needed
		decayFlag,
	).Int64()
	if err != nil {
		return fmt.Errorf("nvidia adaptive throttle apply: %w", err)
	}
	return nil
}

// redisluaBool decodes a 0/1 integer from a Lua return array.
func redisluaBool(arr []any, idx int) (bool, error) {
	if idx >= len(arr) {
		return false, fmt.Errorf("index %d out of range (len=%d)", idx, len(arr))
	}
	switch v := arr[idx].(type) {
	case int64:
		return v == 1, nil
	case string:
		return v == "1", nil
	default:
		return false, fmt.Errorf("unexpected type %T", arr[idx])
	}
}

// redisluaInt64 decodes an int64 from a Lua return array.
func redisluaInt64(arr []any, idx int) (int64, error) {
	if idx >= len(arr) {
		return 0, fmt.Errorf("index %d out of range (len=%d)", idx, len(arr))
	}
	switch v := arr[idx].(type) {
	case int64:
		return v, nil
	case string:
		var n int64
		_, err := fmt.Sscanf(v, "%d", &n)
		return n, err
	default:
		return 0, fmt.Errorf("unexpected type %T", arr[idx])
	}
}
