//go:build unit

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)
func TestGetNVIDIAAdaptiveThrottleMetrics_ZeroWhenSourceNil(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// OpsService 未注入 throttle source → 返回零值快照（不 503）
	svc := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h := &OpsHandler{opsService: svc}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/ops/nvidia-adaptive-throttle", nil)

	h.GetNVIDIAAdaptiveThrottleMetrics(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Data service.NVIDIAAdaptiveThrottleMetricsSnapshot `json:"Data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, int64(0), body.Data.Reserves)
	require.Equal(t, int64(0), body.Data.Blocked)
}

func TestGetNVIDIAAdaptiveThrottleMetrics_ServiceUnavailableWhenNilOps(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &OpsHandler{opsService: nil}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/ops/nvidia-adaptive-throttle", nil)

	h.GetNVIDIAAdaptiveThrottleMetrics(c)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "Ops service not available")
}

func TestGetNVIDIAAdaptiveThrottleMetrics_SuccessShape(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 验证响应 JSON 包含 5 个计数字段（与 NVIDIAAdaptiveThrottleMetricsSnapshot 对齐）
	svc := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h := &OpsHandler{opsService: svc}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/ops/nvidia-adaptive-throttle", nil)

	h.GetNVIDIAAdaptiveThrottleMetrics(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Data map[string]any `json:"Data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	for _, key := range []string{"Reserves", "Allowed", "Blocked", "Outcomes", "CacheErrors"} {
		_, ok := body.Data[key]
		require.True(t, ok, "response must contain field %q", key)
	}
}
