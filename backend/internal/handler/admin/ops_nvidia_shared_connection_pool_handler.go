package admin

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// GetNVIDIASharedConnectionPoolMetrics returns a snapshot of NVIDIA shared
// connection pool runtime metrics (reuse rate, latency percentiles).
// GET /api/v1/admin/ops/nvidia-shared-connection-pool
func (h *OpsHandler) GetNVIDIASharedConnectionPoolMetrics(c *gin.Context) {
	if h.nvidiaSharedConnectionPoolMetrics == nil {
		response.Error(c, http.StatusServiceUnavailable, "NVIDIA shared connection pool metrics not available")
		return
	}
	response.Success(c, h.nvidiaSharedConnectionPoolMetrics.Snapshot())
}
