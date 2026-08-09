//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newPriorityLastUsed(id int64, priority int, lastUsedAt *time.Time) *Account {
	a := &Account{ID: id, Priority: priority, LastUsedAt: lastUsedAt}
	return a
}

func TestSortNvidiaThrottleSchedulingOrder_EmptyReturnsEmpty(t *testing.T) {
	require.Empty(t, sortNvidiaThrottleSchedulingOrder(nil, nil))
	require.Empty(t, sortNvidiaThrottleSchedulingOrder([]*Account{}, nil))
}

func TestSortNvidiaThrottleSchedulingOrder_NilAccountsPreservedAtTail(t *testing.T) {
	now := time.Now()
	a := &Account{ID: 1, Priority: 1, LastUsedAt: &now}
	in := []*Account{nil, a, nil}
	out := sortNvidiaThrottleSchedulingOrder(in, nil)
	require.Len(t, out, 3)
	require.NotNil(t, out[0])
	require.Nil(t, out[1])
	require.Nil(t, out[2], "tail nil accounts appended to end")
}

func TestSortNvidiaThrottleSchedulingOrder_PriorityWins(t *testing.T) {
	now := time.Now()
	lo := &Account{ID: 10, Priority: 5, LastUsedAt: &now}
	hi := &Account{ID: 11, Priority: 1, LastUsedAt: &now}
	in := []*Account{lo, hi}
	out := sortNvidiaThrottleSchedulingOrder(in, nil)
	require.Equal(t, int64(11), out[0].ID, "lower priority value wins (hi)")
	require.Equal(t, int64(10), out[1].ID)
}

func TestSortNvidiaThrottleSchedulingOrder_NewerLastUsedAtWins(t *testing.T) {
	newer := time.Now()
	older := newer.Add(-time.Hour)
	a := &Account{ID: 1, Priority: 1, LastUsedAt: &older}
	b := &Account{ID: 2, Priority: 1, LastUsedAt: &newer}
	in := []*Account{a, b}
	out := sortNvidiaThrottleSchedulingOrder(in, nil)
	require.Equal(t, int64(2), out[0].ID, "newer LastUsedAt ranks first (anti-LRU)")
	require.Equal(t, int64(1), out[1].ID)
}

func TestSortNvidiaThrottleSchedulingOrder_NilLastUsedAtWorstRank(t *testing.T) {
	newer := time.Now()
	withLastUsed := &Account{ID: 1, Priority: 1, LastUsedAt: &newer}
	withoutLastUsed := &Account{ID: 2, Priority: 1, LastUsedAt: nil}
	in := []*Account{withoutLastUsed, withLastUsed}
	out := sortNvidiaThrottleSchedulingOrder(in, nil)
	require.Equal(t, int64(1), out[0].ID, "account with LastUsedAt ranks above nil")
	require.Equal(t, int64(2), out[1].ID)
}

func TestSortNvidiaThrottleSchedulingOrder_ReuseHeatWithinBucket(t *testing.T) {
	now := time.Now()
	metrics := NewNVIDIASharedConnectionPoolMetrics()
	hotAccount := &Account{ID: 1, Priority: 1, LastUsedAt: &now}
	coldAccount := &Account{ID: 2, Priority: 1, LastUsedAt: &now}
	for i := 0; i < 10; i++ {
		metrics.RecordAccountReuse(1, now)
	}
	require.Equal(t, int64(10), metrics.SnapshotAccountReuse(1).ReusedTotal)

	in := []*Account{coldAccount, hotAccount}
	out := sortNvidiaThrottleSchedulingOrder(in, metrics)
	require.Equal(t, int64(1), out[0].ID, "hot (high reuse) account ranks above cold within same (priority+LastUsedAt) bucket")
	require.Equal(t, int64(2), out[1].ID)
}

func TestSortNvidiaThrottleSchedulingOrder_FailPenaltyOverridesReuseHeat(t *testing.T) {
	now := time.Now()
	metrics := NewNVIDIASharedConnectionPoolMetrics()
	hotButBad := &Account{ID: 1, Priority: 1, LastUsedAt: &now}
	coldButGood := &Account{ID: 2, Priority: 1, LastUsedAt: &now}
	// hotButBad: 10 successful reuses but 5 recent failures → weight = 10 - 5*3 = -5
	for i := 0; i < 10; i++ {
		metrics.RecordAccountReuse(1, now)
	}
	for i := 0; i < 5; i++ {
		metrics.RecordAccountFail(1, now)
	}
	require.Equal(t, int64(-5), nvidiaReuseHeatFor(hotButBad, metrics), "weight = reusedTotal - 3*recentFailCount")
	require.Equal(t, int64(0), nvidiaReuseHeatFor(coldButGood, metrics))

	in := []*Account{hotButBad, coldButGood}
	out := sortNvidiaThrottleSchedulingOrder(in, metrics)
	require.Equal(t, int64(2), out[0].ID, "single failure (×3 penalty) must push hot-but-degraded account below cold-and-fresh")
	require.Equal(t, int64(1), out[1].ID)
}

func TestNvidiaReuseHeatFor_NilAccountOrMetricsReturnsZero(t *testing.T) {
	require.Equal(t, int64(0), nvidiaReuseHeatFor(nil, nil))
	metrics := NewNVIDIASharedConnectionPoolMetrics()
	require.Equal(t, int64(0), nvidiaReuseHeatFor(&Account{ID: 1}, nil))
	require.Equal(t, int64(0), nvidiaReuseHeatFor(nil, metrics))
}

