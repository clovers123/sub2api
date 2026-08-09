package service

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	openAIAccountStateUpdateTimeout       = 5 * time.Second
	openAIOAuth429FallbackCooldown        = 5 * time.Second
	openAIOAuth429RetryWindow             = 2 * time.Minute
	openAIOAuth429RetryDelay              = 500 * time.Millisecond
	openAIOAuth429MaxRetryDelay           = 8 * time.Second
	openAIOAuth429MaxAccountAttempts      = 3
	openAIStopSchedulingBridgeCooldown    = 2 * time.Minute
	openAIOAuth429StormWindow             = 10 * time.Second
	openAIOAuth429StormMaxAccountSwitches = 1

	// nvidiaResourceExhaustedCooldown 是 NVIDIA 免费 API Worker 耗尽后的冷却时长。
	// 48/48 Worker 饱和度通常是持续性的，2 分钟才足够等待 Worker 池排空。
	nvidiaResourceExhaustedCooldown = 2 * time.Minute
	// nvidiaUnauthorizedCooldown 是 NVIDIA 免费 API key 失效后的冷却时长。
	// API key 失效通常需要 NVIDIA 侧重新申请或人工重置，24h 才有重试意义。
	nvidiaUnauthorizedCooldown = 24 * time.Hour
	// nvidiaQuotaCooldown 是 NVIDIA 免费 API 日/周期配额耗尽后的冷却时长。
	// 配额耗尽（"quota"/"daily"/"monthly" 语义 429）在额度重置前重试无意义，
	// 30 分钟足够等待多数周期重置，同时避免高频轮询被风控升级。
	nvidiaQuotaCooldown = 30 * time.Minute
)

// OpenAIOAuth429FailoverState tracks the request-local follow-up budget after
// the first Grok OAuth 429. Once that 429 occurs, exactly one different account
// may be attempted; any failure from that follow-up account ends failover.
type OpenAIOAuth429FailoverState struct {
	grokOAuth429FollowupPending bool
}

type openAIOAuth429Disposition uint8

const (
	openAIOAuth429Transient openAIOAuth429Disposition = iota
	openAIOAuth429Quota5h
	openAIOAuth429Quota7d
	openAIOAuth429QuotaReset
)

// classifyOpenAIOAuth429 区分账号配额耗尽信号与普通瞬时 429。明确窗口达到
// 100% 时以该窗口为准；没有 100% 标记但包含重置头时，沿用 v179 的兼容语义，
// 仍视为配额限流信号。
func classifyOpenAIOAuth429(headers http.Header, responseBody []byte) (openAIOAuth429Disposition, *time.Time) {
	if snapshot := ParseCodexRateLimitHeaders(headers); snapshot != nil {
		if normalized := snapshot.Normalize(); normalized != nil {
			if normalized.Used7dPercent != nil && *normalized.Used7dPercent >= 100 {
				if normalized.Reset7dSeconds != nil {
					now := time.Now()
					resetAt := now.Add(time.Duration(*normalized.Reset7dSeconds) * time.Second)
					return openAIOAuth429Quota7d, &resetAt
				}
				return openAIOAuth429Quota7d, nil
			}
			if normalized.Used5hPercent != nil && *normalized.Used5hPercent >= 100 {
				if normalized.Reset5hSeconds != nil {
					now := time.Now()
					resetAt := now.Add(time.Duration(*normalized.Reset5hSeconds) * time.Second)
					return openAIOAuth429Quota5h, &resetAt
				}
				return openAIOAuth429Quota5h, nil
			}
		}
	}
	if resetAt := calculateOpenAI429ResetTime(headers); resetAt != nil {
		return openAIOAuth429QuotaReset, resetAt
	}
	if resetUnix := parseOpenAIRateLimitResetTime(responseBody); resetUnix != nil {
		resetAt := time.Unix(*resetUnix, 0)
		return openAIOAuth429QuotaReset, &resetAt
	}
	return openAIOAuth429Transient, nil
}

func openAIAccountStateContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(base, openAIAccountStateUpdateTimeout)
}

func isOpenAIOAuthAccount(account *Account) bool {
	return account != nil && account.IsOpenAIOAuthLike()
}

func isGrokOAuthAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformGrok && account.Type == AccountTypeOAuth
}

func isOpenAIAccount(account *Account) bool {
	return account != nil && (account.Platform == PlatformOpenAI || account.Platform == PlatformGrok)
}

// isNVIDIAResourceExhaustedError 检测 NVIDIA 免费 API 的容量耗尽错误。
// 命中两种模式之一即认为真：
//   - ResourceExhausted + "Worker local total request limit"（Worker 池饱和）
//   - 503 + capacity（容量不足）
//
// 后者是兜底通道，确保当 nvidia_adaptive_throttle_enabled 关闭时也能快速冷切，
// 不会落到 generic 1min 基础 cooldown。
func isNVIDIAResourceExhaustedError(statusCode int, body []byte) bool {
	if statusCode != http.StatusTooManyRequests && statusCode != http.StatusServiceUnavailable {
		return false
	}
	if len(body) == 0 {
		return false
	}
	lower := bytes.ToLower(body)
	if bytes.Contains(lower, []byte("resourceexhausted")) &&
		bytes.Contains(lower, []byte("worker local total request limit")) {
		return true
	}
	if statusCode == http.StatusServiceUnavailable && bytes.Contains(lower, []byte("capacity")) {
		return true
	}
	return false
}

// isNVIDIAUnauthorizedError 检测 NVIDIA 免费 API key 失效错误。
// 命中后送 24h BlockAccountScheduling 冷却池，避免 generic 1min cooldown 反复轮到失效 key。
// 关键字选择保守——必须显式出现 "unauthorized" 或 "invalid_api_key" 子串，避免误判下游 5xx。
func isNVIDIAUnauthorizedError(statusCode int, body []byte) bool {
	if statusCode != http.StatusUnauthorized {
		return false
	}
	if len(body) == 0 {
		return false
	}
	lower := bytes.ToLower(body)
	if bytes.Contains(lower, []byte("unauthorized")) {
		return true
	}
	if bytes.Contains(lower, []byte("invalid_api_key")) {
		return true
	}
	return false
}

