package service

import "sync/atomic"

// NVIDIAAdaptiveThrottleMetrics stores process-local adaptive-throttle counters.
type NVIDIAAdaptiveThrottleMetrics struct {
	Reserves    atomic.Int64
	Allowed     atomic.Int64
	Blocked     atomic.Int64
	Outcomes    atomic.Int64
	CacheErrors atomic.Int64
}

// NVIDIAAdaptiveThrottleMetricsSnapshot is a point-in-time metrics view.
type NVIDIAAdaptiveThrottleMetricsSnapshot struct {
	Reserves    int64
	Allowed     int64
	Blocked     int64
	Outcomes    int64
	CacheErrors int64
}

// SnapshotNVIDIAAdaptiveThrottleMetrics returns the current process-local counters.
func (s *RateLimitService) SnapshotNVIDIAAdaptiveThrottleMetrics() NVIDIAAdaptiveThrottleMetricsSnapshot {
	if s == nil || s.nvidiaMetrics == nil {
		return NVIDIAAdaptiveThrottleMetricsSnapshot{}
	}
	return NVIDIAAdaptiveThrottleMetricsSnapshot{
		Reserves:    s.nvidiaMetrics.Reserves.Load(),
		Allowed:     s.nvidiaMetrics.Allowed.Load(),
		Blocked:     s.nvidiaMetrics.Blocked.Load(),
		Outcomes:    s.nvidiaMetrics.Outcomes.Load(),
		CacheErrors: s.nvidiaMetrics.CacheErrors.Load(),
	}
}
