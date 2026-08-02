package service

import (
	"context"
	"math/rand"
	"sort"
	"time"
)

// nvidiaSchedulingLocalityCtxKey is the request-scoped marker telling
// tryAcquireByLegacyOrder that the candidate set came from the NVIDIA
// adaptive-throttle path. Use a context key so the locality behavior is
// thread-safe without new GatewayService fields / globals.
type nvidiaSchedulingLocalityCtxKey struct{}

// WithNVIDIASchedulingLocality marks a scheduling call as NVIDIA-throttled so
// sortNvidiaThrottleSchedulingOrder takes over inside tryAcquireByLegacyOrder.
// Returns the parent context unchanged.
func WithNVIDIASchedulingLocality(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, nvidiaSchedulingLocalityCtxKey{}, true)
}

// nvidiaThrottleLocalityEnabled reports whether the scheduling call entered
// the NVIDIA adaptive-throttle path. Returns false if ctx is nil.
func nvidiaThrottleLocalityEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, ok := ctx.Value(nvidiaSchedulingLocalityCtxKey{}).(bool)
	return ok && v
}

// sortNvidiaThrottleSchedulingOrder ranks NVIDIA scheduling candidates by the
// scheduling-locality tiebreak specific to NVIDIA's prefix-based KV cache:
//
//  1. Priority (lower = higher)
//  2. LoadRate (lower = higher) — passed via secondaryClassification; nil here means
//     "skip load signal, prioritize LastUsedAt recursion"
//  3. LastUsedAt (newer = higher) — opposite of LRU: keeps cache-warm accounts first
//  4. (same LastUsedAt second) — prefer account with positive connection reuse heat
//  5. (everything tie-bounded) — shuffle randomly inside the same (priority + LastUsedAt) bucket
//
// This function does NOT mutate schedule candidates. Caller passes metrics so
// connection reuse heat can be queried without leaking repository internals for
// idle/inFlight state.
//
// Order is intentionally separate from OpenAI OAuth / Grok legacy sort path,
// which keep the global LRU fairness invariant. This is the only path where
// NVIDIA upstream's prefix-based KV cache policy rewards scheduling locality
// above round-robin fairness.
func sortNvidiaThrottleSchedulingOrder(candidates []*Account, metrics *NVIDIASharedConnectionPoolMetrics) []*Account {
	if len(candidates) == 0 {
		return candidates
	}

	out := append([]*Account(nil), candidates...)

	// Filter out nil accounts to keep sort inputs clean.
	live := make([]*Account, 0, len(out))
	for _, a := range out {
		if a != nil {
			live = append(live, a)
		}
	}
	if len(live) == 0 {
		return out
	}

	sort.SliceStable(live, func(i, j int) bool {
		a, b := live[i], live[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		// The LastUsedAt tiebreak is reversed: newer wins.
		switch {
		case a.LastUsedAt == nil && b.LastUsedAt != nil:
			return false
		case a.LastUsedAt != nil && b.LastUsedAt == nil:
			return true
		case a.LastUsedAt == nil && b.LastUsedAt == nil:
			// fall through to reuse heat / shuffle below
			return false
		default:
			return a.LastUsedAt.After(*b.LastUsedAt)
		}
	})

	// If we have reuse heat metrics, stable-sort by reuse heat inside same
	// (priority + LastUsedAt) bucket. Pure Go stdlib stable sort keeps the
	// LastUsedAt order for any entry of equal heat value.
	if metrics != nil {
		// Two-pass sweep: identify buckets by (priority + LastUsedAt second)
		// then stably pivot by reuse heat inside the bucket.
		bucketStart := 0
		for bucketStart < len(live) {
			bucketEnd := bucketStart + 1
			for bucketEnd < len(live) && sameNvidiaPriorityAndLastUsedAtSecond(live[bucketStart], live[bucketEnd]) {
				bucketEnd++
			}
			if bucketEnd-bucketStart > 1 {
				sort.SliceStable(live[bucketStart:bucketEnd], func(i, j int) bool {
					a, b := live[bucketStart+i], live[bucketStart+j]
					return nvidiaReuseHeatFor(a, metrics) > nvidiaReuseHeatFor(b, metrics)
				})
			}
			bucketStart = bucketEnd
		}

		// C: 同 heat 桶内按 RPM 基线降序（容量大的账号优先）。
		// 仅对 reuse-heat 相同的账号生效，不影响既有 heat 语义。
		bucketStart = 0
		for bucketStart < len(live) {
			bucketEnd := bucketStart + 1
			for bucketEnd < len(live) && sameNvidiaHeatBucket(live[bucketStart], live[bucketEnd], metrics) {
				bucketEnd++
			}
			if bucketEnd-bucketStart > 1 {
				sort.SliceStable(live[bucketStart:bucketEnd], func(i, j int) bool {
					a, b := live[bucketStart+i], live[bucketStart+j]
					return nvidiaBaselineRPMFor(a, metrics) > nvidiaBaselineRPMFor(b, metrics)
				})
			}
			bucketStart = bucketEnd
		}
	}

	// Shuffle inside (priority + LastUsedAt second + reuse heat) buckets to
	// avoid snapshot bias when candidates share identical observable state.
	bucketStart := 0
	for bucketStart < len(live) {
		bucketEnd := bucketStart + 1
		for bucketEnd < len(live) && sameNvidiaSortBucket(live[bucketStart], live[bucketEnd], metrics) {
			bucketEnd++
		}
		if bucketEnd-bucketStart > 1 {
			rand.Shuffle(bucketEnd-bucketStart, func(i, j int) {
				live[bucketStart+i], live[bucketStart+j] = live[bucketStart+j], live[bucketStart+i]
			})
		}
		bucketStart = bucketEnd
	}

	// Append any nil accounts to the tail to preserve caller's expectation
	// that input length equals output length and nil entries do not leak
	// forward into scheduling decisions.
	tail := out[:0]
	for _, a := range out {
		if a == nil {
			tail = append(tail, a)
		}
	}
	return append(live, tail...)
}

// sameNvidiaPriorityAndLastUsedAtSecond reports whether two accounts are
// observationally equivalent on the deterministic parts of the NVIDIA sort
// (priority + LastUsedAt truncated to one second). Accounts tied on both
// fields fall through to reuse heat + shuffle.
func sameNvidiaPriorityAndLastUsedAtSecond(a, b *Account) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Priority != b.Priority {
		return false
	}
	if (a.LastUsedAt == nil) != (b.LastUsedAt == nil) {
		return false
	}
	if a.LastUsedAt == nil && b.LastUsedAt == nil {
		return true
	}
	return a.LastUsedAt.Truncate(time.Second).Equal(b.LastUsedAt.Truncate(time.Second))
}