// handleOpenAIAccountUpstreamError expects canonicalModel to be the model used
// for scheduling after applying account mapping exactly once.
func (s *OpenAIGatewayService) handleOpenAIAccountUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte, canonicalModel ...string) bool {
	if account != nil && account.Platform == PlatformGrok && isGrokContentPolicyRejection(statusCode, responseBody) {
		return false
	}
	// Any non-2xx upstream HTTP response means the model request was actually sent.
	if s != nil {
		scheduleOllamaCloudUsageActivity(s.deferredService, account)
	}
	// Capacity shedding describes this request, not account health. Keep the
	// account schedulable while the request-local retry budget handles recovery.
	if account != nil && account.Platform == PlatformOpenAI && isOpenAIRequestScopedCapacityShed("", responseBody) {
		return false
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	if account != nil && account.Platform == PlatformOpenAI && isOpenAIHTTPUpstreamAccessStateError(statusCode, "", responseBody) {
		message := "OpenAI upstream account or workspace is unavailable"
		if upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(responseBody)); upstreamMsg != "" {
			message = upstreamMsg
		}
		if s != nil && s.rateLimitService != nil {
			s.rateLimitService.handleAuthError(stateCtx, account, message)
		}
		if s != nil {
			s.BlockAccountScheduling(account, time.Time{}, "openai_access_state")
		}
		return true
	}

	if account != nil && account.Platform == PlatformOpenAI && isOpenAIContextWindowError("", responseBody) {
		return false
	}

	if isOpenAIImageRateLimitError(statusCode, responseBody) {
		if s != nil && s.rateLimitService != nil {
			_ = s.rateLimitService.HandleOpenAIImageRateLimit(stateCtx, account, statusCode, headers, responseBody)
		}
		return false
	}

	if s == nil || account == nil {
		return false
	}
	// Team 联动熔断必须先于 model-not-found 与账户级临时不可调度规则的早退。
	if s.rateLimitService != nil {
		s.rateLimitService.maybeHandleOpenAITeamLinkedError(stateCtx, account, statusCode, responseBody)
	}

	// NVIDIA 免费 API 配额耗尽型 429（quota/daily/monthly 语义）：在进入自适应节流（1s+Retry-After 短退避）
	// 之前先行判定，直接送 30min 长冷却并阻止 failover 扩散——额度重置前重试无意义，高频轮询反而升级风控。
	// 仅 NVIDIA API-Key 账号生效：quota 关键词较通用，非 NVIDIA 上游的同类 429 不应被误封 30min。
	if account.Type == AccountTypeAPIKey && isNVIDIAAccountByHostname(account) &&
		statusCode == http.StatusTooManyRequests && classifyNVIDIA429Body(responseBody) == nvidia429Quota {
		recordNVIDIAUpstreamFailure(s, ctx, account, statusCode, responseBody, nvidiaReasonQuotaExhausted)
		s.BlockAccountScheduling(account, time.Now().Add(nvidiaQuotaCooldown), "nvidia_quota_exhausted")
		return true
	}

	// NVIDIA 自适应节流：在进入旧有的 isNVIDIAResourceExhaustedError 全账户分支之前，
	// 将上游结果记录到自适应节流缓存中。命中被处理的信号（429 / 503 capacity）时，
	// 立即判定 shouldDisable=true 并提前返回，不再执行下方的全账户封锁策略。
	if s.rateLimitService != nil {
		update := NvidiaThrottleUpdate{
			Scope: NVIDIAThrottleScope{
				AccountID:      account.ID,
				CanonicalModel: firstCanonical(canonicalModel),
			},
			StatusCode:   statusCode,
			Headers:      headers,
			ResponseBody: responseBody,
		}
		if handled := s.rateLimitService.RecordNVIDIAAdaptiveThrottleOutcome(stateCtx, update); handled {
			// F1: throttle cache 命中并提前返回时，仍须把失败计入 N3 recentFailCount，
			// 否则持续被节流的高频失败账号反而不会被 nvidiaReuseHeatFor 降权，
			// 调度器继续 hot-list 已经被节流的账号。
			if statusCode == http.StatusTooManyRequests || statusCode == http.StatusServiceUnavailable {
				recordNVIDIAUpstreamFailure(s, ctx, account, statusCode, responseBody, nvidiaReasonThrottleHandled)
				return true
			}
		}
	}

	// NVIDIA 免费 API key 失效（401）：送 24h 长 cooldown 防止反复轮询失效账号。
	// 需在 isNVIDIAResourceExhaustedError 之前，因为 unauthorized 不可逆且不应等 2min 重试。
	if account.Type == AccountTypeAPIKey && isNVIDIAUnauthorizedError(statusCode, responseBody) {
		recordNVIDIAUpstreamFailure(s, ctx, account, statusCode, responseBody, nvidiaReasonUnauthorized)
		s.BlockAccountScheduling(account, time.Now().Add(nvidiaUnauthorizedCooldown), "nvidia_unauthorized")
		return true
	}

	// NVIDIA 免费 API 的 Worker 池耗尽 (ResourceExhausted)：强制冷却账号并阻止 failover 扩散。
	// 如果 temp_unschedulable_rules 已独立匹配，此分支作为补充保障路径，确保账号被立即内存封锁。
	if account.Type == AccountTypeAPIKey && isNVIDIAResourceExhaustedError(statusCode, responseBody) {
		recordNVIDIAUpstreamFailure(s, ctx, account, statusCode, responseBody, nvidiaReasonResourceExhausted)
		s.BlockAccountScheduling(account, time.Now().Add(nvidiaResourceExhaustedCooldown), "nvidia_resource_exhausted")
		if s.rateLimitService != nil {
			_ = s.rateLimitService.HandleUpstreamError(stateCtx, account, statusCode, headers, responseBody)
		}
		return true
	}

	// N1: NVIDIA 5xx（500/502/504/520-524）走 generic 路径之前先统一脱敏日志，
	// 让运维 grep `nvidia_upstream_error` 可覆盖 NVIDIA 全部上游错误而非仅 401/429/503。
	// 400 单独由 N2 降级重试路径处理。
	if statusCode >= 500 && isNVIDIAAccountByHostname(account) {
		recordNVIDIAUpstreamFailure(s, ctx, account, statusCode, responseBody, nvidiaReason5xxUpstream)
		recordNVIDIASingle5xxHeatPenalty(s, account, statusCode)

		// P1: 连续 5xx 自动冷却 — 触发时调用 BlockAccountScheduling
		// 短期排除坏账号，冷却后自松。阈值/窗口/冷却时间从 admin settings 热读。
		if metrics := s.nvidiaSharedPoolMetricsForSorting.Load(); metrics != nil {
			s.maybeCoolNVIDIAOnConsecutive5xx(ctx, account, metrics)
		}
	}
	stateCtx = withTempUnschedulableModel(stateCtx, canonicalModel)
	if s.rateLimitService != nil && len(canonicalModel) > 0 && s.rateLimitService.HandleUpstreamModelNotFound(stateCtx, account, canonicalModel[0], statusCode, responseBody) {
		return true
	}
	// Isolate a custom temporary-unschedulable match to the known upstream
	// model before entering the generic account error path. This keeps the
	// account available to other models and avoids the account runtime blocker.
	if s.rateLimitService != nil && statusCode != http.StatusUnauthorized && len(canonicalModel) > 0 && strings.TrimSpace(canonicalModel[0]) != "" &&
		s.rateLimitService.HandleTempUnschedulable(stateCtx, account, statusCode, responseBody, canonicalModel[0]) {
		return true
	}
	if statusCode == http.StatusTooManyRequests {
		s.markOpenAIOAuth429RateLimited(stateCtx, account, headers, responseBody)
	}
	if s.rateLimitService == nil {
		return false
	}
	shouldDisable := s.rateLimitService.HandleUpstreamError(stateCtx, account, statusCode, headers, responseBody)
	modelTempMatched := statusCode != http.StatusUnauthorized && tempUnschedulableModel(stateCtx, nil) != "" &&
		len(matchTempUnschedulableRules(account, statusCode, responseBody)) > 0
	if shouldDisable && !modelTempMatched {
		s.BlockAccountScheduling(account, time.Time{}, "upstream_disable")
	}
	// Pool-mode retryable upstream errors are already bounded by the request-local
	// same-account retry budget. Recording the generic account+model transient
	// cooldown here would block the next approved retry before that budget is used.
	poolModeRetryable := account.IsPoolMode() && account.IsPoolModeRetryableStatus(statusCode)
	if !shouldDisable && account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey &&
		shouldCooldownOpenAITransientUpstreamError(statusCode, responseBody) && !poolModeRetryable {
		model := ""
		if len(canonicalModel) > 0 {
			model = canonicalModel[0]
		}
		decision := s.recordOpenAIAccountModelTransientFailure(account, model, time.Now())
		if decision.FailureStreak > 0 {
			slog.Warn("openai_model_transient_state",
				"account_id", account.ID,
				"model", openAIAccountModelTransientModel(model),
				"failure_streak", decision.FailureStreak,
				"cooldown_ms", decision.Cooldown.Milliseconds(),
				"block_scope", "account_model",
			)
		}
	}
	return shouldDisable
}

