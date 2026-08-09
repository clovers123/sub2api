package service

import (
	"context"
	"errors"
	"sync"
)

// errNVIDIAInflightLimitReached 是 A1 并发槽位已满时的哨兵错误（已写 429 响应）。
var errNVIDIAInflightLimitReached = errors.New("nvidia upstream inflight limit reached")

// NVIDIAInflightTracker 按 (account, canonical_model) 粒度跟踪进行中的 NVIDIA 推理请求数，
// 实现 A1 并发维度节流：per-scope 信号量，默认 max_inflight=4。
//
// 设计要点：
//   - max=0 表示"并发控制关闭"，Acquire 永远放行（但计数仍递增，便于上层观测）；
//   - Release 永远不把计数减为负数；
//   - 所有操作均并发安全，映射懒初始化。
type NVIDIAInflightTracker struct {
	mu      sync.Mutex
	inflight map[NVIDIAThrottleScope]int
}

func newNVIDIAInflightTracker() *NVIDIAInflightTracker {
	return &NVIDIAInflightTracker{inflight: make(map[NVIDIAThrottleScope]int)}
}

// Acquire 尝试为 scope 预约一个并发槽位。
// max <= 0 表示关闭并发控制：恒返回 true（仍会计数）。
// 返回 false 表示已达上限，调用方应拒绝新请求（如返回 429 语义错误）。
func (t *NVIDIAInflightTracker) Acquire(scope NVIDIAThrottleScope, max int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if max > 0 && t.inflight[scope] >= max {
		return false
	}
	t.inflight[scope]++
	return true
}

// Release 归还 scope 的一个并发槽位。次数不足时钳制在 0，绝不出现负数。
// 归零后删除对应 entry，避免 (account, model) 组合随时间无限增长导致内存泄漏。
func (t *NVIDIAInflightTracker) Release(scope NVIDIAThrottleScope) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inflight[scope] > 0 {
		t.inflight[scope]--
		if t.inflight[scope] == 0 {
			delete(t.inflight, scope)
		}
	}
}

// Count 返回 scope 当前进行中的请求数（观测用）。
func (t *NVIDIAInflightTracker) Count(scope NVIDIAThrottleScope) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inflight[scope]
}

// nvidiaInflightFull 报告 scope 的并发槽位是否已满（A1 调度层预 gate 用）。
// MaxInflight<=0（关闭并发控制）时永远不满。
func (s *RateLimitService) nvidiaInflightFull(ctx context.Context, scope NVIDIAThrottleScope) bool {
	if s == nil || s.nvidiaInflight == nil {
		return false
	}
	s.ensureNVIDIAAdaptiveThrottleRuntime()
	settings := s.getNVIDIAAdaptiveThrottleSettings(ctx)
	if settings.MaxInflight <= 0 {
		return false
	}
	return s.nvidiaInflight.Count(scope) >= settings.MaxInflight
}

// TryAcquireNVIDIAInflight 尝试为 scope 预约一个并发槽位（A1 forward 层）。
// 返回 release 回调与是否成功；MaxInflight<=0（关闭）时恒成功且 release 为 no-op。
// 成功时调用方必须保证在请求完成（含流结束/失败）后调用 release，否则槽位泄漏。
func (s *RateLimitService) TryAcquireNVIDIAInflight(ctx context.Context, scope NVIDIAThrottleScope) (release func(), ok bool) {
	noop := func() {}
	if s == nil || s.nvidiaInflight == nil {
		return noop, true
	}
	s.ensureNVIDIAAdaptiveThrottleRuntime()
	settings := s.getNVIDIAAdaptiveThrottleSettings(ctx)
	if settings.MaxInflight <= 0 {
		return noop, true
	}
	if !s.nvidiaInflight.Acquire(scope, settings.MaxInflight) {
		return nil, false
	}
	return func() { s.nvidiaInflight.Release(scope) }, true
}