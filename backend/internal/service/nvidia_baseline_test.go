//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNVIDIABaselineRecordsAndResets(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	now := time.Now()

	// 初始无基线
	require.Equal(t, int64(0), m.SnapshotAccountBaseline(1))

	// 3 次成功 → 基线 3
	m.RecordAccountSuccess(1, now)
	m.RecordAccountSuccess(1, now.Add(time.Second))
	m.RecordAccountSuccess(1, now.Add(2*time.Second))
	require.Equal(t, int64(3), m.SnapshotAccountBaseline(1))

	// 窗口过期（61 秒后）→ 基线归 0
	m.RecordAccountSuccess(1, now.Add(time.Duration(nvidiaBaselineWindowSeconds+1)*time.Second))
	require.Equal(t, int64(1), m.SnapshotAccountBaseline(1), "过期后新窗口从 1 开始")

	// 从未记录的账号 → 0
	require.Equal(t, int64(0), m.SnapshotAccountBaseline(999))
}

func TestNVIDIABaselineViaRecordAccountReuse(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	now := time.Now()

	// RecordAccountReuse 也是成功请求 → 计入基线
	m.RecordAccountReuse(7, now)
	m.RecordAccountReuse(7, now.Add(time.Second))
	require.Equal(t, int64(2), m.SnapshotAccountBaseline(7))
}

func TestNVIDIASchedulingSortPrefersHighBaselineInSameHeatBucket(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	now := time.Now()

	// 两个账号：同样的 priority、LastUsedAt、reuse heat，但基线不同
	accLow := &Account{ID: 1, Priority: 0, LastUsedAt: &now}
	accHigh := &Account{ID: 2, Priority: 0, LastUsedAt: &now}
	// 同样 reuse 5 次（heat = 5）
	for i := 0; i < 5; i++ {
		m.RecordAccountReuse(1, now)
		m.RecordAccountReuse(2, now)
	}
	// 高基线账号额外 10 次成功（窗口内）→ baseline=15，低基线账号 baseline=5
	for i := 0; i < 10; i++ {
		m.RecordAccountSuccess(2, now.Add(time.Duration(i)*time.Second))
	}

	sorted := sortNvidiaThrottleSchedulingOrder([]*Account{accLow, accHigh}, m)
	require.Equal(t, int64(2), sorted[0].ID, "同 heat 桶内基线高的账号优先")

	// 验证 heat 相同（确保测试前置条件成立）
	require.Equal(t, nvidiaReuseHeatFor(accLow, m), nvidiaReuseHeatFor(accHigh, m))
	// 基线确实不同
	require.Greater(t, nvidiaBaselineRPMFor(accHigh, m), nvidiaBaselineRPMFor(accLow, m))
}

// TestNVIDIASchedulingBaselineDoesNotCrossPriorityBuckets verifies that the
// baseline sort pass does NOT reorder accounts across priority boundaries when
// both accounts share identical heat (e.g. 0 on cold start). Regression test
// for sameNvidiaHeatBucket missing sameNvidiaPriorityAndLastUsedAtSecond.
func TestNVIDIASchedulingBaselineDoesNotCrossPriorityBuckets(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	now := time.Now()

	// Two accounts with different priorities, both heat=0 (never reused).
	// Priority 1 (higher priority) MUST come first regardless of baseline RPM.
	accP1 := &Account{ID: 1, Priority: 1, LastUsedAt: &now}
	accP2 := &Account{ID: 2, Priority: 2, LastUsedAt: &now}
	// Give P2 higher baseline RPM (10 vs 0).
	for i := 0; i < 10; i++ {
		m.RecordAccountSuccess(2, now.Add(time.Duration(i)*time.Second))
	}

	sorted := sortNvidiaThrottleSchedulingOrder([]*Account{accP2, accP1}, m)

	require.Equal(t, int64(1), sorted[0].ID,
		"high-priority account (priority=1) MUST come before low-priority (priority=2)")
	require.Equal(t, int64(2), sorted[1].ID,
		"low-priority account must be second regardless of higher RPM baseline")
}