func shouldCooldownOpenAITransientUpstreamError(statusCode int, responseBody []byte) bool {
	switch statusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 520, 521, 522, 523, 524:
		return true
	case http.StatusBadRequest:
		return isOpenAITransientProcessingError(statusCode, "", responseBody)
	default:
		return false
	}
}

func (s *OpenAIGatewayService) markOpenAIOAuth429RateLimited(ctx context.Context, account *Account, headers http.Header, responseBody []byte) {
	if s == nil || !isOpenAIOAuthAccount(account) {
		return
	}
	// Spark 影子：不按 /responses 429 的 global x-codex-* 信号做内存运行时熔断(同 handle429,外审第8轮 P1)。
	// 同时避免把 spark 的 429 计入全局 429 storm 计数(recordOpenAIOAuth429),否则会误伤母账号 failover 决策。
	if account.IsShadow() {
		return
	}
	s.recordOpenAIOAuth429()
	disposition, resetAt := classifyOpenAIOAuth429(headers, responseBody)
	if disposition == openAIOAuth429Transient && s.openAIOAuth429RetryWindowActive(account) {
		return
	}

	cooldownUntil := time.Now().Add(openAIOAuth429FallbackCooldown)
	if resetAt != nil && resetAt.After(time.Now()) {
		cooldownUntil = *resetAt
	} else if s.rateLimitService != nil {
		if cooldown, ok := s.rateLimitService.get429FallbackCooldown(ctx, account); ok && cooldown > 0 {
			cooldownUntil = time.Now().Add(cooldown)
		}
	}
	s.BlockAccountScheduling(account, cooldownUntil, "429")
	s.openaiOAuth429RetryStartedAt.Delete(account.ID)
}

