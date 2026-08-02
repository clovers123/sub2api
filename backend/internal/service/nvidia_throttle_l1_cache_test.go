//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNvidiaThrottleL1_NilSafe(t *testing.T) {
	// nil cache 任何调用都不 panic, IsBlocked 返回 false (不命中)
	var l1 *nvidiaThrottleL1
	require.False(t, l1.IsBlocked(1, "model-x"))
	l1 = &nvidiaThrottleL1{} // cache 字段 nil
	require.False(t, l1.IsBlocked(1, "model-x"))

	// newNvidiaThrottleL1 不会返回 nil (除非 ristretto 内部失败)
	real := newNvidiaThrottleL1()
	require.NotNil(t, real)
}

func TestNvidiaThrottleL1_MarkBlockedThenIsBlocked(t *testing.T) {
	l1 := newNvidiaThrottleL1()
	require.NotNil(t, l1)

	// ristretto Set 是异步, 调用 Wait 确保命中
	l1.MarkBlocked(1001, "deepseek-ai/deepseek-v4-pro")
	l1.cache.Wait()

	// 写入后立即命中
	require.True(t, l1.IsBlocked(1001, "deepseek-ai/deepseek-v4-pro"))

	// 不同账号同 model 不命中
	require.False(t, l1.IsBlocked(1002, "deepseek-ai/deepseek-v4-pro"))

	// 同账号不同 model 不命中
	require.False(t, l1.IsBlocked(1001, "z-ai/glm-5.2"))
}

func TestNvidiaThrottleL1_TTLExpires(t *testing.T) {
	l1 := newNvidiaThrottleL1()
	require.NotNil(t, l1)

	l1.MarkBlocked(2002, "stepfun-ai/step-3.7-flash")
	l1.cache.Wait()
	require.True(t, l1.IsBlocked(2002, "stepfun-ai/step-3.7-flash"))

	// 等待 TTL 过期 + ristretto 内部 cleanup
	time.Sleep(nvidiaThrottleL1TTL + 200*time.Millisecond)
	require.False(t, l1.IsBlocked(2002, "stepfun-ai/step-3.7-flash"),
		"L1 TTL %v 后应自动失效, 不卡超过该窗口的 block", nvidiaThrottleL1TTL)
}

func TestNvidiaThrottleL1_ReserveShortCircuit(t *testing.T) {
	// L1 命中 → Reserve 直接返回 blocked, 不调 L2 (此处 L2 nil, 调用必崩所以证明没调)
	l1 := newNvidiaThrottleL1()
	require.NotNil(t, l1)
	l1.MarkBlocked(3003, "nvidia/nemotron-3-ultra-550b-a55b")
	l1.cache.Wait()

	svc := &RateLimitService{
		nvidiaAdaptiveThrottleCache: &panicNvidiaCache{}, // L2 调用即 panic
		nvidiaMetrics:               &NVIDIAAdaptiveThrottleMetrics{},
		nvidiaThrottleL1:            l1,
		nvidiaThrottleNow:           time.Now,
		nvidiaThrottleJitter:        defaultNVIDIAAdaptiveThrottleJitter,
		nvidiaThrottleSettings:      newNVIDIAAdaptiveThrottleSettingsCache(),
	}

	res, ok := svc.ReserveNVIDIAAdaptiveThrottle(context.Background(), NvidiaReserveRequest{
		Scope: NVIDIAThrottleScope{AccountID: 3003, CanonicalModel: "nvidia/nemotron-3-ultra-550b-a55b"},
	})
	require.True(t, ok)
	require.False(t, res.Allowed)
	require.NotNil(t, res.Err)
	// metrics: 1 reserve + 1 blocked, 0 cache_errors (没查 L2)
	require.Equal(t, int64(1), svc.nvidiaMetrics.Reserves.Load())
	require.Equal(t, int64(1), svc.nvidiaMetrics.Blocked.Load())
	require.Equal(t, int64(0), svc.nvidiaMetrics.CacheErrors.Load())
}

// panicNvidiaCache 任何方法调用都 panic, 用于证明 L1 短路没走 L2.
type panicNvidiaCache struct{}

func (p *panicNvidiaCache) Reserve(_ context.Context, _ int64, _ string) (NvidiaCacheReserveResult, error) {
	panic("L2 should not be called when L1 hits")
}
func (p *panicNvidiaCache) Apply(_ context.Context, _ int64, _ string, _ NvidiaThrottleRate) error {
	panic("Apply not expected in this test")
}

