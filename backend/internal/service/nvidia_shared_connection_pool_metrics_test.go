//go:build unit

package service

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNVIDIASharedConnectionPoolMetrics_RecordConnectionReuse(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	m.RecordConnectionReuse(true)
	m.RecordConnectionReuse(true)
	m.RecordConnectionReuse(false)
	snap := m.Snapshot()
	require.Equal(t, int64(2), snap.Reuse.ReusedTotal)
	require.Equal(t, int64(1), snap.Reuse.NotReusedTotal)
}

func TestNVIDIASharedConnectionPoolMetrics_NilSafe(t *testing.T) {
	var m *NVIDIASharedConnectionPoolMetrics
	require.NotPanics(t, func() {
		m.RecordConnectionReuse(true)
		m.RecordAccountReuse(1, time.Now())
		m.RecordAccountFail(1, time.Now())
		m.RecordConsecutive5xx(1, time.Now(), 3, time.Minute)
		_ = m.Snapshot()
		_ = m.SnapshotAccountReuse(1)
		_ = m.RecordAccountSwitchToRequestWritten
	})
}

func TestRecordAccountReuse_CountsAndTimestamp(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	at1 := time.Now().Add(-time.Second)
	at2 := time.Now()
	m.RecordAccountReuse(7, at1)
	m.RecordAccountReuse(7, at2)
	// Older timestamp must not overwrite the newer reuse time CAS.
	m.RecordAccountReuse(7, at1.Add(-2*time.Second))
	snap := m.SnapshotAccountReuse(7)
	require.Equal(t, int64(3), snap.ReusedTotal, "reused_total always increments; only timestamp is CAS-protected")
	require.Equal(t, at2.UnixNano(), snap.LastReuseAtUnixNano, "older reuse event must not regress lastReuseAt")
}

func TestRecordAccountFail_IncrementsRecentFailCountAndResetsDecayStreak(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	// Build up consecutive successes to near the decay threshold (5).
	for i := 0; i < 4; i++ {
		m.RecordAccountReuse(11, time.Now())
	}
	// A failure must reset consecutiveReuseSuccess so that one-off follow-up success
	// cannot immediately clear recentFailCount.
	m.RecordAccountFail(11, time.Now())
	require.Equal(t, int64(1), m.SnapshotAccountReuse(11).RecentFailCount)
	m.RecordAccountReuse(11, time.Now())
	// One success after a failure does NOT clear the failure counter (need 5 consecutive).
	require.Equal(t, int64(1), m.SnapshotAccountReuse(11).RecentFailCount,
		"failure must reset decay streak so consecutiveReuseSuccess starts over")
}

func TestRecordAccountReuse_Consecutive5ClearsRecentFailCount(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	m.RecordAccountFail(12, time.Now())
	m.RecordAccountFail(12, time.Now())
	require.Equal(t, int64(2), m.SnapshotAccountReuse(12).RecentFailCount)
	for i := 0; i < nvidiaReuseSuccessDecayThreshold; i++ {
		m.RecordAccountReuse(12, time.Now())
	}
	require.Equal(t, int64(0), m.SnapshotAccountReuse(12).RecentFailCount,
		"%d consecutive successes must decay recentFailCount to 0", nvidiaReuseSuccessDecayThreshold)
	m.RecordAccountReuse(12, time.Now())
	require.Equal(t, int64(0), m.SnapshotAccountReuse(12).RecentFailCount,
		"decay does not over-trigger / does not go negative")
}

func TestRecordAccountReuse_Consecutive5xxCountClearedOnSuccess(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	_ = m.RecordConsecutive5xx(13, time.Now(), 3, time.Minute)
	_ = m.RecordConsecutive5xx(13, time.Now(), 3, time.Minute)
	require.NotPanics(t, func() {
		m.RecordAccountReuse(13, time.Now())
	})
}

func TestRecordConsecutive5xx_WindowAndReset(t *testing.T) {
	t.Run("below_threshold_returns_false", func(t *testing.T) {
		m := NewNVIDIASharedConnectionPoolMetrics()
		require.False(t, m.RecordConsecutive5xx(21, time.Now(), 3, time.Minute))
		require.False(t, m.RecordConsecutive5xx(21, time.Now(), 3, time.Minute))
	})
	t.Run("within_window_reaches_threshold", func(t *testing.T) {
		m := NewNVIDIASharedConnectionPoolMetrics()
		now := time.Now()
		require.False(t, m.RecordConsecutive5xx(22, now, 3, time.Minute))
		require.False(t, m.RecordConsecutive5xx(22, now.Add(10*time.Second), 3, time.Minute))
		require.True(t, m.RecordConsecutive5xx(22, now.Add(20*time.Second), 3, time.Minute),
			"third 5xx inside the window must reach threshold")
	})
	t.Run("after_reaching_threshold_streak_resets", func(t *testing.T) {
		m := NewNVIDIASharedConnectionPoolMetrics()
		now := time.Now()
		m.RecordConsecutive5xx(23, now, 3, time.Minute)
		m.RecordConsecutive5xx(23, now.Add(1*time.Second), 3, time.Minute)
		m.RecordConsecutive5xx(23, now.Add(2*time.Second), 3, time.Minute)
		// After the streak reset, the next 5xx starts a new streak.
		require.False(t, m.RecordConsecutive5xx(23, now.Add(3*time.Second), 3, time.Minute),
			"post-reach streak reset must start accumulating again")
	})
	t.Run("outsideWindow_returns_false_and_resets_streak", func(t *testing.T) {
		m := NewNVIDIASharedConnectionPoolMetrics()
		base := time.Unix(0, 0)
		m.RecordConsecutive5xx(24, base, 3, time.Minute)
		m.RecordConsecutive5xx(24, base.Add(1*time.Second), 3, time.Minute)
		// Third call far outside the 1-minute window → false + reset.
		require.False(t, m.RecordConsecutive5xx(24, base.Add(2*time.Hour), 3, time.Minute))
	})
}

