package service

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

type nvidiaThrottleSignal int

const (
	nvidiaThrottleSignalNone nvidiaThrottleSignal = iota
	nvidiaThrottleSignalSuccess
	nvidiaThrottleSignalRate
	nvidiaThrottleSignalCapacity
)

// NVIDIAThrottleScope identifies an NVIDIA account and canonical model pair.
type NVIDIAThrottleScope struct {
	AccountID      int64
	CanonicalModel string
}

// NvidiaReserveRequest carries an adaptive-throttle reservation request.
type NvidiaReserveRequest struct {
	Scope NVIDIAThrottleScope
}

// NvidiaReserveResult carries the reservation decision and optional blocked error.
type NvidiaReserveResult struct {
	Allowed      bool
	RetryAfterMs int64
	Err          error
}

// NvidiaThrottleUpdate carries an upstream outcome for adaptive-throttle learning.
//
// Headers carries the NVIDIA upstream response headers. The 429 path uses
// `Retry-After` (delta-seconds or HTTP-date per RFC 6585 §4) to calibrate
// spacing — when NVIDIA explicitly tells us how long to wait the scheduler
// MUST honor that instruction rather than overwriting it with the default
// `1s + jitter` spacing that burns client RPS quota in 429 storms.
//
// Headers may be nil when the caller has no header access (e.g. test paths);
// a missing/empty `Retry-After` is equivalent to falling back to the default.
type NvidiaThrottleUpdate struct {
	Scope        NVIDIAThrottleScope
	StatusCode   int
	Headers      http.Header
	ResponseBody []byte
}

// NVIDIAAdaptiveThrottleBlockedError reports a temporary adaptive-throttle block.
type NVIDIAAdaptiveThrottleBlockedError struct {
	RetryAfter time.Duration
	ShortWait  time.Duration
}

func (e *NVIDIAAdaptiveThrottleBlockedError) Error() string {
	return fmt.Sprintf("nvidia adaptive throttle blocked for %s", e.RetryAfter)
}

// ReserveNVIDIAAdaptiveThrottle reserves an NVIDIA account/model send window.
func (s *RateLimitService) ReserveNVIDIAAdaptiveThrottle(ctx context.Context, request NvidiaReserveRequest) (NvidiaReserveResult, bool) {
	if !s.nvidiaAdaptiveThrottleEnabled(ctx, request.Scope) {
		return NvidiaReserveResult{}, false
	}
	s.ensureNVIDIAAdaptiveThrottleRuntime()
	s.nvidiaMetrics.Reserves.Add(1)
	result, err := s.nvidiaAdaptiveThrottleCache.Reserve(ctx, request.Scope.AccountID, request.Scope.CanonicalModel)
	if err != nil {
		s.nvidiaMetrics.CacheErrors.Add(1)
		return NvidiaReserveResult{}, false
	}
	if result.Allowed {
		s.nvidiaMetrics.Allowed.Add(1)
		return NvidiaReserveResult{Allowed: true}, true
	}
	s.nvidiaMetrics.Blocked.Add(1)
	retryAfter := time.Duration(result.RetryAfterMs) * time.Millisecond
	return NvidiaReserveResult{
		Allowed:      false,
		RetryAfterMs: result.RetryAfterMs,
		Err:          &NVIDIAAdaptiveThrottleBlockedError{RetryAfter: retryAfter},
	}, true
}

// RecordNVIDIAAdaptiveThrottleOutcome records a classified upstream outcome.
func (s *RateLimitService) RecordNVIDIAAdaptiveThrottleOutcome(ctx context.Context, update NvidiaThrottleUpdate) bool {
	if !s.nvidiaAdaptiveThrottleEnabled(ctx, update.Scope) {
		return false
	}
	signal := classifyNVIDIAAdaptiveThrottleSignal(update.StatusCode, update.ResponseBody)
	if signal == nvidiaThrottleSignalNone {
		return false
	}
	settings := s.getNVIDIAAdaptiveThrottleSettings(ctx)
	rate := s.nvidiaThrottleRate(signal, settings, update.Headers)
	if err := s.nvidiaAdaptiveThrottleCache.Apply(ctx, update.Scope.AccountID, update.Scope.CanonicalModel, rate); err != nil {
		s.ensureNVIDIAAdaptiveThrottleRuntime()
		s.nvidiaMetrics.CacheErrors.Add(1)
		return false
	}
	s.ensureNVIDIAAdaptiveThrottleRuntime()
	s.nvidiaMetrics.Outcomes.Add(1)
	return true
}

