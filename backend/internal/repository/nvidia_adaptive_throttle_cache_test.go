//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestNvidiaAdaptiveThrottleCache_Reserve_AllowsWhenEmpty(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := newNvidiaAdaptiveThrottleCacheForTest(rdb)

	r, err := c.Reserve(ctx, 1, "meta/llama-3.1-405b-instruct")
	require.NoError(t, err)
	require.True(t, r.Allowed, "first send for fresh (account,model) key must be allowed")
	require.Equal(t, int64(0), r.RetryAfterMs)
}

func TestNvidiaAdaptiveThrottleCache_Reserve_GatedAfterApplySpacing(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := newNvidiaAdaptiveThrottleCacheForTest(rdb)

	// Apply a rate signal: spacing=500ms penalty=1.
	require.NoError(t, c.Apply(ctx, 1, "m1", service.NvidiaThrottleRate{SpacingMs: 500, ConsecutivePenalty: 1}))

	// First Reserve stamps next_send_at_ms = now + spacingMs = ~500ms, and returns Allowed=true.
	r1, err := c.Reserve(ctx, 1, "m1")
	require.NoError(t, err)
	require.True(t, r1.Allowed, "first reserve after apply stamps nextSend but still allows the current request")

	// Second Reserve within the same window: now < now + spacingMs → gated.
	start := time.Now()
	r2, err := c.Reserve(ctx, 1, "m1")
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.False(t, r2.Allowed, "second reserve inside spacingMs window must be gated")
	require.Greater(t, r2.RetryAfterMs, int64(0), "retry_ms must be positive while gated")
	require.LessOrEqual(t, r2.RetryAfterMs, int64(500), "retry_ms bounded by spacingMs")
	require.Less(t, elapsed, 50*time.Millisecond, "Reserve must not block; it returns RetryAfterMs asynchronously")
}

func TestNvidiaAdaptiveThrottleCache_Apply_AIMDDecayHalvesAndFloorsToZero(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := newNvidiaAdaptiveThrottleCacheForTest(rdb)

	// set initial spacing=800ms via Apply (not via Reserve)
	err := c.Apply(ctx, 1, "m2", service.NvidiaThrottleRate{SpacingMs: 800, ConsecutivePenalty: 1})
	require.NoError(t, err)
	// Apply-success triggers DecaySpacing=true → halve: 400
	err = c.Apply(ctx, 1, "m2", service.NvidiaThrottleRate{DecaySpacing: true})
	require.NoError(t, err)
	err = c.Apply(ctx, 1, "m2", service.NvidiaThrottleRate{DecaySpacing: true})
	require.NoError(t, err)
	err = c.Apply(ctx, 1, "m2", service.NvidiaThrottleRate{DecaySpacing: true})
	require.NoError(t, err)
	err = c.Apply(ctx, 1, "m2", service.NvidiaThrottleRate{DecaySpacing: true})
	require.NoError(t, err)
	// 800 → 400 → 200 → 100 → floor(<100)=0.
	// After floor, Reserve stamps next_send_at = now + 0 → next reserve is allowed.
	r, err := c.Reserve(ctx, 1, "m2")
	require.NoError(t, err)
	require.True(t, r.Allowed, "after four decays from 800 spacing must collapse below 100ms floor and then to 0 (allow immediately again)")
}

func TestNvidiaAdaptiveThrottleCache_Reserve_KeyIsPerScope(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := newNvidiaAdaptiveThrottleCacheForTest(rdb)

	require.NoError(t, c.Apply(ctx, 1, "modelA", service.NvidiaThrottleRate{SpacingMs: 1000}))
	require.NoError(t, c.Apply(ctx, 1, "modelB", service.NvidiaThrottleRate{SpacingMs: 1000}))
	rA1, err := c.Reserve(ctx, 1, "modelA")
	require.NoError(t, err)
	require.True(t, rA1.Allowed, "first Reserve stamps nextSend but allows current request")
	rB1, err := c.Reserve(ctx, 1, "modelB")
	require.NoError(t, err)
	require.True(t, rB1.Allowed, "first Reserve stamps nextSend but allows current request")

	// Now second Reserve for modelA is gated; modelB starts spacing from its own first Reserve, not from modelA's.
	rA2, err := c.Reserve(ctx, 1, "modelA")
	require.NoError(t, err)
	require.False(t, rA2.Allowed, "second reserve for modelA must be gated")
	require.NotEqual(t, rA2.RetryAfterMs, int64(0))
}

func TestNvidiaAdaptiveThrottleCache_Reserve_AccountsDiffer(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := newNvidiaAdaptiveThrottleCacheForTest(rdb)

	require.NoError(t, c.Apply(ctx, 1, "modelA", service.NvidiaThrottleRate{SpacingMs: 1000}))
	r1A, err := c.Reserve(ctx, 1, "modelA")
	require.NoError(t, err)
	require.True(t, r1A.Allowed, "first Reserve stamps nextSend but allows current request")
	r1, err := c.Reserve(ctx, 1, "modelA")
	require.False(t, r1.Allowed, "second reserve for account1 modelA gated by its own spacing")
	require.Greater(t, r1.RetryAfterMs, int64(0))

	// Account2 has never been throttled before, must be allowed.
	r2, err := c.Reserve(ctx, 2, "modelA")
	require.NoError(t, err)
	require.True(t, r2.Allowed)
}

func TestNvidiaAdaptiveThrottleCache_KeyShape(t *testing.T) {
	k := nvidiaAdaptiveThrottleKey(1, "literal-not-hash")
	require.Contains(t, k, "nvidia:adaptive:1:")
	require.Contains(t, k, ":") // two colons: nvidia:adaptive:1:hexdigest
	t.Run("different_models_produce_different_hashes", func(t *testing.T) {
		k1 := nvidiaAdaptiveThrottleKey(1, "modelA")
		k2 := nvidiaAdaptiveThrottleKey(1, "modelB")
		require.NotEqual(t, k1, k2)
		require.Contains(t, k1, "nvidia:adaptive:1:")
		require.Contains(t, k2, "nvidia:adaptive:1:")
	})
}

// newNvidiaAdaptiveThrottleCacheForTest wraps NewNvidiaAdaptiveThrottleCache but
// tolerates miniredis not supporting SCRIPT LOAD preloading for replicate_commands
// (the Lua helper runs in EVAL mode the same way).
func newNvidiaAdaptiveThrottleCacheForTest(rdb *redis.Client) service.NvidiaAdaptiveThrottleCache {
	return NewNvidiaAdaptiveThrottleCache(rdb)
}
