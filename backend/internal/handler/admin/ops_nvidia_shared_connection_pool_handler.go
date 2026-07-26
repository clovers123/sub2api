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