func TestRecordConsecutive5xx_NilSafe(t *testing.T) {
	var m *NVIDIASharedConnectionPoolMetrics
	require.NotPanics(t, func() {
		require.False(t, m.RecordConsecutive5xx(1, time.Now(), 3, time.Minute))
	})
}

func TestRecordConsecutive5xx_RejectsBadArgs(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	require.False(t, m.RecordConsecutive5xx(0, time.Now(), 3, time.Minute), "accountID=0 rejected")
	require.False(t, m.RecordConsecutive5xx(31, time.Now(), 0, time.Minute), "threshold=0 rejected")
	require.False(t, m.RecordConsecutive5xx(31, time.Now(), 3, 0), "window=0 rejected")
}

func TestNVIDIASharedConnectionPoolMetrics_ConcurrentAccountReuse(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	var wg sync.WaitGroup
	const goroutines = 50
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.RecordAccountReuse(42, time.Now())
		}()
	}
	wg.Wait()
	require.Equal(t, int64(goroutines), m.SnapshotAccountReuse(42).ReusedTotal)
}

func TestNVIDIASharedConnectionPoolMetrics_SetMaxConnsPerHost(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	require.NotPanics(t, func() { m.SetMaxConnsPerHost(0) }, "0 is per http.sig a valid no-op delay")
	m.SetMaxConnsPerHost(240)
	snap := m.Snapshot()
	require.Equal(t, 240, snap.MaxConnsPerHost)
	m.SetMaxConnsPerHost(-1) // negative rejected
	require.Equal(t, 240, m.Snapshot().MaxConnsPerHost, "negative must not overwrite")
}

func TestRecordInferencePrewarm_SuccessCountsAndTimestamp(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	m.RecordInferencePrewarm(7, true)
	time.Sleep(time.Millisecond)
	last1, success, fail := m.SnapshotPrewarm(7)
	require.Equal(t, int64(1), success, "success_total must increment")
	require.Equal(t, int64(0), fail, "success must not touch fail_total")
	require.Greater(t, last1, int64(0), "success must write lastPrewarmAt")

	m.RecordInferencePrewarm(7, true)
	last2, success2, fail2 := m.SnapshotPrewarm(7)
	require.Equal(t, int64(2), success2)
	require.Equal(t, int64(0), fail2)
	require.GreaterOrEqual(t, last2, last1, "newer prewarm must not regress lastPrewarmAt")
}

func TestRecordInferencePrewarm_FailureOnlyIncrementsFailTotal(t *testing.T) {
	m := NewNVIDIASharedConnectionPoolMetrics()
	m.RecordInferencePrewarm(8, false)
	lastAt, success, fail := m.SnapshotPrewarm(8)
	require.Equal(t, int64(0), success, "failure must not touch success_total")
	require.Equal(t, int64(1), fail)
	require.Equal(t, int64(0), lastAt, "failure must not write lastPrewarmAt")

	m.RecordInferencePrewarm(8, true)
	lastAt, success, fail = m.SnapshotPrewarm(8)
	require.Equal(t, int64(1), success)
	require.Equal(t, int64(1), fail)
	require.Greater(t, lastAt, int64(0))
}

func TestRecordInferencePrewarm_NilSafeAndBadArgs(t *testing.T) {
	var m *NVIDIASharedConnectionPoolMetrics
	require.NotPanics(t, func() {
		m.RecordInferencePrewarm(1, true)
		m.RecordInferencePrewarm(1, false)
	})
	m = NewNVIDIASharedConnectionPoolMetrics()
	m.RecordInferencePrewarm(0, true)
	_, success, fail := m.SnapshotPrewarm(0)
	require.Equal(t, int64(0), success, "accountID=0 rejected")
	require.Equal(t, int64(0), fail, "accountID=0 rejected")
}

var _ = NewNVIDIASharedConnectionPoolMetrics
