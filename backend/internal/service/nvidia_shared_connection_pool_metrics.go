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
	accountReuseMu sync.Mutex
	accountReuseBy map[int64]*nvidiaAccountReuseState

	// maxConnsPerHost: configured ceiling on per-host connections in the NVIDIA
	// shared pool. Atomic to allow concurrent reads from Snapshot().
	maxConnsPerHost atomic.Int32
}

// nvidiaAccountReuseState holds per-account reuse bookkeeping for the scheduler.
// Fields are atomic so the scheduler hot-path (SnapshotAccountReuse) can read
// ReusedTotal without locking; only map access is mutex-protected.
// recentFailCount tracks recent upstream failures (5xx/429/exhausted) to
// down-weight accounts with degraded reliability in the sorting heat.
// consecutiveReuseSuccess 反映连续成功调用次数，每达到 nvidiaReuseSuccessDecayThreshold
// 后重置 recentFailCount=0，实现"成功冲抵失败"的衰减机制 (F4)。
//
// P1 连续 5xx 自动冷却：consecutive5xxCount 跟踪"连续 NVIDIA 5xx 失败次数"，
// first5xxAtNano 记录窗口内第一次 5xx 的时间戳。RecordConsecutive5xxReached
// 在累计数超过 threshold 且首末失败间隔仍在 window 内时返回 true——上游调用方
// 接住信号触发 BlockAccountScheduling。一次成功 RecordAccountReuse 会清零
// consecutive5xxCount + first5xxAtNano，让账号恢复后立即重新轮换。
type nvidiaAccountReuseState struct {
	reusedTotal              atomic.Int64
	lastReuseAtNano          atomic.Int64
	recentFailCount          atomic.Int64
	consecutiveReuseSuccess  atomic.Int64
	// F9: 记录最近一次失败时间(unix-nano)，与 lastReuseAtNano 对称。
	// 当前仅写入不读取，为后续可能引入"基于时间窗的衰减"留钩子；N3 当前
	// 仍靠 consecutiveReuseSuccess 驱动衰减以保持 hot-path 无锁。
	lastFailAtNano           atomic.Int64
	// P1: 连续 5xx 计数与窗口起点（unix-nano），用于触发短期 BlockAccountScheduling 冷却。
	consecutive5xxCount      atomic.Int64
	first5xxAtNano           atomic.Int64
}

// nvidiaReuseSuccessDecayThreshold 是连续成功 reuse 次数阈值，达到后清除
// recentFailCount，避免账号一次事故被永久压在 sort 末尾的"黑洞"问题 (F4)。
// 选 5 次为平衡点：足够小让恢复快，足够大避免单次偶发成功就清零惩罚。
const nvidiaReuseSuccessDecayThreshold = 5