func (s *OpenAIGatewayService) shouldRetryOpenAIOAuth429OnSameAccount(account *Account, statusCode int, shouldDisable bool) bool {
	return s.shouldRetryOpenAIOAuth429OnSameAccountWithResponse(account, statusCode, shouldDisable, nil, nil)
}

func (s *OpenAIGatewayService) shouldRetryOpenAIOAuth429OnSameAccountWithResponse(account *Account, statusCode int, shouldDisable bool, headers http.Header, responseBody []byte) bool {
	if shouldDisable || statusCode != http.StatusTooManyRequests || !isOpenAIOAuthAccount(account) || account.IsShadow() {
		return false
	}
	disposition, _ := classifyOpenAIOAuth429(headers, responseBody)
	if disposition != openAIOAuth429Transient {
		return false
	}
	// markOpenAIOAuth429RateLimited parks the account once the window expires.
	// Do not accidentally create a fresh window after that transition.
	if s.isOpenAIAccountRuntimeBlocked(account) {
		return false
	}
	return s.openAIOAuth429RetryWindowActive(account)
}

// ShouldRetryOpenAIOAuth429 lets RateLimitService defer persistent account
// cooldown until the gateway's same-account retry window is exhausted.
func (s *OpenAIGatewayService) ShouldRetryOpenAIOAuth429(account *Account, headers http.Header, responseBody []byte) bool {
	if s == nil || !isOpenAIOAuthAccount(account) || account.IsShadow() || s.isOpenAIAccountRuntimeBlocked(account) {
		return false
	}
	disposition, _ := classifyOpenAIOAuth429(headers, responseBody)
	if disposition != openAIOAuth429Transient {
		return false
	}
	return s.openAIOAuth429RetryWindowActive(account)
}

func (s *OpenAIGatewayService) openAIOAuth429RetryWindowActive(account *Account) bool {
	if s == nil || !isOpenAIOAuthAccount(account) || account.IsShadow() {
		return false
	}
	now := time.Now()
	value, _ := s.openaiOAuth429RetryStartedAt.LoadOrStore(account.ID, now)
	startedAt, ok := value.(time.Time)
	if !ok {
		s.openaiOAuth429RetryStartedAt.Store(account.ID, now)
		startedAt = now
	}
	return now.Before(startedAt.Add(openAIOAuth429RetryWindow))
}

func (s *OpenAIGatewayService) openAIOAuth429RetryDeadline(account *Account) time.Time {
	if s == nil || !isOpenAIOAuthAccount(account) || account.IsShadow() {
		return time.Time{}
	}
	value, ok := s.openaiOAuth429RetryStartedAt.Load(account.ID)
	if !ok {
		return time.Time{}
	}
	startedAt, ok := value.(time.Time)
	if !ok {
		return time.Time{}
	}
	return startedAt.Add(openAIOAuth429RetryWindow)
}

