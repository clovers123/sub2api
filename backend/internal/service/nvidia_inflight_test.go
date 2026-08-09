//go:build unit

package service

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNVIDIAInflightTracker_AcquireRespectsMax(t *testing.T) {
	tr := newNVIDIAInflightTracker()
	scope := NVIDIAThrottleScope{AccountID: 7, CanonicalModel: "meta/llama-3.1-405b-instruct"}

	got := []bool{}
	for i := 0; i < 6; i++ {
		got = append(got, tr.Acquire(scope, 4))
	}
	require.Equal(t, []bool{true, true, true, true, false, false}, got,
		"前 4 个 acquire 成功，之后拒绝")
	require.Equal(t, 4, tr.Count(scope))
	require.Equal(t, 0, tr.Count(NVIDIAThrottleScope{AccountID: 8, CanonicalModel: "x"}), "不同 scope 互不影响")
}

func TestNVIDIAInflightTracker_ReleaseReturnsCapacity(t *testing.T) {
	tr := newNVIDIAInflightTracker()
	scope := NVIDIAThrottleScope{AccountID: 1, CanonicalModel: "m"}

	require.True(t, tr.Acquire(scope, 2))
	require.True(t, tr.Acquire(scope, 2))
	require.False(t, tr.Acquire(scope, 2))

	tr.Release(scope)
	require.Equal(t, 1, tr.Count(scope))
	require.True(t, tr.Acquire(scope, 2), "release 后恢复可 acquire")
	require.False(t, tr.Acquire(scope, 2))
}

func TestNVIDIAInflightTracker_ReleaseFloorZero(t *testing.T) {
	tr := newNVIDIAInflightTracker()
	scope := NVIDIAThrottleScope{AccountID: 2, CanonicalModel: "m"}

	tr.Release(scope)
	tr.Release(scope)
	require.Equal(t, 0, tr.Count(scope), "重复 release 不得为负")
}

func TestNVIDIAInflightTracker_ConcurrentAcquireBounded(t *testing.T) {
	tr := newNVIDIAInflightTracker()
	scope := NVIDIAThrottleScope{AccountID: 42, CanonicalModel: "meta/llama-3.1-405b-instruct"}
	const max = 8
	const workers = 200

	var wg sync.WaitGroup
	acquired := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acquired <- tr.Acquire(scope, max)
		}()
	}
	wg.Wait()
	close(acquired)

	total := 0
	for ok := range acquired {
		if ok {
			total++
		}
	}
	require.Equal(t, max, total, "200 goroutine 竞争 8 上限，恰好 8 个 acquire 成功")

	for i := 0; i < max; i++ {
		tr.Release(scope)
	}
	require.Equal(t, 0, tr.Count(scope), "全部 release 后归零")

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.Release(scope)
		}()
	}
	wg.Wait()
	require.Equal(t, 0, tr.Count(scope), "并发过度 release 也归零（floor 0）")
}

func TestNVIDIAInflightTracker_ReleaseDeletesZeroEntry(t *testing.T) {
	tr := newNVIDIAInflightTracker()
	scope := NVIDIAThrottleScope{AccountID: 1, CanonicalModel: "m"}

	require.True(t, tr.Acquire(scope, 2))
	require.Equal(t, 1, len(tr.inflight), "acquire 后 map 有 entry")
	tr.Release(scope)
	require.Equal(t, 0, len(tr.inflight), "归零后 entry 被删除，map 不再增长")

	// 多次 acquire/release 后 map 大小保持有界
	for i := 0; i < 100; i++ {
		tr.Acquire(scope, 1)
		tr.Release(scope)
	}
	require.Equal(t, 0, len(tr.inflight), "100 轮 acquire/release 后 map 仍为空")
}

func TestNVIDIAInflightTracker_MaxZeroDisablesGate(t *testing.T) {
	tr := newNVIDIAInflightTracker()
	scope := NVIDIAThrottleScope{AccountID: 1, CanonicalModel: "m"}
	require.True(t, tr.Acquire(scope, 0), "max=0 表示关闭并发控制，永远放行")
	require.True(t, tr.Acquire(scope, 0))
	require.Equal(t, 2, tr.Count(scope), "max=0 时仍计数（上层决定是否使用计数）")
}