func (s *RateLimitService) nvidiaAdaptiveThrottleEnabled(ctx context.Context, scope NVIDIAThrottleScope) bool {
	if s == nil || s.nvidiaAdaptiveThrottleCache == nil || scope.AccountID <= 0 || strings.TrimSpace(scope.CanonicalModel) == "" {
		return false
	}
	return s.getNVIDIAAdaptiveThrottleSettings(ctx).Enabled
}

func (s *RateLimitService) nvidiaThrottleRate(signal nvidiaThrottleSignal, settings *NVIDIAAdaptiveThrottleSettings, headers http.Header) NvidiaThrottleRate {
	switch signal {
	case nvidiaThrottleSignalSuccess:
		return NvidiaThrottleRate{ConsecutivePenalty: 1, DecaySpacing: true}
	case nvidiaThrottleSignalRate:
		// NVIDIA NIM 在 429 响应中常带 Retry-After 头给出明确的等待指令。
		// 优先 honor 上游显式指令,缺失/无效时回退原有 1s+jitter 默认。
		// 必要性: NVIDIA 容量极紧时 Retry-After: 5/10/30 是真实容量恢复曲线,
		//   覆盖为 1s spacing 会引发连续 6 次无谓被拒 + 烧光 client RPS 配额,
		//   并触发 NVIDIA 主动风控升级。RFC 6585 §4 明确指令应当被 honor。
		spacing := time.Second
		if headers != nil {
			if ra := ParseRetryAfter(headers, time.Now()); ra > spacing {
				spacing = ra
			}
		}
		spacing += s.nvidiaThrottleJitter(time.Second)
		if spacing > settings.MaxSpacing {
			spacing = settings.MaxSpacing
		}
		return NvidiaThrottleRate{SpacingMs: spacing.Milliseconds(), ConsecutivePenalty: 1}
	case nvidiaThrottleSignalCapacity:
		penalty := settings.MaxSpacing + s.nvidiaThrottleJitter(settings.MaxSpacing)
		return NvidiaThrottleRate{CapacityPenaltyMs: penalty.Milliseconds(), CapacityWindowMs: settings.MaxSpacing.Milliseconds()}
	default:
		return NvidiaThrottleRate{}
	}
}

func classifyNVIDIAAdaptiveThrottleSignal(statusCode int, responseBody []byte) nvidiaThrottleSignal {
	switch {
	case statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices:
		return nvidiaThrottleSignalSuccess
	case statusCode == http.StatusTooManyRequests:
		return nvidiaThrottleSignalRate
	case statusCode == http.StatusServiceUnavailable && isNVIDIACapacitySignal(responseBody):
		return nvidiaThrottleSignalCapacity
	default:
		return nvidiaThrottleSignalNone
	}
}

func isNVIDIACapacitySignal(responseBody []byte) bool {
	lower := strings.ToLower(string(responseBody))
	for _, marker := range []string{"resourceexhausted", "worker", "local", "total", "request", "limit"} {
		if !strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

func (s *RateLimitService) ensureNVIDIAAdaptiveThrottleRuntime() {
	if s.nvidiaMetrics == nil {
		s.nvidiaMetrics = &NVIDIAAdaptiveThrottleMetrics{}
	}
	if s.nvidiaThrottleJitter == nil {
		s.nvidiaThrottleJitter = defaultNVIDIAAdaptiveThrottleJitter
	}
}

func defaultNVIDIAAdaptiveThrottleJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}
