package service

import (
	"bytes"
	"net/http"
)

// nvidia429Class 是 429 响应 body 的语义分级，决定冷却力度。
type nvidia429Class int

const (
	// nvidia429Rate 短冷却：瞬时 RPM 超限 / 无法识别的 429。
	// 保持既有自适应节流行为（1s+Retry-After spacing + L1 8s）。
	nvidia429Rate nvidia429Class = iota
	// nvidia429Quota 长冷却：日/周期配额耗尽。应送较长 BlockAccountScheduling
	// 冷却，避免高频轮询触发风控升级；不应继续走自适应节流短退避。
	nvidia429Quota
	// nvidia429Resource 中冷却：worker 池饱和（ResourceExhausted），
	// 委托既有 isNVIDIAResourceExhaustedError 语义（2min 封禁）。
	nvidia429Resource
)

// nvidia429QuotaMarkers 是 quota（配额耗尽）语义的强关键词。
// 关键词保守：只选「配额」专属词，避免把 "Rate limit exceeded" 等
// 瞬时限流报文误判为配额耗尽（"rate limit" 不含 quota 系列词）。
var nvidia429QuotaMarkers = []string{
	"quota",
	"daily",
	"monthly",
	"hourly",
	"period",
}

// classifyNVIDIA429Body 将 429 body 按语义分级：
//
//  1. resource：委托 isNVIDIAResourceExhaustedError（ResourceExhausted + worker local total request limit）
//     ——权重最高，NVIDIA worker 池饱和报文中即使包含 "exceeded" 等字样也不能降级。
//  2. quota：body 含配额耗尽强关键词（quota/daily/monthly/hourly/period）→ 长冷却。
//  3. 默认 rate：未知 body 保持既有短退避语义，不因未知报文放大冷却。
//
// 分级依据纯 body 文本，不下探 Retry-After（该维度由 nvidiaThrottleRate 已有逻辑处理）。
func classifyNVIDIA429Body(body []byte) nvidia429Class {
	if isNVIDIAResourceExhaustedError(http.StatusTooManyRequests, body) {
		return nvidia429Resource
	}
	if len(body) == 0 {
		return nvidia429Rate
	}
	lower := bytes.ToLower(body)
	for _, marker := range nvidia429QuotaMarkers {
		if bytes.Contains(lower, []byte(marker)) {
			return nvidia429Quota
		}
	}
	return nvidia429Rate
}