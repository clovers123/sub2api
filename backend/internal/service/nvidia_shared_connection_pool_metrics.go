package service

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const nvidiaSharedConnectionLatencySampleCapacity = 2048

type NVIDIASharedConnectionPoolMetrics struct {
	reusedTotal                   atomic.Int64
	notReusedTotal                atomic.Int64
	accountSwitchToRequestWritten nvidiaSharedConnectionLatencyWindow
	dns                           nvidiaSharedConnectionLatencyWindow
	connect                       nvidiaSharedConnectionLatencyWindow
	tlsHandshake                  nvidiaSharedConnectionLatencyWindow
	ttfb                          nvidiaSharedConnectionLatencyWindow

	// accountReuse tracks per-account reuse events so schedulers can prefer
	// accounts whose upstream HTTP/2 connections are already warm.
	accountReuseMu       sync.Mutex
	accountReuseBy       map[int64]*nvidiaAccountReuseState
}

// nvidiaAccountReuseState holds per-account reuse bookkeeping for the scheduler.
// Guarded by accountReuseMu on the parent metrics instance.
type nvidiaAccountReuseState struct {
	reusedTotal       int64
	lastReuseAtNano   int64
}

// NVIDIAAccountReuseSnapshot is the read-only view of an account's reuse events.
type NVIDIAAccountReuseSnapshot struct {
	ReusedTotal         int64 `json:"reused_total"`
	LastReuseAtUnixNano int64 `json:"last_reuse_at_unix_nano"`
}

type nvidiaSharedConnectionLatencyWindow struct {
	sampleCount atomic.Int64
	maxMS       atomic.Int64
	mu          sync.Mutex
	samples     []int64
	next        int
}

type NVIDIASharedConnectionPoolMetricsSnapshot struct {
	Reuse                         NVIDIASharedConnectionReuseSnapshot   `json:"nvidia_shared_connection_reuse_total"`
	AccountSwitchToRequestWritten NVIDIASharedConnectionLatencySnapshot `json:"nvidia_account_switch_to_request_written_ms"`
	DNS                           NVIDIASharedConnectionLatencySnapshot `json:"nvidia_dns_ms"`
	Connect                       NVIDIASharedConnectionLatencySnapshot `json:"nvidia_connect_ms"`
	TLSHandshake                  NVIDIASharedConnectionLatencySnapshot `json:"nvidia_tls_handshake_ms"`
	TTFB                          NVIDIASharedConnectionLatencySnapshot `json:"nvidia_ttfb_ms"`
}

type NVIDIASharedConnectionReuseSnapshot struct {
	ReusedTotal    int64 `json:"reused_total"`
	NotReusedTotal int64 `json:"not_reused_total"`
}

type NVIDIASharedConnectionLatencySnapshot struct {
	SampleCount         int64 `json:"sample_count"`
	RetainedSampleCount int   `json:"retained_sample_count"`
	P50MS               int64 `json:"p50_ms"`
	P95MS               int64 `json:"p95_ms"`
	P99MS               int64 `json:"p99_ms"`
	MaxMS               int64 `json:"max_ms"`
}

func NewNVIDIASharedConnectionPoolMetrics() *NVIDIASharedConnectionPoolMetrics {
	return &NVIDIASharedConnectionPoolMetrics{}
}

func (m *NVIDIASharedConnectionPoolMetrics) RecordConnectionReuse(reused bool) {
	if m == nil {
		return
	}
	if reused {
		m.reusedTotal.Add(1)
		return
	}
	m.notReusedTotal.Add(1)
}

func (m *NVIDIASharedConnectionPoolMetrics) RecordAccountSwitchToRequestWritten(duration time.Duration) {
	if m != nil {
		m.accountSwitchToRequestWritten.record(duration)
	}
}
func (m *NVIDIASharedConnectionPoolMetrics) RecordDNS(duration time.Duration) {
	if m != nil {
		m.dns.record(duration)
	}
}
func (m *NVIDIASharedConnectionPoolMetrics) RecordConnect(duration time.Duration) {
	if m != nil {
		m.connect.record(duration)
	}
}
func (m *NVIDIASharedConnectionPoolMetrics) RecordTLSHandshake(duration time.Duration) {
	if m != nil {
		m.tlsHandshake.record(duration)
	}
}
func (m *NVIDIASharedConnectionPoolMetrics) RecordTTFB(duration time.Duration) {
	if m != nil {
		m.ttfb.record(duration)
	}
}