// sameNvidiaSortBucket extends sameNvidiaPriorityAndLastUsedAtSecond with
// reuse-heat + baseline equality, used to bound final shuffle buckets.
// 必须包含 baseline 维度：否则 C 优化的 baseline 排序结果会被随后的
// rand.Shuffle 洗掉，让同等 heat 但 baseline 不同的账号失去 C 优化意图。
func sameNvidiaSortBucket(a, b *Account, metrics *NVIDIASharedConnectionPoolMetrics) bool {
	if !sameNvidiaPriorityAndLastUsedAtSecond(a, b) {
		return false
	}
	if metrics == nil {
		return true
	}
	if nvidiaReuseHeatFor(a, metrics) != nvidiaReuseHeatFor(b, metrics) {
		return false
	}
	return nvidiaBaselineRPMFor(a, metrics) == nvidiaBaselineRPMFor(b, metrics)
}

// sameNvidiaHeatBucket reports whether two accounts have identical
// reuse-heat (within the same priority + LastUsedAt bucket). C: 用于
// RPM 基线排序的桶边界——只有 priority + LastUsedAt + heat 都相同的账号才按基线比较。
// 必须前置 sameNvidiaPriorityAndLastUsedAtSecond: 否则 heat=0 的账号会跨不同
// priority 桶做 baseline sort, 打乱 priority 优先级的语义。
func sameNvidiaHeatBucket(a, b *Account, metrics *NVIDIASharedConnectionPoolMetrics) bool {
	if !sameNvidiaPriorityAndLastUsedAtSecond(a, b) {
		return false
	}
	if metrics == nil {
		return true
	}
	return nvidiaReuseHeatFor(a, metrics) == nvidiaReuseHeatFor(b, metrics)
}

// nvidiaReuseHeatFor returns a comparable heat scalar for an account.
// Accounts with more reuse history (positive heat) sort after accounts with
// less reuse in sortNvidiaThrottleSchedulingOrder. Recent upstream failures
// (RecordAccountFail) subtract from the heat so the scheduler stops hot-listing
// degraded accounts even when their reuse count is high.
// Penalty factor 3 ensures a single failure meaningfully overrides reuse bias
// (`weight = reusedTotal - recentFailCount*3`).
func nvidiaReuseHeatFor(a *Account, metrics *NVIDIASharedConnectionPoolMetrics) int64 {
	if a == nil || metrics == nil {
		return 0
	}
	snap := metrics.SnapshotAccountReuse(a.ID)
	return snap.ReusedTotal - snap.RecentFailCount*3
}

// nvidiaBaselineRPMFor returns the account's current RPM baseline (successful
// requests in the last minute). C: 调度在同 heat 桶内优先选基线高的账号，
// 让容量大的 NVIDIA 账号承担更多流量。
func nvidiaBaselineRPMFor(a *Account, metrics *NVIDIASharedConnectionPoolMetrics) int64 {
	if a == nil || metrics == nil {
		return 0
	}
	return metrics.SnapshotAccountBaseline(a.ID)
}