func openAIOAuth429SameAccountRetryDelay(headers http.Header, deadline time.Time) time.Duration {
	delay := openAIOAuth429RetryDelay
	now := time.Now()
	if resetAt := parseRetryAfterResetTime(headers, now); resetAt != nil && resetAt.After(now) {
		delay = resetAt.Sub(now)
	}
	if delay > openAIOAuth429MaxRetryDelay {
		delay = openAIOAuth429MaxRetryDelay
	}
	if remaining := time.Until(deadline); !deadline.IsZero() && delay > remaining {
		delay = remaining
	}
	if delay < 0 {
		return 0
	}
	return delay
}

func (s *OpenAIGatewayService) BlockAccountScheduling(account *Account, until time.Time, reason string) {
	if s == nil || !isOpenAIAccount(account) {
		return
	}
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	defer mu.Unlock()
	_, _ = s.blockAccountSchedulingLocked(account, until, reason)
}

func (s *OpenAIGatewayService) openAIAccountRuntimeBlockLock(accountID int64) *sync.Mutex {
	actual, _ := s.openaiAccountRuntimeBlockLocks.LoadOrStore(accountID, &sync.Mutex{})
	mu, ok := actual.(*sync.Mutex)
	if !ok {
		mu = &sync.Mutex{}
		s.openaiAccountRuntimeBlockLocks.Store(accountID, mu)
	}
	return mu
}

func (s *OpenAIGatewayService) blockAccountSchedulingLocked(account *Account, until time.Time, _ string) (uint64, bool) {
	generation := s.openaiAccountRuntimeBlockSequence.Add(1)
	s.openaiAccountRuntimeBlockGeneration.Store(account.ID, generation)
	now := time.Now()
	blockUntil := until
	if blockUntil.IsZero() || !blockUntil.After(now) {
		blockUntil = now.Add(openAIStopSchedulingBridgeCooldown)
	}

	for {
		current, loaded := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
		if !loaded {
			actual, stored := s.openaiAccountRuntimeBlockUntil.LoadOrStore(account.ID, blockUntil)
			if !stored {
				return generation, true
			}
			current = actual
		}

		currentUntil, ok := current.(time.Time)
		if !ok || currentUntil.IsZero() {
			if s.openaiAccountRuntimeBlockUntil.CompareAndSwap(account.ID, current, blockUntil) {
				return generation, true
			}
			continue
		}
		if !blockUntil.After(currentUntil) {
			return generation, false
		}
		if s.openaiAccountRuntimeBlockUntil.CompareAndSwap(account.ID, current, blockUntil) {
			return generation, true
		}
	}
}

func (s *OpenAIGatewayService) ClearAccountSchedulingBlock(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	mu := s.openAIAccountRuntimeBlockLock(accountID)
	mu.Lock()
	defer mu.Unlock()
	s.openaiAccountRuntimeBlockUntil.Delete(accountID)
	s.openaiOAuth429RetryStartedAt.Delete(accountID)
	s.openaiAccountRuntimeBlockGeneration.Store(accountID, s.openaiAccountRuntimeBlockSequence.Add(1))
}

func (s *OpenAIGatewayService) isOpenAIAccountRuntimeBlocked(account *Account) bool {
	if s == nil || !isOpenAIAccount(account) {
		return false
	}
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	defer mu.Unlock()
	value, ok := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	if !ok {
		return false
	}
	cooldownUntil, ok := value.(time.Time)
	if !ok || cooldownUntil.IsZero() {
		s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
		s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
		return false
	}
	if time.Now().Before(cooldownUntil) {
		return true
	}
	s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
	s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
	return false
}

