package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// NVIDIA 账号 smoke probe 常量。
const (
	// nvidiaSmokeProbeTimeout 单次 NVIDIA key 探活的总超时。
	nvidiaSmokeProbeTimeout = 15 * time.Second
	// nvidiaSmokeProbeMaxBodyBytes 探活响应体读取上限，防止恶意/异常大 body 拖垮内存。
	nvidiaSmokeProbeMaxBodyBytes = 4096
)

// ProbeNVIDIAAccountSmoke 对给定 base_url 的 API-Key 账号做一次轻量探活：
// 请求 `GET {base_url}/v1/models`（NVIDIA NIM 的 OpenAI 兼容端点），
// 用一个零成本请求验证 key 是否有效、网络是否可达。
//
// 返回探活结果分类，供调用方决定"创建即失败"（401/403）还是"仅告警"
// （网络超时/5xx 等暂时性失败——账号可能有效只是当前网络/上游抖动）。
//
// 探活只读不写，不消耗客户端任何配额；不会触发 NVIDIA 风控
// （单个 models 列表请求与正常 SDK 启动行为一致）。
//
// 注意：本函数不做 NVIDIA hostname 判定（由调用方 scheduleNVIDIAAccountSmokeProbe
// 用 isNVIDIAAccountByHostname 过滤后再调用），这样测试可注入任意 httptest URL。
func ProbeNVIDIAAccountSmoke(ctx context.Context, client *http.Client, account *Account) NVIDIASmokeProbeResult {
	if account == nil || client == nil {
		return NVIDIASmokeProbeResult{Outcome: NVIDIASmokeProbeOutcomeInvalid}
	}
	baseURL := account.GetBaseURL()
	if baseURL == "" {
		return NVIDIASmokeProbeResult{Outcome: NVIDIASmokeProbeOutcomeInvalid}
	}
	apiKey := account.GetCredential("api_key")
	if strings.TrimSpace(apiKey) == "" {
		return NVIDIASmokeProbeResult{Outcome: NVIDIASmokeProbeOutcomeInvalid}
	}

	probeCtx, cancel := context.WithTimeout(ctx, nvidiaSmokeProbeTimeout)
	defer cancel()

	modelsURL := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return NVIDIASmokeProbeResult{Outcome: NVIDIASmokeProbeOutcomeNetworkError}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// 超时/连接拒绝 → 网络问题，账号本身可能有效，仅告警。
		return NVIDIASmokeProbeResult{Outcome: NVIDIASmokeProbeOutcomeNetworkError}
	}
	defer func() { _ = resp.Body.Close() }()

	// 读取并丢弃 body（限制上限），复用连接。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, nvidiaSmokeProbeMaxBodyBytes))

	switch {
	case resp.StatusCode == http.StatusOK:
		return NVIDIASmokeProbeResult{Outcome: NVIDIASmokeProbeOutcomeOK}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// key 明确无效 → 创建即失败。
		return NVIDIASmokeProbeResult{
			Outcome:     NVIDIASmokeProbeOutcomeUnauthorized,
			StatusCode:  resp.StatusCode,
			Description: "NVIDIA API key rejected: " + resp.Status,
		}
	default:
		// 5xx / 429 等暂时性失败 → 账号可能有效，仅告警。
		return NVIDIASmokeProbeResult{
			Outcome:    NVIDIASmokeProbeOutcomeUpstreamError,
			StatusCode: resp.StatusCode,
		}
	}
}

// NVIDIASmokeProbeOutcome 是探活结果分类。
type NVIDIASmokeProbeOutcome int

const (
	// NVIDIASmokeProbeOutcomeOK key 有效，网络可达。
	NVIDIASmokeProbeOutcomeOK NVIDIASmokeProbeOutcome = iota
	// NVIDIASmokeProbeOutcomeUnauthorized key 被 NVIDIA 拒绝（401/403）→ 创建应失败。
	NVIDIASmokeProbeOutcomeUnauthorized
	// NVIDIASmokeProbeOutcomeUpstreamError 上游返回 5xx/429 等暂时性错误 → 仅告警。
	NVIDIASmokeProbeOutcomeUpstreamError
	// NVIDIASmokeProbeOutcomeNetworkError 网络不可达/超时 → 仅告警。
	NVIDIASmokeProbeOutcomeNetworkError
	// NVIDIASmokeProbeOutcomeInvalid 参数不合法（非 NVIDIA 账号/缺 key）→ 不探活。
	NVIDIASmokeProbeOutcomeInvalid
)

// NVIDIASmokeProbeResult 是一次探活的结果。
type NVIDIASmokeProbeResult struct {
	Outcome     NVIDIASmokeProbeOutcome
	StatusCode  int
	Description string
}

// SmokeProbeBlocking 表示该结果是否应阻断账号创建。
func (r NVIDIASmokeProbeResult) SmokeProbeBlocking() bool {
	return r.Outcome == NVIDIASmokeProbeOutcomeUnauthorized
}

// Log 统一记录探活结果，让运维 grep `nvidia_smoke_probe` 可追踪全部探活事件。
func (r NVIDIASmokeProbeResult) Log(accountID int64) {
	fields := []zap.Field{
		zap.Int64("account_id", accountID),
		zap.String("outcome", r.Outcome.String()),
	}
	if r.StatusCode > 0 {
		fields = append(fields, zap.Int("status_code", r.StatusCode))
	}
	if r.Description != "" {
		fields = append(fields, zap.String("description", r.Description))
	}
	switch r.Outcome {
	case NVIDIASmokeProbeOutcomeOK:
		logger.L().Info("nvidia_smoke_probe", fields...)
	case NVIDIASmokeProbeOutcomeUnauthorized:
		logger.L().Error("nvidia_smoke_probe", fields...)
	default:
		logger.L().Warn("nvidia_smoke_probe", fields...)
	}
}

// String 返回可 grep 的结果分类字符串。
func (o NVIDIASmokeProbeOutcome) String() string {
	switch o {
	case NVIDIASmokeProbeOutcomeOK:
		return "ok"
	case NVIDIASmokeProbeOutcomeUnauthorized:
		return "unauthorized"
	case NVIDIASmokeProbeOutcomeUpstreamError:
		return "upstream_error"
	case NVIDIASmokeProbeOutcomeNetworkError:
		return "network_error"
	case NVIDIASmokeProbeOutcomeInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}
