package admin

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// nvidiaSharedConnectionPoolStatus wraps pool metrics + prewarmer status.
type nvidiaSharedConnectionPoolStatus struct {
	service.NVIDIASharedConnectionPoolMetricsSnapshot
	Prewarm service.NVIDIAConnectionPrewarmerSnapshot `json:"prewarm"`
}

// GetNVIDIASharedConnectionPoolMetrics returns a snapshot of NVIDIA shared
// connection pool runtime metrics (reuse rate, latency percentiles).
// GET /api/v1/admin/ops/nvidia-shared-connection-pool
func (h *OpsHandler) GetNVIDIASharedConnectionPoolMetrics(c *gin.Context) {
	if h.nvidiaSharedConnectionPoolMetrics == nil {
		response.Error(c, http.StatusServiceUnavailable, "NVIDIA shared connection pool metrics not available")
		return
	}
	response.Success(c, nvidiaSharedConnectionPoolStatus{
		NVIDIASharedConnectionPoolMetricsSnapshot: h.nvidiaSharedConnectionPoolMetrics.Snapshot(),
		Prewarm: h.nvidiaPrewarmer.Snapshot(),
	})
}

// GetNVIDIAAdaptiveThrottleMetrics returns a snapshot of the NVIDIA adaptive
// throttle live counters (Reserves / Allowed / Blocked / Outcomes / CacheErrors).
// D: ops 面板实时命中率可视化——管理员可看到被节流次数 vs 放行次数。
// GET /api/v1/admin/ops/nvidia-adaptive-throttle
func (h *OpsHandler) GetNVIDIAAdaptiveThrottleMetrics(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	response.Success(c, h.opsService.SnapshotNVIDIAAdaptiveThrottle())
}
