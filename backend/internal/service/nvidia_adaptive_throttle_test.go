//go:build unit

package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClassifyNVIDIAAdaptiveThrottleSignal(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       nvidiaThrottleSignal
	}{
		{name: "status_200_is_success", statusCode: http.StatusOK, body: nil, want: nvidiaThrottleSignalSuccess},
		{name: "status_204_is_success", statusCode: http.StatusNoContent, body: nil, want: nvidiaThrottleSignalSuccess},
		{name: "status_299_is_success", statusCode: 299, body: nil, want: nvidiaThrottleSignalSuccess},
		{name: "status_300_falls_through", statusCode: http.StatusMultipleChoices, body: nil, want: nvidiaThrottleSignalNone},
		{name: "status_400_is_none", statusCode: http.StatusBadRequest, body: nil, want: nvidiaThrottleSignalNone},
		{name: "status_401_is_none", statusCode: http.StatusUnauthorized, body: nil, want: nvidiaThrottleSignalNone},
		{name: "status_429_is_rate", statusCode: http.StatusTooManyRequests, body: nil, want: nvidiaThrottleSignalRate},
		{name: "status_503_no_capacity_keyword_is_none", statusCode: http.StatusServiceUnavailable, body: []byte(`{"err":"oops"}`), want: nvidiaThrottleSignalNone},
		{name: "status_500_is_none", statusCode: http.StatusInternalServerError, body: nil, want: nvidiaThrottleSignalNone},
		{name: "status_502_is_none", statusCode: http.StatusBadGateway, body: nil, want: nvidiaThrottleSignalNone},
		{name: "status_503_capacity_marker_complete", statusCode: http.StatusServiceUnavailable,
			body: []byte(`{"detail":"ResourceExhausted worker local total request limit reached"}`),
			want: nvidiaThrottleSignalCapacity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, classifyNVIDIAAdaptiveThrottleSignal(tt.statusCode, tt.body))
		})
	}
}

func TestIsNVIDIACapacitySignal_RequiresAllMarkers(t *testing.T) {
	markers := []string{"resourceexhausted", "worker", "local", "total", "request", "limit"}
	all := []byte("")
	for _, m := range markers {
		all = append(all, []byte(" "+m)...)
	}
	require.True(t, isNVIDIACapacitySignal(all),
		"markers=[%s] collectively should classify as capacity", markers)

	for _, missing := range markers {
		body := []byte{}
		for _, m := range markers {
			if m == missing {
				continue
			}
			body = append(body, []byte(" "+m)...)
		}
		require.False(t, isNVIDIACapacitySignal(body),
			"missing any marker %q should fail classification; body=%s", missing, body)
	}
	require.False(t, isNVIDIACapacitySignal([]byte("ResourceExhausted request limit")),
		"only ResourceExhausted/Request/Limit present without worker/local/total must fail")
}

func TestIsNVIDIACapacitySignal_CaseInsensitive(t *testing.T) {
	require.True(t, isNVIDIACapacitySignal([]byte("RESOURCEEXHAUSTED Worker Local Total Request Limit")))
}

func TestNvidiaThrottleRate_SuccessDecaysSpacing(t *testing.T) {
	s := &RateLimitService{}
	settings := &NVIDIAAdaptiveThrottleSettings{MaxSpacing: 30 * time.Second}
	rate := s.nvidiaThrottleRate(nvidiaThrottleSignalSuccess, settings)
	require.Equal(t, int64(1), rate.ConsecutivePenalty, "success → penalty count =1")
	require.Equal(t, int64(0), rate.SpacingMs, "success → SpacingMs unset; DecaySpacing=true lets Lua halve existing")
	require.True(t, rate.DecaySpacing, "success → DecaySpacing=true")
}

func TestNvidiaThrottleRate_RateLimitJitterBoundedByMaxSpacing(t *testing.T) {
	s := &RateLimitService{nvidiaThrottleJitter: func(max time.Duration) time.Duration { return max * 10 }}
	settings := &NVIDIAAdaptiveThrottleSettings{MaxSpacing: time.Second}
	rate := s.nvidiaThrottleRate(nvidiaThrottleSignalRate, settings)
	require.Equal(t, int64(1), rate.ConsecutivePenalty)
	require.Equal(t, int64(settings.MaxSpacing.Milliseconds()), rate.SpacingMs, "SpacingMs capped to MaxSpacing=1s even when jitter overflows")
	require.False(t, rate.DecaySpacing, "rate-limit signal → no decay flag (penalty must not recover)")
}

func TestNvidiaThrottleRate_CapacityUsesMaxSpacingPlusJitter(t *testing.T) {
	s := &RateLimitService{nvidiaThrottleJitter: func(max time.Duration) time.Duration { return 100 * time.Millisecond }}
	settings := &NVIDIAAdaptiveThrottleSettings{MaxSpacing: 5 * time.Second}
	rate := s.nvidiaThrottleRate(nvidiaThrottleSignalCapacity, settings)
	require.Equal(t, int64(5)*1000+int64(100), rate.CapacityPenaltyMs, "CapacityPenaltyMs = MaxSpacing + jitter(ms)")
	require.Equal(t, int64(settings.MaxSpacing.Milliseconds()), rate.CapacityWindowMs)
}

func TestNvidiaThrottleRate_NoneSignalIsZero(t *testing.T) {
	s := &RateLimitService{}
	settings := &NVIDIAAdaptiveThrottleSettings{MaxSpacing: time.Second}
	rate := s.nvidiaThrottleRate(nvidiaThrottleSignalNone, settings)
	require.Equal(t, NvidiaThrottleRate{}, rate, "non classifiable signal must produce all-zero rate")
}

func TestDefaultNVIDIAAdaptiveThrottleJitter_NonNegativeAndBounded(t *testing.T) {
	for _, max := range []time.Duration{0, time.Microsecond, time.Second, time.Minute} {
		got := defaultNVIDIAAdaptiveThrottleJitter(max)
		require.LessOrEqual(t, int64(0), int64(got), "jitter must be non-negative for max=%v", max)
		require.LessOrEqual(t, int64(got), int64(max), "jitter must not exceed max=%v", max)
	}
}

func TestNVIDIAThrottleScope_IsStructValue(t *testing.T) {
	s := NVIDIAThrottleScope{AccountID: 1, CanonicalModel: "meta/llama-3.1-405b-instruct"}
	require.Equal(t, int64(1), s.AccountID)
	require.Equal(t, "meta/llama-3.1-405b-instruct", s.CanonicalModel)
}

func TestNVIDIAAdaptiveThrottleBlockedError_MessageContainsDuration(t *testing.T) {
	e := &NVIDIAAdaptiveThrottleBlockedError{RetryAfter: 5 * time.Second, ShortWait: 100 * time.Millisecond}
	require.Contains(t, e.Error(), "5s")
	require.Equal(t, 100*time.Millisecond, e.ShortWait)
}