func TestSameNvidiaPriorityAndLastUsedAtSecond(t *testing.T) {
	now := time.Now()
	aSameSec := now.Truncate(time.Second)
	bSameSec := aSameSec.Add(500 * time.Millisecond)
	require.True(t, sameNvidiaPriorityAndLastUsedAtSecond(
		&Account{ID: 1, Priority: 1, LastUsedAt: &aSameSec},
		&Account{ID: 2, Priority: 1, LastUsedAt: &bSameSec},
	), "same second bucket (truncated to s) equivalent")

	require.False(t, sameNvidiaPriorityAndLastUsedAtSecond(
		&Account{ID: 1, Priority: 1, LastUsedAt: &now},
		&Account{ID: 2, Priority: 2, LastUsedAt: &now},
	), "priority differs")

	require.False(t, sameNvidiaPriorityAndLastUsedAtSecond(
		&Account{ID: 1, Priority: 1, LastUsedAt: &now},
		&Account{ID: 2, Priority: 1},
	), "one nil LastUsedAt")
}

var _ = newPriorityLastUsed

func TestNVIDIASchedulingSortPrefersNewerPrewarmWithinSameBucket(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	now := time.Now()
	// 同 priority + LastUsedAt + heat + baseline 桶：只有 prewarm 时间不同。
	accA := &Account{ID: 1, Priority: 0, LastUsedAt: &now}
	accB := &Account{ID: 2, Priority: 0, LastUsedAt: &now}
	// 同样的 prewarm 计数（success=1），但 B 后预热（时间更新）。
	m.RecordInferencePrewarm(1, true)
	time.Sleep(2 * time.Millisecond)
	m.RecordInferencePrewarm(2, true)

	require.Greater(t, nvidiaPrewarmScoreFor(accB, m), nvidiaPrewarmScoreFor(accA, m), "premise: B has newer prewarm")

	sorted := sortNvidiaThrottleSchedulingOrder([]*Account{accA, accB}, m)
	require.Equal(t, int64(2), sorted[0].ID, "同 heat+baseline 桶内 prewarm 新者优先")
}

func TestNvidiaSchedulingSort_PrewarmTiebreakSkipsFailHeavyAccounts(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	now := time.Now()

	accA := &Account{ID: 1, Priority: 0, LastUsedAt: &now}
	accB := &Account{ID: 2, Priority: 0, LastUsedAt: &now}
	// A 有成功预热，但随后 3 次传输层失败（>2）：prewarm 不再计分。
	m.RecordInferencePrewarm(1, true)
	m.RecordInferencePrewarm(1, false)
	m.RecordInferencePrewarm(1, false)
	m.RecordInferencePrewarm(1, false)
	// B 从未预热。
	require.Equal(t, int64(0), nvidiaPrewarmScoreFor(accA, m), "failTotal>2 视为无 prewarm")
	require.Equal(t, int64(0), nvidiaPrewarmScoreFor(accB, m))
	require.True(t, nvidiaPrewarmInSameBucket(accA, accB, m), "同为无 score 应同桶")
}

func TestNvidiaSchedulingSort_PrewarmNeverCrossesBaselineBucket(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	now := time.Now()

	// 两个账号 heat=0、同 LastUsedAt，但 baseline 不同（10 vs 0）。
	accLow := &Account{ID: 1, Priority: 0, LastUsedAt: &now}
	accHigh := &Account{ID: 2, Priority: 0, LastUsedAt: &now}
	for i := 0; i < 10; i++ {
		m.RecordAccountSuccess(2, now.Add(time.Duration(i)*time.Second))
	}
	// 给 accLow（ID=1）更新鲜的 prewarm，但 baseline 更低——必须仍排后。
	time.Sleep(1 * time.Millisecond)
	m.RecordInferencePrewarm(1, true)

	sorted := sortNvidiaThrottleSchedulingOrder([]*Account{accLow, accHigh}, m)
	require.Equal(t, int64(2), sorted[0].ID, "prewarm 不得跨 baseline 桶（baseline 高的优先）")
}

func TestNvidiaSameNvidiaSortBucket_IncludesPrewarmDimension(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	now := time.Now()
	accA := &Account{ID: 1, Priority: 0, LastUsedAt: &now}
	accB := &Account{ID: 2, Priority: 0, LastUsedAt: &now}

	require.True(t, sameNvidiaSortBucket(accA, accB, m), "无 prewarm 记录应同桶")

	m.RecordInferencePrewarm(1, true)
	time.Sleep(2 * time.Millisecond)
	m.RecordInferencePrewarm(2, true)
	require.False(t, sameNvidiaSortBucket(accA, accB, m),
		"prewarm 时间不同不得同 shuffle 桶，否则 shuffle 洗掉 prewarm 排序成果")
}

func TestNvidiaSchedulingSort_PrewarmTiebreakDoesNotDisturbHeatOrder(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	now := time.Now()

	// Same priority+LastUsedAt, different reuse heat → heat 必须仍然优先于 prewarm。
	accHeatHot := &Account{ID: 1, Priority: 0, LastUsedAt: &now}
	accHeatCold := &Account{ID: 2, Priority: 0, LastUsedAt: &now}
	for i := 0; i < 10; i++ {
		m.RecordAccountReuse(1, now)
	}
	require.Greater(t, nvidiaReuseHeatFor(accHeatHot, m), nvidiaReuseHeatFor(accHeatCold, m))
	m.RecordInferencePrewarm(2, true)
	m.RecordInferencePrewarm(2, true)
	m.RecordInferencePrewarm(2, true)
	require.Greater(t, nvidiaPrewarmScoreFor(accHeatCold, m), nvidiaPrewarmScoreFor(accHeatHot, m),
		"premise: cold-heat account has much newer prewar")

	sorted := sortNvidiaThrottleSchedulingOrder([]*Account{accHeatCold, accHeatHot}, m)
	require.Equal(t, int64(1), sorted[0].ID, "heat 排序语义不被 prewarm 破坏（不同 heat 不同桶）")
}