// IsNvidiaPrewarmBlocked 报告账号当前是否处于运行时封锁（ID 版，供预热器过滤）。
// 语义与 isOpenAIAccountRuntimeBlocked 的封锁判定一致，但只按 ID 查询，
// 不依赖 account 平台类型——调用方（NVIDIAInferencePrewarmer.collectAccounts）
// 已经保证传入的是 NVIDIA 候选账号。
func (s *OpenAIGatewayService) IsNvidiaPrewarmBlocked(accountID int64) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	mu := s.openAIAccountRuntimeBlockLock(accountID)
	mu.Lock()
	defer mu.Unlock()
	value, ok := s.openaiAccountRuntimeBlockUntil.Load(accountID)
	if !ok {
		return false
	}
	cooldownUntil, ok := value.(time.Time)
	if !ok || cooldownUntil.IsZero() {
		s.openaiAccountRuntimeBlockUntil.Delete(accountID)
		s.openaiAccountRuntimeBlockGeneration.Store(accountID, s.openaiAccountRuntimeBlockSequence.Add(1))
		return false
	}
	if time.Now().Before(cooldownUntil) {
		return true
	}
	s.openaiAccountRuntimeBlockUntil.Delete(accountID)
	s.openaiAccountRuntimeBlockGeneration.Store(accountID, s.openaiAccountRuntimeBlockSequence.Add(1))
	return false
}

func (s *OpenAIGatewayService) getOpenAIAccountModelTransientState() *openAIAccountModelTransientState {
	if s == nil {
		return nil
	}
	s.openaiModelTransientOnce.Do(func() {
		if s.openaiModelTransient == nil {
			s.openaiModelTransient = newOpenAIAccountModelTransientState(openAIModelTransientDefaultMax)
		}
	})
	return s.openaiModelTransient
}

func firstCanonical(canonicalModel []string) string {
	if len(canonicalModel) > 0 {
		return canonicalModel[0]
	}
	return ""
}

func canonicalOpenAIAccountSchedulingModel(account *Account, requestedModel string) string {
	model := strings.TrimSpace(requestedModel)
	if account == nil || model == "" {
		return model
	}
	if account.IsOpenAI() {
		return resolveOpenAIAccountUpstreamModelForRequest(account, model, false)
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(model)); mapped != "" {
		return mapped
	}
	return model
}

func openAIAccountModelTransientModel(canonicalModel string) string {
	return normalizeOpenAIAccountModelTransientModel(canonicalModel)
}

func (s *OpenAIGatewayService) recordOpenAIAccountModelTransientFailure(account *Account, canonicalModel string, now time.Time) openAIAccountModelTransientDecision {
	if s == nil || account == nil {
		return openAIAccountModelTransientDecision{}
	}
	state := s.getOpenAIAccountModelTransientState()
	if state == nil {
		return openAIAccountModelTransientDecision{}
	}
	return state.recordFailure(account.ID, openAIAccountModelTransientModel(canonicalModel), now)
}

func (s *OpenAIGatewayService) clearOpenAIAccountModelTransientState(accountID int64, model string) {
	state := s.getOpenAIAccountModelTransientState()
	if state == nil {
		return
	}
	state.recordSuccess(accountID, model)
}

func (s *OpenAIGatewayService) isOpenAIAccountModelRuntimeBlocked(account *Account, requestedModel string) bool {
	if s == nil || account == nil {
		return false
	}
	state := s.getOpenAIAccountModelTransientState()
	if state == nil {
		return false
	}
	canonicalModel := canonicalOpenAIAccountSchedulingModel(account, requestedModel)
	return state.isBlocked(account.ID, openAIAccountModelTransientModel(canonicalModel), time.Now())
}

func (s *OpenAIGatewayService) isOpenAIAccountRequestRuntimeBlocked(account *Account, requestedModel string) bool {
	return s != nil && (s.isOpenAIAccountRuntimeBlocked(account) || s.isOpenAIAccountModelRuntimeBlocked(account, requestedModel))
}

