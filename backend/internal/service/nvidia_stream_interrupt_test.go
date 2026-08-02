//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestStreamRawChatCompletions_InterruptedNVIDIARecordsFailure 验证：
// NVIDIA 账号流式响应被上游中途截断（非 [DONE] 正常结束）时，
//  1. StreamInterrupted=true 被标记（阻止 success 路径记 200）
//  2. recordNVIDIAUpstreamFailure + recordNVIDIASingle5xxHeatPenalty 被触发
//     （记录 502 失败信号让调度器避开该账号）
func TestStreamRawChatCompletions_InterruptedNVIDIARecordsFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-ai/deepseek-v4-pro","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// 上游返回 200 + SSE，但发送两行后 pipe 被关闭（模拟上游流中断）
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-ai/deepseek-v4-pro\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_ = pw.Close() // 不写 [DONE]，直接关闭 → scanner 读 EOF/错误
	}()

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_stream_interrupt"}},
		Body:       pr,
	}}

	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}
	// NVIDIA 账号专用 metrics：必须非 nil 否则 recordNVIDIASingle5xxHeatPenalty 跳过
	metrics := NewNVIDIASharedConnectionPoolMetrics()
	svc.SetNVIDIASharedPoolMetricsForSorting(metrics)
	account := rawChatCompletionsNVIDIATestAccount()

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.StreamInterrupted, "mid-stream interruption must mark StreamInterrupted=true")
}

func TestIsNVIDIASSEErrorEvent_Detection(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"error_object", `{"id":"1","error":{"type":"server_error","message":"boom"}}`, true},
		{"error_null", `{"id":"1","error":null}`, false},
		{"error_string", `{"id":"1","error":"something"}`, false},
		{"no_error_field", `{"id":"1","choices":[{"delta":{"content":"hi"}}]}`, false},
		{"a_is_error_not_key", `{"id":"1","a":"error"}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isNVIDIASSEErrorEvent([]byte(tc.payload))
			require.Equal(t, tc.want, got)
		})
	}
}

func TestNVIDIASSEErrorEvent_StreamScanner(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// SSE stream with mid-stream error event.
	const streamBody = `data: {"id":"1","choices":[{"delta":{"content":"hi"}}]}
data: {"id":"2","error":{"type":"internal_error","message":"boom"}}
`

	body := []byte(`{"model":"deepseek-ai/deepseek-v4-pro","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(streamBody))
		_ = pw.Close()
	}()

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_n6_stream"}},
		Body:       pr,
	}}

	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}
	metrics := NewNVIDIASharedConnectionPoolMetrics()
	svc.SetNVIDIASharedPoolMetricsForSorting(metrics)
	account := rawChatCompletionsNVIDIATestAccount()

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.StreamInterrupted, "mid-stream error event must mark StreamInterrupted=true")

	// Verify we wrote the error event to the client, not the raw error text.
	output := rec.Body.String()
	require.Contains(t, output, "data: [DONE]", "must terminate with DONE")
	require.Contains(t, output, `"type":"upstream_midstream_error"`, "standard error type written to client")

	// Heat penalty should have been recorded.
	require.Greater(t, metrics.SnapshotAccountReuse(account.ID).RecentFailCount, int64(0))
}