// NVIDIAAccountReuseSnapshot is the read-only view of an account's reuse events.
type NVIDIAAccountReuseSnapshot struct {
	ReusedTotal         int64 `json:"reused_total"`
	LastReuseAtUnixNano int64 `json:"last_reuse_at_unix_nano"`
	RecentFailCount     int64 `json:"recent_fail_count"`
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
	MaxConnsPerHost               int                                   `json:"nvidia_max_conns_per_host"`
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
// whose upstream HTTP/2 connections are already warm.
// Lock only for map access; per-account fields are atomic for lock-free reads
// in the scheduler hot-path (SnapshotAccountReuse).
func (m *NVIDIASharedConnectionPoolMetrics) RecordAccountReuse(accountID int64, at time.Time) {
	if m == nil || accountID <= 0 {
		return
	}
	m.accountReuseMu.Lock()
	if m.accountReuseBy == nil {
		m.accountReuseBy = make(map[int64]*nvidiaAccountReuseState)
	}
	state, ok := m.accountReuseBy[accountID]
	if !ok {
		state = &nvidiaAccountReuseState{}
		m.accountReuseBy[accountID] = state
	}
	m.accountReuseMu.Unlock()

	state.reusedTotal.Add(1)
	// F4: 连续成功 reuse 累积到 decayThreshold 后清零 recentFailCount，
	// 避免账号一次事故后长期被压在 sort 末尾。每次 reuse 成功 +1，达到阈值
	// 后重置 (consecutiveReuseSuccess 归零 + recentFailCount 归零)，让账号
	// 在 NVIDIA 容量恢复后能重新参与热排序。
	// P1: 同时清零连续 5xx 计数 + 窗口起点。成功即代表账号当前可服务，
	// 之前的"集中 5xx 爆发"周期结束；下一次累计从 0 开始。
	if state.consecutive5xxCount.Load() > 0 {
		state.consecutive5xxCount.Store(0)
		state.first5xxAtNano.Store(0)
	}
	if state.consecutiveReuseSuccess.Add(1) >= nvidiaReuseSuccessDecayThreshold {
		state.recentFailCount.Store(0)
		state.consecutiveReuseSuccess.Store(0)
	}
	ts := at.UnixNano()
	for {
		old := state.lastReuseAtNano.Load()
		if ts <= old {
			break
		}
		if state.lastReuseAtNano.CompareAndSwap(old, ts) {
			break
		}
	}
}

// RecordAccountFail increments the per-account recent failure counter and stamps
// the latest failure timestamp. The scheduler (nvidiaReuseHeatFor) down-weights
// hot accounts whose recent upstream calls have failed to avoid hammering
// degraded accounts. at is kept for ABI symmetry with RecordAccountReuse; the
// current scheduler reads recentFailCount + consecutiveReuseSuccess only, but
// at is recorded as a forward hook for time-windowed decay without requiring
// another signature change.
func (m *NVIDIASharedConnectionPoolMetrics) RecordAccountFail(accountID int64, at time.Time) {
	if m == nil || accountID <= 0 {
		return
	}
	m.accountReuseMu.Lock()
	if m.accountReuseBy == nil {
		m.accountReuseBy = make(map[int64]*nvidiaAccountReuseState)
	}
	state, ok := m.accountReuseBy[accountID]
	if !ok {
		state = &nvidiaAccountReuseState{}
		m.accountReuseBy[accountID] = state
	}
	m.accountReuseMu.Unlock()

	state.recentFailCount.Add(1)
	state.consecutiveReuseSuccess.Store(0)
	// F4: 失败打断连续成功链，让衰减阈值重新计算，避免短暂偶发成功就清零大量失败计数
	casLastFailAtNano(state, at.UnixNano())
}

// RecordConsecutive5xx records a single NVIDIA 5xx failure as part of a
// consecutive streak and reports whether the streak plus time-window now
// qualifies the account for a short scheduling cooldown.
//
// Algorithm:
//  1. Bump consecutive5xxCount.
//  2. If this is the first failure (count == 1), stamp first5xxAtNano = now.
//  3. When count >= threshold, check window: if (now - first5xxAtNano) <= window,
//     return reached=true and reset the streak (caller must call again after the
//     cooldown ends, so we don't keep re-triggering). Otherwise reset the streak
//     (stale burst) and return false.
//  4. Otherwise (count < threshold) return false (still accumulating).
//
// The check and reset are atomic as a single caller (recordNVIDIAUpstreamFailure)
// holds the per-failure verbose path; concurrent 5xx from different goroutines on
// the same account are rare but possible—they will simply race on the count and
// one of them will win the threshold race, the others may cause a redundant
// reset, which is harmless (the next 5xx starts a fresh streak).
//
// at is the canonical failure time; pass time.Now() at the call site.
func (m *NVIDIASharedConnectionPoolMetrics) RecordConsecutive5xx(accountID int64, at time.Time, threshold int, window time.Duration) (reached bool) {
	if m == nil || accountID <= 0 || threshold <= 0 || window <= 0 {
		return false
	}
	m.accountReuseMu.Lock()
	if m.accountReuseBy == nil {
		m.accountReuseBy = make(map[int64]*nvidiaAccountReuseState)
	}
	state, ok := m.accountReuseBy[accountID]
	if !ok {
		state = &nvidiaAccountReuseState{}
		m.accountReuseBy[accountID] = state
	}
	m.accountReuseMu.Unlock()

	nowNano := at.UnixNano()
	count := state.consecutive5xxCount.Add(1)
	if count == 1 {
		state.first5xxAtNano.Store(nowNano)
		return false
	}
	if count < int64(threshold) {
		return false
	}
	// count >= threshold: evaluate window
	firstNano := state.first5xxAtNano.Load()
	elapsed := at.Sub(time.Unix(0, firstNano))
	// Reset the streak regardless of result so we don't re-fire on every subsequent 5xx.
	state.consecutive5xxCount.Store(0)
	state.first5xxAtNano.Store(0)
	return elapsed <= window
}

func casLastFailAtNano(state *nvidiaAccountReuseState, ts int64) {
	for {
		old := state.lastFailAtNano.Load()
		if ts <= old {
			break
		}
		if state.lastFailAtNano.CompareAndSwap(old, ts) {
			break
		}
	}
}

// SnapshotAccountReuse returns a point-in-time view of an account's reuse state.
// Returns zero value if accountID was never recorded.
// Lock only for map lookup; per-account fields are read atomically so the
// scheduler hot-path (sort tiebreak) never blocks on this mutex.
func (m *NVIDIASharedConnectionPoolMetrics) SnapshotAccountReuse(accountID int64) NVIDIAAccountReuseSnapshot {
	if m == nil || accountID <= 0 {
		return NVIDIAAccountReuseSnapshot{}
	}
	m.accountReuseMu.Lock()
	state, ok := m.accountReuseBy[accountID]
	m.accountReuseMu.Unlock()
	if !ok || state == nil {
		return NVIDIAAccountReuseSnapshot{}
	}
	return NVIDIAAccountReuseSnapshot{
		ReusedTotal:         state.reusedTotal.Load(),
		LastReuseAtUnixNano: state.lastReuseAtNano.Load(),
		RecentFailCount:     state.recentFailCount.Load(),
	}
}

// SetMaxConnsPerHost records the per-host connection ceiling exposed via Snapshot().
func (m *NVIDIASharedConnectionPoolMetrics) SetMaxConnsPerHost(n int) {
	if m == nil || n < 0 {
		return
	}
	m.maxConnsPerHost.Store(int32(n))
}

func (m *NVIDIASharedConnectionPoolMetrics) Snapshot() NVIDIASharedConnectionPoolMetricsSnapshot {
	if m == nil {
		return NVIDIASharedConnectionPoolMetricsSnapshot{}
	}
	return NVIDIASharedConnectionPoolMetricsSnapshot{
		Reuse:                         NVIDIASharedConnectionReuseSnapshot{ReusedTotal: m.reusedTotal.Load(), NotReusedTotal: m.notReusedTotal.Load()},
		AccountSwitchToRequestWritten: m.accountSwitchToRequestWritten.snapshot(),
		DNS:                           m.dns.snapshot(), Connect: m.connect.snapshot(), TLSHandshake: m.tlsHandshake.snapshot(), TTFB: m.ttfb.snapshot(),
		MaxConnsPerHost:               int(m.maxConnsPerHost.Load()),
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