func TestNvidiaThrottleL1_BackfillOnL2Blocked(t *testing.T) {
	// L1 miss + L2 blocked → 回填 L1, 下次同 scope 直接命中 L1
	l1 := newNvidiaThrottleL1()
	require.NotNil(t, l1)

	svc := &RateLimitService{
		nvidiaAdaptiveThrottleCache: &fakeBlockedNvidiaCache{},
		nvidiaMetrics:               &NVIDIAAdaptiveThrottleMetrics{},
		nvidiaThrottleL1:            l1,
		nvidiaThrottleNow:           time.Now,
		nvidiaThrottleJitter:        defaultNVIDIAAdaptiveThrottleJitter,
		nvidiaThrottleSettings:      newNVIDIAAdaptiveThrottleSettingsCache(),
	}

	// 第一次: L1 miss, L2 blocked → 回填 L1
	res, ok := svc.ReserveNVIDIAAdaptiveThrottle(context.Background(), NvidiaReserveRequest{
		Scope: NVIDIAThrottleScope{AccountID: 4004, CanonicalModel: "minimaxai/minimax-m3"},
	})
	require.True(t, ok)
	require.False(t, res.Allowed)
	l1.cache.Wait() // ristretto 异步写入完成

	// 第二次: 应该 L1 命中, 不再调 L2
	// 替换 L2 为 panic cache 验证没有走 L2
	svc.nvidiaAdaptiveThrottleCache = &panicNvidiaCache{}
	res2, ok2 := svc.ReserveNVIDIAAdaptiveThrottle(context.Background(), NvidiaReserveRequest{
		Scope: NVIDIAThrottleScope{AccountID: 4004, CanonicalModel: "minimaxai/minimax-m3"},
	})
	require.True(t, ok2)
	require.False(t, res2.Allowed)
}

type fakeBlockedNvidiaCache struct{}

func (f *fakeBlockedNvidiaCache) Reserve(_ context.Context, _ int64, _ string) (NvidiaCacheReserveResult, error) {
	return NvidiaCacheReserveResult{Allowed: false, RetryAfterMs: 30000}, nil
}
func (f *fakeBlockedNvidiaCache) Apply(_ context.Context, _ int64, _ string, _ NvidiaThrottleRate) error {
	return nil
}

func TestNvidiaThrottleL1_RecordOutcomeWritesL1(t *testing.T) {
	// RecordNVIDIAAdaptiveThrottleOutcome 在 Rate / Capacity signal 时写 L1
	l1 := newNvidiaThrottleL1()
	require.NotNil(t, l1)

	svc := &RateLimitService{
		nvidiaAdaptiveThrottleCache: &fakeNoOpNvidiaCache{},
		nvidiaMetrics:               &NVIDIAAdaptiveThrottleMetrics{},
		nvidiaThrottleL1:            l1,
		nvidiaThrottleNow:           time.Now,
		nvidiaThrottleJitter:        defaultNVIDIAAdaptiveThrottleJitter,
		nvidiaThrottleSettings:      newNVIDIAAdaptiveThrottleSettingsCache(),
	}

	// 429 = Rate signal → 应写 L1
	wrote := svc.RecordNVIDIAAdaptiveThrottleOutcome(context.Background(), NvidiaThrottleUpdate{
		Scope:        NVIDIAThrottleScope{AccountID: 5005, CanonicalModel: "z-ai/glm-5.2"},
		StatusCode:   http.StatusTooManyRequests,
		ResponseBody:  nil,
	})
	require.True(t, wrote)
	l1.cache.Wait() // ristretto 异步写入完成
	require.True(t, l1.IsBlocked(5005, "z-ai/glm-5.2"), "429 应写 L1")

	// 200 success 信号也走 Apply (返回 true), 但不应写 L1 (L1 只反映节流状态)
	wrote2 := svc.RecordNVIDIAAdaptiveThrottleOutcome(context.Background(), NvidiaThrottleUpdate{
		Scope:        NVIDIAThrottleScope{AccountID: 6006, CanonicalModel: "z-ai/glm-5.2"},
		StatusCode:   http.StatusOK,
		ResponseBody:  nil,
	})
	require.True(t, wrote2, "200 success 仍走 Apply (decays spacing)")
	require.False(t, l1.IsBlocked(6006, "z-ai/glm-5.2"), "200 success 不应写 L1")
}

type fakeNoOpNvidiaCache struct{}

func (f *fakeNoOpNvidiaCache) Reserve(_ context.Context, _ int64, _ string) (NvidiaCacheReserveResult, error) {
	return NvidiaCacheReserveResult{Allowed: true}, nil
}
func (f *fakeNoOpNvidiaCache) Apply(_ context.Context, _ int64, _ string, _ NvidiaThrottleRate) error {
	return nil
}
