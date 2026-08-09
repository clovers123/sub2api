//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClassifyNVIDIA429Body 锁定 429 body 语义分级的分类行为：
//   - resource：ResourceExhausted + Worker local total request limit（委托 isNVIDIAResourceExhaustedError）
//   - rate：瞬时 RPM 超限（rate limit / requests per minute / rpm / too many requests）
//   - quota：日/周期配额耗尽（quota / daily / monthly / plan period 等）→ 长冷却
//   - 未知 body 回落到 rate（保持既有行为，不因未知语义放大冷却）
func TestClassifyNVIDIA429Body(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
		want nvidia429Class
	}{
		{
			name: "resource_exhausted_worker_local_total_request_limit",
			body: []byte(`{"error":{"message":"ResourceExhausted: Worker local total request limit reached"}}`),
			want: nvidia429Resource,
		},
		{
			name: "resource_exhausted_lowercase",
			body: []byte(`{"error":{"message":"resourceexhausted: worker local total request limit"}}`),
			want: nvidia429Resource,
		},
		{
			name: "quota_daily_exceeded",
			body: []byte(`{"error":{"message":"You exceeded your daily quota"}}`),
			want: nvidia429Quota,
		},
		{
			name: "quota_plan_period_exceeded",
			body: []byte(`{"error":{"message":"Your plan period limit has been exceeded"}}`),
			want: nvidia429Quota,
		},
		{
			name: "quota_monthly_limit",
			body: []byte(`{"error":{"message":"Monthly token limit exhausted"}}`),
			want: nvidia429Quota,
		},
		{
			name: "quota_current_quota",
			body: []byte(`{"error":{"message":"Exceeded your current quota"}}`),
			want: nvidia429Quota,
		},
		{
			name: "rate_limit_exceeded_generic",
			body: []byte(`{"error":{"message":"Rate limit exceeded"}}`),
			want: nvidia429Rate,
		},
		{
			name: "rate_requests_per_minute",
			body: []byte(`{"error":{"message":"Requests per minute limit reached"}}`),
			want: nvidia429Rate,
		},
		{
			name: "rate_too_many_requests",
			body: []byte(`{"error":{"message":"429 Too Many Requests"}}`),
			want: nvidia429Rate,
		},
		{
			name: "empty_body_defaults_to_rate",
			body: []byte(``),
			want: nvidia429Rate,
		},
		{
			name: "unknown_body_defaults_to_rate",
			body: []byte(`{"error":{"message":"some random error"}}`),
			want: nvidia429Rate,
		},
		{
			name: "quota_keyword_inside_larger_message",
			body: []byte(`{"error":{"message":"[RATE] API quota exhausted for today, retry later"}}`),
			want: nvidia429Quota,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, classifyNVIDIA429Body(tt.body))
		})
	}
}

// TestClassifyNVIDIA429BodyConstantValues 锁定 nvidia429Class 枚举值，
// 防止未来有人调整枚举顺序破坏持久化/日志语义（当前无持久化，但保持稳定）。
func TestClassifyNVIDIA429BodyConstantValues(t *testing.T) {
	t.Parallel()
	require.Equal(t, nvidia429Class(0), nvidia429Rate)
	require.Equal(t, nvidia429Class(1), nvidia429Quota)
	require.Equal(t, nvidia429Class(2), nvidia429Resource)
}

// TestClassifyNVIDIA429ClassEstablishesResourcePriority 验证 resource 判定
// 优先于 quota：NVIDIA worker 池耗尽报文不因含 "exceeded" 字样被误判为配额型。
func TestClassifyNVIDIA429ClassEstablishesResourcePriority(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":{"message":"ResourceExhausted: Worker local total request limit exceeded"}}`)
	require.Equal(t, nvidia429Resource, classifyNVIDIA429Body(body))
}

// TestIsNVIDIAResourceExhaustedErrorStatusGate 锁死 429 分类函数的 status 前置条件，
// 确保只有 429/503 才会被当作 resource（与既有 isNVIDIAResourceExhaustedError 一致）。
func TestIsNVIDIAResourceExhaustedErrorStatusGate(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":{"message":"ResourceExhausted: worker local total request limit"}}`)
	require.True(t, isNVIDIAResourceExhaustedError(http.StatusTooManyRequests, body))
	require.True(t, isNVIDIAResourceExhaustedError(http.StatusServiceUnavailable, body))
	require.False(t, isNVIDIAResourceExhaustedError(http.StatusBadRequest, body))
	require.False(t, isNVIDIAResourceExhaustedError(http.StatusOK, body))
}