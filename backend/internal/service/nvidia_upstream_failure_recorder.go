package service

import (
	"context"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// F10: NVIDIA 上游失败 reason 常量。所有 recordNVIDIAUpstreamFailure 调用
// 必须从此处取值，确保日志/告警字段可 grep 不漂移。
const (
	nvidiaReasonThrottleHandled     = "nvidia_throttle_handled"
	nvidiaReasonUnauthorized        = "nvidia_unauthorized"
	nvidiaReasonResourceExhausted   = "nvidia_resource_exhausted"
	nvidiaReason5xxUpstream         = "nvidia_5xx_upstream"
	nvidiaReason5xxNIMInternal      = "nvidia_5xx_nim_internal"      // 500: NIM 自身故障
	nvidiaReason5xxGatewayTransient = "nvidia_5xx_gateway_transient" // 502/503/504: 网关层临时
	nvidiaReasonStreamInterrupt     = "nvidia_stream_interrupt"      // SSE 流中断（非 HTTP status）
	nvidiaReasonRetrySendError      = "nvidia_retry_send_error"
	nvidiaReasonQuotaExhausted      = "nvidia_quota_exhausted" // 429 配额耗尽（quota/daily/monthly 语义）→ 30min 长冷却
)

// recordNVIDIAUpstreamFailure 是 NVIDIA 上游错误的统一记录入口，集中处理：
//   - 脱敏日志（通过 logNVIDIAUpstreamError）
//   - 调度惩罚计数（通过 RecordAccountFail），影响 nvidiaReuseHeatFor 排序
//
// 调用方只需要传入 reason + statusCode + body；本函数会自动从 s 取出 metrics
// 句柄并调用相应方法。空 s 或 nil account 不会 panic。
//
// reason 必须取自上方 nvidiaReason* 常量块，确保日志/告警字段可 grep 不漂移。
//
// N3 依赖此函数把"上游失败"信号同步给调度器；不入记录会导致失败账号继续
// 被 hot-listing，违背 N3 的设计目的。
func recordNVIDIAUpstreamFailure(s *OpenAIGatewayService, ctx context.Context, account *Account, statusCode int, body []byte, reason string) {
	if account == nil {
		return
	}

	// 统一脱敏日志 — N1/N2 的所有 NVIDIA 上游错误路径都走这里
	logNVIDIAUpstreamError(ctx, account, statusCode, body, reason)

	// 调度器惩罚计数 — N3: recentFailCount 让 nvidiaReuseHeatFor 调权。
	// 仅在 metrics 句柄可用时进行；空句柄安全跳过。
	if s == nil {
		return
	}
	if metrics := s.nvidiaSharedPoolMetricsForSorting.Load(); metrics != nil {
		metrics.RecordAccountFail(account.ID, time.Now())
		logger.L().Debug("recorded nvidia upstream failure for scheduling penalty",
			zap.Int64("account_id", account.ID),
			zap.String("reason", reason),
		)
	}
}

// 5xx 子类区分对待的虚拟失败计数（N4）。
//   - 500 NIM 自身故障 → 加重惩罚，迅速排除账号
//   - 502/503/504 网关层临时 → 维持原值，允许快速恢复
//   - 0/未知 5xx → 维持原值（向后兼容）
const (
	nvidiaSingle5xxTransientHeatPenaltyCount = 5
	nvidiaSingle5xx500HeatPenaltyCount       = 10
)

// recordNVIDIASingle5xxHeatPenalty 按 N4 决策注入虚拟失败计数：
// heat = ReusedTotal - 3*RecentFailCount，heat 为负时账号排末位。
// 仅影响排序，不 BlockAccountScheduling（连续 5xx 冷却交给 maybeCoolNVIDIAOnConsecutive5xx）。
// stream interrupt 路径（无 HTTP code）传 StatusBadGateway，按"网关层临时"处理。
func recordNVIDIASingle5xxHeatPenalty(s *OpenAIGatewayService, account *Account, statusCode int) {
	if account == nil || s == nil {
		return
	}
	metrics := s.nvidiaSharedPoolMetricsForSorting.Load()
	if metrics == nil {
		return
	}

	penaltyCount := nvidiaSingle5xxTransientHeatPenaltyCount
	reason := nvidiaReason5xxUpstream
	switch statusCode {
	case http.StatusInternalServerError:
		penaltyCount = nvidiaSingle5xx500HeatPenaltyCount
		reason = nvidiaReason5xxNIMInternal
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		reason = nvidiaReason5xxGatewayTransient
	}

	now := time.Now()
	for i := 0; i < penaltyCount; i++ {
		metrics.RecordAccountFail(account.ID, now)
	}
	logger.L().Debug("nvidia single 5xx heat penalty applied",
		zap.Int64("account_id", account.ID),
		zap.Int("status_code", statusCode),
		zap.Int("penalty_count", penaltyCount),
		zap.String("reason", reason),
	)
}