// RecordAccountReuse increments the per-account reuse counter for accountID
// and updates the latest reuse timestamp so schedulers can prefer accounts
// whose upstream HTTP/2 connection was recently reused (warmed).
func (m *NVIDIASharedConnectionPoolMetrics) RecordAccountReuse(accountID int64, at time.Time) {
	if m == nil || accountID <= 0 {
		return
	}
	m.accountReuseMu.Lock()
	defer m.accountReuseMu.Unlock()
	if m.accountReuseBy == nil {
		m.accountReuseBy = make(map[int64]*nvidiaAccountReuseState)
	}
	state, ok := m.accountReuseBy[accountID]
	if !ok {
		state = &nvidiaAccountReuseState{}
		m.accountReuseBy[accountID] = state
	}
	state.reusedTotal++
	ts := at.UnixNano()
	if ts > state.lastReuseAtNano {
		state.lastReuseAtNano = ts
	}
}

// SnapshotAccountReuse returns a point-in-time view of an account's reuse state.
// Returns zero value if accountID was never recorded.
func (m *NVIDIASharedConnectionPoolMetrics) SnapshotAccountReuse(accountID int64) NVIDIAAccountReuseSnapshot {
	if m == nil || accountID <= 0 {
		return NVIDIAAccountReuseSnapshot{}
	}
	m.accountReuseMu.Lock()
	defer m.accountReuseMu.Unlock()
	state, ok := m.accountReuseBy[accountID]
	if !ok || state == nil {
		return NVIDIAAccountReuseSnapshot{}
	}
	return NVIDIAAccountReuseSnapshot{
		ReusedTotal:         state.reusedTotal,
		LastReuseAtUnixNano: state.lastReuseAtNano,
	}
}

func (m *NVIDIASharedConnectionPoolMetrics) Snapshot() NVIDIASharedConnectionPoolMetricsSnapshot {
	if m == nil {
		return NVIDIASharedConnectionPoolMetricsSnapshot{}
	}
	return NVIDIASharedConnectionPoolMetricsSnapshot{
		Reuse:                         NVIDIASharedConnectionReuseSnapshot{ReusedTotal: m.reusedTotal.Load(), NotReusedTotal: m.notReusedTotal.Load()},
		AccountSwitchToRequestWritten: m.accountSwitchToRequestWritten.snapshot(),
		DNS:                           m.dns.snapshot(), Connect: m.connect.snapshot(), TLSHandshake: m.tlsHandshake.snapshot(), TTFB: m.ttfb.snapshot(),
	}
}

func (w *nvidiaSharedConnectionLatencyWindow) record(duration time.Duration) {
	milliseconds := duration.Milliseconds()
	if milliseconds < 0 {
		milliseconds = 0
	}
	w.mu.Lock()
	w.sampleCount.Add(1)
	if milliseconds > w.maxMS.Load() {
		w.maxMS.Store(milliseconds)
	}
	if len(w.samples) < nvidiaSharedConnectionLatencySampleCapacity {
		w.samples = append(w.samples, milliseconds)
	} else {
		w.samples[w.next] = milliseconds
		w.next = (w.next + 1) % nvidiaSharedConnectionLatencySampleCapacity
	}
	w.mu.Unlock()
}

func (w *nvidiaSharedConnectionLatencyWindow) snapshot() NVIDIASharedConnectionLatencySnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := NVIDIASharedConnectionLatencySnapshot{SampleCount: w.sampleCount.Load(), MaxMS: w.maxMS.Load()}
	samples := append([]int64(nil), w.samples...)
	result.RetainedSampleCount = len(samples)
	if len(samples) == 0 {
		return result
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	result.P50MS = nvidiaSharedConnectionPercentile(samples, 0.50)
	result.P95MS = nvidiaSharedConnectionPercentile(samples, 0.95)
	result.P99MS = nvidiaSharedConnectionPercentile(samples, 0.99)
	return result
}

func nvidiaSharedConnectionPercentile(sorted []int64, quantile float64) int64 {
	index := int(float64(len(sorted)-1) * quantile)
	return sorted[index]
}