func (s *OpenAIGatewayService) recordOpenAIOAuth429() {
	if s == nil {
		return
	}
	now := time.Now()
	windowStart := s.openaiOAuth429WindowStartUnixNano.Load()
	if windowStart == 0 || now.Sub(time.Unix(0, windowStart)) >= openAIOAuth429StormWindow {
		if s.openaiOAuth429WindowStartUnixNano.CompareAndSwap(windowStart, now.UnixNano()) {
			s.openaiOAuth429WindowCount.Store(1)
			return
		}
	}
	s.openaiOAuth429WindowCount.Add(1)
}

func (s *OpenAIGatewayService) ShouldStopOpenAIOAuth429Failover(account *Account, statusCode int, failedSwitches int, state *OpenAIOAuth429FailoverState) bool {
	if failedSwitches < openAIOAuth429StormMaxAccountSwitches {
		return false
	}
	if state != nil && state.grokOAuth429FollowupPending {
		// The follow-up budget was armed by a Grok OAuth 429. Consume it on
		// any failing follow-up account, even if a mixed pool selected an API-key
		// account next.
		return true
	}
	if isGrokOAuthAccount(account) {
		if state == nil {
			// Preserve the old threshold for callers that have not adopted the
			// request-local state contract yet.
			return statusCode == http.StatusTooManyRequests && failedSwitches >= 2
		}
		if statusCode == http.StatusTooManyRequests {
			state.grokOAuth429FollowupPending = true
		}
		return false
	}
	if statusCode != http.StatusTooManyRequests || !isOpenAIOAuthAccount(account) {
		return false
	}
	// Each OpenAI OAuth candidate has already consumed its full same-account
	// retry window before reaching this switch point. A global storm is useful
	// telemetry, but must not prevent trying the bounded next-account budget.
	return failedSwitches >= openAIOAuth429MaxAccountAttempts
}

// maybeCoolNVIDIAOnConsecutive5xx 是 P1 连续 5xx 自动冷却的判定入口。
//
//  1. 从 admin settings（热读 DB，不重启）取出阈值、时间窗、冷却时长。
//  2. 调用 RecordConsecutive5xx 累积同一账号的 5xx 失败 streak。
//  3. 当 streak 数达到阈值且首末时间差仍在 window 内，将账号 BlockAccountScheduling
//     冷却 cooldown 秒，到期后 scheduler 重新释放。
//  4. 一次成功 RecordAccountReuse 即清零 streak，避免健康账号被持久封堵。
//
// 此方法仅在 fastpath N1 块（statusCode >= 500 && isNVIDIAAccountByHostname）调用，
// 对整个 failover 链无副作用。
func (s *OpenAIGatewayService) maybeCoolNVIDIAOnConsecutive5xx(
	ctx context.Context,
	account *Account,
	metrics *NVIDIASharedConnectionPoolMetrics,
) {
	if s == nil || account == nil || metrics == nil {
		return
	}
	if s.settingService == nil {
		return
	}

	settings, err := s.settingService.GetAllSettings(ctx)
	if err != nil {
		return // fail-to-read settings → skip cooldown safely
	}

	threshold := settings.NVIDIAConsecutive5xxThreshold
	windowSeconds := settings.NVIDIAConsecutive5xxWindowSeconds
	cooldownSeconds := settings.NVIDIAConsecutive5xxCooldownSeconds

	// Guard: reasonable defaults if DB read falls through to zero (shouldn't
	// happen because parseSettings clamps, but be defensive).
	if threshold <= 0 {
		threshold = 3
	}
	if windowSeconds <= 0 {
		windowSeconds = 60
	}
	if cooldownSeconds <= 0 {
		cooldownSeconds = 120
	}

	reached := metrics.RecordConsecutive5xx(
		account.ID,
		time.Now(),
		threshold,
		time.Duration(windowSeconds)*time.Second,
	)
	if !reached {
		return
	}

	s.BlockAccountScheduling(
		account,
		time.Now().Add(time.Duration(cooldownSeconds)*time.Second),
		"nvidia_consecutive_5xx_cooldown",
	)
	slog.Warn("nvidia_account_cooled",
		"account_id", account.ID,
		"threshold", threshold,
		"window_sec", windowSeconds,
		"cooldown_sec", cooldownSeconds,
	)
}
