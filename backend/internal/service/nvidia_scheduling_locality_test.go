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

