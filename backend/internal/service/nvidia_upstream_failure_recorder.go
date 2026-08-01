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

// nvidiaSingle5xxHeatPenaltyCount 是单次 NVIDIA 5xx 注入的虚拟失败计数。
//
// 仅做热度降权, 不 BlockAccountScheduling — 真正的连续 5xx 冷却仍由
// maybeCoolNVIDIAOnConsecutive5xx 在累计达到 threshold 时触发。
// 成功 reuse 后 consecutiveReuseSuccess 衰减阈值已自动清零, 不产生永久压顶。
const nvidiaSingle5xxHeatPenaltyCount = 5

// recordNVIDIASingle5xxHeatPenalty 在单次 NVIDIA 5xx 错误时立即注入虚拟失败计数。
// nvidiaReuseHeatFor: heat = ReusedTotal - 3*RecentFailCount
// 注入 5 个虚拟失败计数后热度立即为负, sortNvidiaThrottleSchedulingOrder
// 把该账号排到末位, 让下次 failover 走到健康账号。仅影响热度排序, 不触发账号封锁。
func recordNVIDIASingle5xxHeatPenalty(s *OpenAIGatewayService, account *Account) {
	if account == nil || s == nil {
		return
	}
	metrics := s.nvidiaSharedPoolMetricsForSorting.Load()
	if metrics == nil {
		return
	}
	now := time.Now()
	for i := 0; i < nvidiaSingle5xxHeatPenaltyCount; i++ {
		metrics.RecordAccountFail(account.ID, now)
	}
	logger.L().Debug("nvidia single 5xx heat penalty applied",
		zap.Int64("account_id", account.ID),
		zap.Int("penalty_count", nvidiaSingle5xxHeatPenaltyCount),
	)
}
