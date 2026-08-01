package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// F10: NVIDIA 上游失败 reason 常量。所有 recordNVIDIAUpstreamFailure 调用
// 必须从此处取值，确保日志/告警字段可 grep 不漂移。
const (
	nvidiaReasonThrottleHandled  = "nvidia_throttle_handled"
	nvidiaReasonUnauthorized      = "nvidia_unauthorized"
	nvidiaReasonResourceExhausted = "nvidia_resource_exhausted"
	nvidiaReason5xxUpstream       = "nvidia_5xx_upstream"
	nvidiaReasonRetrySendError    = "nvidia_retry_send_error"
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
