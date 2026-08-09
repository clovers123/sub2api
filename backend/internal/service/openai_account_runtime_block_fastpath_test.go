//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type oauth429RateLimitRepo struct {
	mockAccountRepoForGemini
	setRateLimitedCalls  int
	lastRateLimitedUntil time.Time
}

func (r *oauth429RateLimitRepo) SetRateLimited(_ context.Context, _ int64, until time.Time) error {
	r.setRateLimitedCalls++
	r.lastRateLimitedUntil = until
	return nil
}

func TestOpenAI429FastPath_KeepsOAuthAccountSchedulableDuringRetryWindow(t *testing.T) {
	repo := &oauth429RateLimitRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	setupTokenAccount := &Account{ID: 44, Platform: PlatformOpenAI, Type: AccountTypeSetupToken}
	grokOAuthAccount := &Account{ID: 45, Platform: PlatformGrok, Type: AccountTypeOAuth}

	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, nil)
	apiKeyShouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), apiKeyAccount, http.StatusTooManyRequests, http.Header{}, nil)

	require.False(t, shouldDisable)
	require.False(t, apiKeyShouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(apiKeyAccount), "API-key 429 keeps the existing scheduler cooldown behavior")
	require.Equal(t, 1, repo.setRateLimitedCalls, "only the API-key 429 should persist a scheduler block")
	require.True(t, svc.shouldRetryOpenAIOAuth429OnSameAccount(account, http.StatusTooManyRequests, false))
	require.True(t, svc.shouldRetryOpenAIOAuth429OnSameAccount(setupTokenAccount, http.StatusTooManyRequests, false))
	require.False(t, svc.shouldRetryOpenAIOAuth429OnSameAccount(apiKeyAccount, http.StatusTooManyRequests, false))
	require.False(t, svc.shouldRetryOpenAIOAuth429OnSameAccount(grokOAuthAccount, http.StatusTooManyRequests, false))
	require.WithinDuration(t, time.Now().Add(openAIOAuth429RetryWindow), svc.openAIOAuth429RetryDeadline(account), time.Second)
	require.WithinDuration(t, time.Now().Add(openAIOAuth429RetryWindow), svc.openAIOAuth429RetryDeadline(setupTokenAccount), time.Second)
}

func TestOpenAI429FastPath_BlocksOAuthOnlyAfterRetryWindow(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 420, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc.openaiOAuth429RetryStartedAt.Store(account.ID, time.Now().Add(-openAIOAuth429RetryWindow-time.Second))

	svc.markOpenAIOAuth429RateLimited(context.Background(), account, http.Header{}, nil)

	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.False(t, svc.shouldRetryOpenAIOAuth429OnSameAccount(account, http.StatusTooManyRequests, false))
}

func TestOpenAI429FastPath_BlocksOAuthImmediatelyWhenSevenDayQuotaIsExhausted(t *testing.T) {
	repo := &oauth429RateLimitRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 423, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", "20")
	headers.Set("x-codex-secondary-reset-after-seconds", "3600")
	headers.Set("x-codex-secondary-window-minutes", "300")

	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, headers, []byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`))

	require.False(t, shouldDisable)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Equal(t, 1, repo.setRateLimitedCalls)
	require.Greater(t, time.Until(repo.lastRateLimitedUntil), 6*24*time.Hour)
	require.False(t, svc.ShouldRetryOpenAIOAuth429(account, headers, nil))
}

func TestOpenAI429FastPath_RetriesOAuthWhenNoQuotaSignalExists(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 424, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	headers := http.Header{"Retry-After": []string{"1"}}

	require.True(t, svc.ShouldRetryOpenAIOAuth429(account, headers, []byte(`{"error":{"type":"rate_limit_error","message":"try again"}}`)))
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIStream429IgnoresSuccessfulQuotaSnapshotHeaders(t *testing.T) {
	repo := &oauth429RateLimitRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 421, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc.openaiOAuth429RetryStartedAt.Store(account.ID, time.Now().Add(-openAIOAuth429RetryWindow-time.Second))
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "37")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	payload := []byte(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`)

	status, disabled := svc.handleOpenAIStreamTerminalAccountSideEffects(nil, account, payload, "slow down", headers)

	require.Equal(t, http.StatusTooManyRequests, status)
	require.False(t, disabled)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	blockedUntil, ok := value.(time.Time)
	require.True(t, ok)
	require.Less(t, time.Until(blockedUntil), time.Minute, "stream 429 must not inherit the normal seven-day quota snapshot")
	if !repo.lastRateLimitedUntil.IsZero() {
		require.Less(t, time.Until(repo.lastRateLimitedUntil), time.Minute)
	}
}

func TestOpenAIHTTP429StillUsesQuotaResetHeaders(t *testing.T) {
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{}}
	account := &Account{ID: 422, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc.openaiOAuth429RetryStartedAt.Store(account.ID, time.Now().Add(-openAIOAuth429RetryWindow-time.Second))
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "37")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")

	svc.markOpenAIOAuth429RateLimited(context.Background(), account, headers, nil)

	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	blockedUntil, ok := value.(time.Time)
	require.True(t, ok)
	require.Greater(t, time.Until(blockedUntil), 6*24*time.Hour, "real HTTP 429 must retain the upstream quota reset")
}

func TestOpenAI429RetryDelayHonorsBoundedRetryAfter(t *testing.T) {
	deadline := time.Now().Add(openAIOAuth429RetryWindow)
	require.Equal(t, openAIOAuth429RetryDelay, openAIOAuth429SameAccountRetryDelay(nil, deadline))
	require.Equal(t, openAIOAuth429MaxRetryDelay, openAIOAuth429SameAccountRetryDelay(http.Header{"Retry-After": []string{"90"}}, deadline))
}

func TestOpenAI429FastPath_OpenCodeGoUsageLimitUsesMessageResetDuration(t *testing.T) {
	repo := &rateLimit429AccountRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimitService}
	rateLimitService.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 44, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	body := []byte(`{"type":"error","error":{"type":"GoUsageLimitError","message":"5-hour usage limit reached. Resets in 4hr 59min. To continue using this model now, enable usage from your available balance: https://opencode.ai/workspace/wrk_test/go"},"metadata":{"workspace":"wrk_test","limitName":"5 hour"}}`)

	before := time.Now()
	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		body,
	)
	after := time.Now()

	require.False(t, shouldDisable)
	require.Equal(t, 1, repo.rateLimitCalls)
	require.Equal(t, account.ID, repo.lastRateLimitID)
	expectedResetAfter := 4*time.Hour + 59*time.Minute
	require.False(t, repo.lastRateLimitReset.Before(before.Add(expectedResetAfter-time.Second)))
	require.False(t, repo.lastRateLimitReset.After(after.Add(expectedResetAfter)))
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

// TestOpenAI429FastPath_SkipsSparkShadow 外审第8轮 P1:spark 影子被选中后若 /responses 返回 429,
// 不得按 global x-codex-* 信号写内存运行时熔断(否则 spark 被冷却到 global reset、单影子场景无可用账号)。
func TestOpenAI429FastPath_SkipsSparkShadow(t *testing.T) {
	svc := &OpenAIGatewayService{}
	parentID := int64(800)
	shadow := &Account{
		ID:              801,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
	}
	normal := &Account{ID: 802, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "18000")
	headers.Set("x-codex-primary-window-minutes", "300")

	svc.markOpenAIOAuth429RateLimited(context.Background(), shadow, headers, nil)
	svc.markOpenAIOAuth429RateLimited(context.Background(), normal, headers, nil)

	require.False(t, svc.isOpenAIAccountRuntimeBlocked(shadow), "spark shadow must not be runtime-blocked by /responses global 429")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(normal), "normal OpenAI OAuth account with an exhausted 5h window must be paused")
}

func TestOpenAIRuntimeBlock_AppliesToOpenAIAPIKeyWhenRateLimitServiceStopsScheduling(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 44, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	svc.BlockAccountScheduling(account, time.Time{}, "custom_error_code")

	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIRuntimeBlock_DoesNotApplyToOtherPlatforms(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 45, Platform: PlatformGemini, Type: AccountTypeOAuth}

	svc.BlockAccountScheduling(account, time.Time{}, "custom_error_code")

	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIRuntimeBlocker_IgnoresNonOpenAIFromRateLimitService(t *testing.T) {
	gateway := &OpenAIGatewayService{}
	repo := &rateLimitAccountRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	rateLimitService.SetAccountRuntimeBlocker(gateway)
	account := &Account{ID: 45, Platform: PlatformGemini, Type: AccountTypeOAuth}

	shouldDisable := rateLimitService.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, []byte("forbidden"))

	require.True(t, shouldDisable)
	require.False(t, gateway.isOpenAIAccountRuntimeBlocked(account))
}

// 自 #4547（issue 4527 第4点）起，临时不可调度规则命中已知模型时按模型隔离：
// 只封 (账号, 模型) 对，不再账号级一刀切；未知模型仍走账号级兜底
// （见 TestOpenAITempUnschedulable_UnknownModelKeepsAccountRuntimeBlock）。
// 池模式规则仍然生效（issue 4470）：停止同账号重试并对命中模型设临时封锁。
func TestOpenAIPoolModeTempRule_StopsSameAccountRetryAndIsolatesBlockToModel(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	gateway := &OpenAIGatewayService{
		cfg:              &config.Config{},
		rateLimitService: rateLimitService,
	}
	rateLimitService.SetAccountRuntimeBlocker(gateway)
	account := &Account{
		ID:          46,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"pool_mode":                    true,
			"pool_mode_retry_status_codes": []any{float64(http.StatusServiceUnavailable)},
			"temp_unschedulable_enabled":   true,
			"temp_unschedulable_rules": []any{
				map[string]any{
					"error_code":       float64(http.StatusServiceUnavailable),
					"keywords":         []any{"unavailable"},
					"duration_minutes": float64(30),
				},
			},
		},
	}
	body := []byte(`{"error":{"message":"Service temporarily unavailable"}}`)
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
	}

	failoverErr := gateway.failoverOpenAIUpstreamHTTPError(
		context.Background(),
		nil,
		account,
		resp,
		body,
		"Service temporarily unavailable",
		"gpt-5.4",
	)

	require.NotNil(t, failoverErr)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Zero(t, repo.tempCalls)
	require.Equal(t, 0, repo.setErrCalls)
	require.Equal(t, StatusActive, account.Status)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "gpt-5.4", repo.modelRateLimitCalls[0].scope)
	require.False(t, gateway.isOpenAIAccountRuntimeBlocked(account))
	require.False(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.5"))
}

func TestOpenAIPoolModeRetryable5xx_DoesNotCreateModelTransientBlock(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	gateway := &OpenAIGatewayService{rateLimitService: rateLimitService}
	account := &Account{
		ID:       47,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                    true,
			"pool_mode_retry_status_codes": []any{float64(524)},
		},
	}

	for i := 0; i < 2; i++ {
		shouldDisable := gateway.handleOpenAIAccountUpstreamError(
			context.Background(),
			account,
			524,
			http.Header{},
			[]byte(`{"error":{"message":"upstream timeout"}}`),
			"gpt-5.4",
		)
		require.False(t, shouldDisable)
	}

	require.False(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.4"))
}

func TestOpenAIPoolModeNonRetryable5xx_StillCreatesModelTransientBlock(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	gateway := &OpenAIGatewayService{rateLimitService: rateLimitService}
	account := &Account{
		ID:       48,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                    true,
			"pool_mode_retry_status_codes": []any{float64(http.StatusGatewayTimeout)},
		},
	}

	for i := 0; i < 2; i++ {
		shouldDisable := gateway.handleOpenAIAccountUpstreamError(
			context.Background(),
			account,
			http.StatusServiceUnavailable,
			http.Header{},
			[]byte(`{"error":{"message":"upstream unavailable"}}`),
			"gpt-5.4",
		)
		require.False(t, shouldDisable)
	}

	require.True(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.4"))
}

func TestOpenAINonPoolAPIKey5xx_StillCreatesModelTransientBlock(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	gateway := &OpenAIGatewayService{rateLimitService: rateLimitService}
	account := &Account{
		ID:       49,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
	}

	for i := 0; i < 2; i++ {
		shouldDisable := gateway.handleOpenAIAccountUpstreamError(
			context.Background(),
			account,
			http.StatusGatewayTimeout,
			http.Header{},
			[]byte(`{"error":{"message":"upstream timeout"}}`),
			"gpt-5.4",
		)
		require.False(t, shouldDisable)
	}

	require.True(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.4"))
}

func TestOpenAIModelNotFound_DoesNotRuntimeBlockWholeAccount(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"code":"model_not_found","message":"model not found"}}`),
		"gpt-5.4",
	)

	require.True(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
}

func TestOpenAIModelTempUnschedulable_DoesNotRuntimeBlockWholeAccount(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"endpoint not found"}}`),
		"gpt-5.4",
	)

	require.True(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "gpt-5.4", repo.modelRateLimitCalls[0].scope)
}

func TestOpenAIModelTempUnschedulable_WriteFailureDoesNotRuntimeBlockWholeAccount(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{modelRateLimitErr: errors.New("write failed")}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"endpoint not found"}}`),
		"gpt-5.4",
	)

	require.True(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
}

func TestOpenAIOAuth429_MatchingModelTempRuleAvoidsAccountRuntimeBlock(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()
	account.Type = AccountTypeOAuth
	account.Credentials["temp_unschedulable_rules"] = []any{
		map[string]any{
			"error_code":       float64(http.StatusTooManyRequests),
			"keywords":         []any{"model quota"},
			"duration_minutes": float64(10),
		},
	}

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(`{"error":{"message":"model quota exhausted"}}`),
		"gpt-5.4",
	)

	require.True(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "gpt-5.4", repo.modelRateLimitCalls[0].scope)
}

func TestOpenAIOAuth429_NonmatchingModelTempRuleKeepsAccountRuntimeBlock(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()
	account.Type = AccountTypeOAuth
	account.Credentials["temp_unschedulable_rules"] = []any{
		map[string]any{
			"error_code":       float64(http.StatusTooManyRequests),
			"keywords":         []any{"different marker"},
			"duration_minutes": float64(10),
		},
	}

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(`{"error":{"message":"global rate limit"}}`),
		"gpt-5.4",
	)

	require.False(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.True(t, svc.shouldRetryOpenAIOAuth429OnSameAccount(account, http.StatusTooManyRequests, false))
	require.Empty(t, repo.modelRateLimitCalls)
}

func TestOpenAITempUnschedulable_UnknownModelKeepsAccountRuntimeBlock(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"endpoint not found"}}`),
	)

	require.True(t, shouldDisable)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Equal(t, 1, repo.tempCalls)
	require.Empty(t, repo.modelRateLimitCalls)
}

func TestOpenAIRuntimeBlock_DoesNotShortenExistingBlock(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 46, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	longUntil := time.Now().Add(10 * time.Minute)

	svc.BlockAccountScheduling(account, longUntil, "oauth_401")
	svc.BlockAccountScheduling(account, time.Time{}, "upstream_disable")

	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	actualUntil, ok := value.(time.Time)
	require.True(t, ok)
	require.WithinDuration(t, longUntil, actualUntil, time.Second)
}

func TestOpenAIRuntimeBlock_ClearAccountSchedulingBlock(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 47, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))

	svc.ClearAccountSchedulingBlock(account.ID)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestShouldStopOpenAIOAuth429Failover_AfterBoundedFullWindows(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	var state OpenAIOAuth429FailoverState

	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))

	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 2, &state))
	require.True(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 3, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(apiKeyAccount, http.StatusTooManyRequests, 1, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusInternalServerError, 1, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 0, &state))
}

func TestShouldStopOpenAIOAuth429Failover_TracksOneGrokFollowupAttempt(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 44, Platform: PlatformGrok, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 45, Platform: PlatformGrok, Type: AccountTypeAPIKey}

	t.Run("429 then 500 stops after one followup", func(t *testing.T) {
		var state OpenAIOAuth429FailoverState
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))
		require.True(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusInternalServerError, 2, &state))
	})

	t.Run("500 then 429 still allows one followup", func(t *testing.T) {
		var state OpenAIOAuth429FailoverState
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusInternalServerError, 1, &state))
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 2, &state))
		require.True(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusBadGateway, 3, &state))
	})

	t.Run("OAuth 429 then API-key failure consumes the same followup", func(t *testing.T) {
		var state OpenAIOAuth429FailoverState
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))
		require.True(t, svc.ShouldStopOpenAIOAuth429Failover(apiKeyAccount, http.StatusInternalServerError, 2, &state))
	})

	var state OpenAIOAuth429FailoverState
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 0, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(apiKeyAccount, http.StatusTooManyRequests, 2, &state))
}

// TestNVIDIA429QuotaFastPath_BlocksNVIDIAAPIKey 验证 quota 语义 429 在 fastpath 中被
// 截获并进入 30min BlockAccountScheduling（E 方案的核心验收：quota → 长冷却，
// 不进入后续 adaptive throttle 短退避与 failover 扩散）。
func TestNVIDIA429QuotaFastPath_BlocksNVIDIAAPIKey(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       50,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://integrate.api.nvidia.com/v1",
			"api_key":  "nvapi-test-key",
		},
	}

	body := []byte(`{"error":{"message":"You have exceeded your current quota, please check your plan and billing details"}}`)
	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, body)

	require.True(t, shouldDisable, "quota-class 429 must short-circuit with shouldDisable=true")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account), "NVIDIA API key with quota 429 must be runtime blocked")

	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	blockedUntil, ok := value.(time.Time)
	require.True(t, ok)
	// 必须用 nvidiaQuotaCooldown（30min），而不是 nvidiaResourceExhaustedCooldown（2min）
	require.WithinDuration(t, time.Now().Add(nvidiaQuotaCooldown), blockedUntil, 5*time.Second)
}

// TestNVIDIA429QuotaFastPath_IgnoresNonNVIDIAAPIKey quota 关键词较通用，
// 非 NVIDIA 上游的 API-Key 账号返回同类 429 body 时不得被 30min 误封。
// 该账号不满足 isNVIDIAAccountByHostname，应保持原路径（OAuth 侧 429 逻辑）。
func TestNVIDIA429QuotaFastPath_IgnoresNonNVIDIAAPIKey(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       51,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey, // 默认 base_url = https://api.openai.com → 非 NVIDIA
		Credentials: map[string]any{
			"api_key": "sk-openai-key",
		},
	}

	body := []byte(`{"error":{"message":"You have exceeded your current quota for today"}}`)
	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, body)

	require.False(t, shouldDisable, "non-NVIDIA API key must not be pulled into nvidia_quota fastpath")
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account), "non-NVIDIA API key must not be 30min blocked")
}

// TestNVIDIAQuotaFastPath_Resource429Keeps2MinCooldown ResourceExhausted 语义
// 的 429 仍走既有 2min 冷却（isNVIDIAResourceExhaustedError），不被 quota 分支抢占。
// 两个分支基于同一 classify 结果互斥，避免新分层破坏批次1的 worker 池饱和语义。
func TestNVIDIAQuotaFastPath_Resource429Keeps2MinCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       52,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://integrate.api.nvidia.com/v1",
			"api_key":  "nvapi-test-key",
		},
	}

	body := []byte(`{"error":{"code":"ResourceExhausted","message":"Worker local total request limit exceeded"}}`)
	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, body)

	require.True(t, shouldDisable, "resource-class 429 still handled")
	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	blockedUntil, ok := value.(time.Time)
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(nvidiaResourceExhaustedCooldown), blockedUntil, 5*time.Second,
		"ResourceExhausted 429 must keep 2min cooldown, not widened to quota 30min")
}

// TestNVIDIAQuotaFastPath_RateLimit429NotBlocked 瞬时 RPM 语义 429 不进 quota 分支：
// 既不进入 nvidia_quota long-cooldown，也不 shouldDisable（保持既有自适应节流语义）。
func TestNVIDIAQuotaFastPath_RateLimit429NotBlocked(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       53,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://integrate.api.nvidia.com/v1",
			"api_key":  "nvapi-test-key",
		},
	}

	body := []byte(`{"error":{"message":"Requests per minute limit exceeded, retry in 60s"}}`)
	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, body)

	require.False(t, shouldDisable, "rate-class 429 must not be intercepted by quota fastpath")
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account), "rate-class 429 must not land in 30min cooldown")
}

func TestIsNvidiaPrewarmBlocked_ReflectsRuntimeBlockByID(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 61, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	require.False(t, svc.IsNvidiaPrewarmBlocked(61), "no block yet")

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "nvidia_quota")
	require.True(t, svc.IsNvidiaPrewarmBlocked(61), "blocked account must be skipped for prewarm")

	svc.ClearAccountSchedulingBlock(61)
	require.False(t, svc.IsNvidiaPrewarmBlocked(61), "cleared block must unblock prewarm")
}

func TestIsNvidiaPrewarmBlocked_NilSafeAndBadArgs(t *testing.T) {
	var svc *OpenAIGatewayService
	require.False(t, svc.IsNvidiaPrewarmBlocked(1), "nil receiver is safe")

	svc = &OpenAIGatewayService{}
	require.False(t, svc.IsNvidiaPrewarmBlocked(0), "accountID<=0 rejected")
	require.False(t, svc.IsNvidiaPrewarmBlocked(-5), "accountID<=0 rejected")
}

// fakeAcceptingNvidiaCache Apply/Reserve 都成功返回, 模拟 adaptive throttle 完全放行的 L2。
type fakeAcceptingNvidiaCache struct{}

func (f *fakeAcceptingNvidiaCache) Reserve(_ context.Context, _ int64, _ string) (NvidiaCacheReserveResult, error) {
	return NvidiaCacheReserveResult{Allowed: true}, nil
}
func (f *fakeAcceptingNvidiaCache) Apply(_ context.Context, _ int64, _ string, _ NvidiaThrottleRate) error {
	return nil
}

// countingNvidiaCache 记录 Apply 调用次数（含并发保护），用于断言
// 非 NVIDIA 账号的结果/失败不会写入 NVIDIA adaptive throttle 缓存。
type countingNvidiaCache struct {
	mu      sync.Mutex
	applies int
}

func (c *countingNvidiaCache) Reserve(_ context.Context, _ int64, _ string) (NvidiaCacheReserveResult, error) {
	return NvidiaCacheReserveResult{Allowed: true}, nil
}
func (c *countingNvidiaCache) Apply(_ context.Context, _ int64, _ string, _ NvidiaThrottleRate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applies++
	return nil
}

func (c *countingNvidiaCache) applyCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applies
}

// TestNVIDIAQuotaFastPath_Resource429Keeps2MinCooldownWithAdaptiveEnabled 回归：
// adaptive throttle 启用（rateLimitService 非 nil + canonicalModel 非空）时,
// ResourceExhausted 429 仍必须进入 2min BlockAccountScheduling, 不能被
// RecordNVIDIAAdaptiveThrottleOutcome 判为 rate 提前 return（B1）。
func TestNVIDIAQuotaFastPath_Resource429Keeps2MinCooldownWithAdaptiveEnabled(t *testing.T) {
	rateLimitSvc := NewRateLimitService(&rateLimitAccountRepoStub{}, nil, &config.Config{}, nil, nil)
	rateLimitSvc.SetNvidiaAdaptiveThrottleCache(&fakeAcceptingNvidiaCache{})
	svc := &OpenAIGatewayService{rateLimitService: rateLimitSvc}
	account := &Account{
		ID:       54,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://integrate.api.nvidia.com/v1",
			"api_key":  "nvapi-test-key",
		},
	}

	body := []byte(`{"error":{"code":"ResourceExhausted","message":"Worker local total request limit exceeded"}}`)
	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(), account, http.StatusTooManyRequests, http.Header{}, body,
		"nvidia/llama-3.3-70b",
	)

	require.True(t, shouldDisable, "resource-class 429 must still short-circuit with adaptive enabled")
	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok, "resource 429 must land in runtime block even with adaptive enabled")
	blockedUntil, ok := value.(time.Time)
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(nvidiaResourceExhaustedCooldown), blockedUntil, 5*time.Second,
		"adaptive enabled must not swallow the 2min resource cooldown")
}

// TestNVIDIAQuotaFastPath_Rate429StillAdaptiveWithAdaptiveEnabled 回归：
// 普通 rate 语义 429 在 adaptive 启用时仍走 adaptive 路径（不被 resource/quota 抢占）。
func TestNVIDIAQuotaFastPath_Rate429StillAdaptiveWithAdaptiveEnabled(t *testing.T) {
	rateLimitSvc := NewRateLimitService(&rateLimitAccountRepoStub{}, nil, &config.Config{}, nil, nil)
	rateLimitSvc.SetNvidiaAdaptiveThrottleCache(&fakeAcceptingNvidiaCache{})
	svc := &OpenAIGatewayService{rateLimitService: rateLimitSvc}
	account := &Account{
		ID:       55,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://integrate.api.nvidia.com/v1",
			"api_key":  "nvapi-test-key",
		},
	}

	body := []byte(`{"error":{"message":"Requests per minute limit exceeded, retry in 60s"}}`)
	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(), account, http.StatusTooManyRequests, http.Header{}, body,
		"nvidia/llama-3.3-70b",
	)

	require.True(t, shouldDisable, "rate-class 429 handled by adaptive path")
	_, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.False(t, ok, "rate-class 429 must NOT land in resource/quota runtime block")
}

// TestNVIDIAQuotaFastPath_Resource429IgnoresNonNVIDIAAPIKey B1' 回归：
// 非 NVIDIA 上游（默认 base_url = api.openai.com）的 API-Key 账号返回
// ResourceExhausted 语义 429 body 时，不得被 NVIDIA resource 分支 2min 封禁
// （provider isolation：NVIDIA 分支必须带 isNVIDIAAccountByHostname guard）。
func TestNVIDIAQuotaFastPath_Resource429IgnoresNonNVIDIAAPIKey(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       56,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey, // 默认 base_url = api.openai.com → 非 NVIDIA
		Credentials: map[string]any{
			"api_key": "sk-openai-key",
		},
	}

	body := []byte(`{"error":{"code":"ResourceExhausted","message":"Worker local total request limit exceeded"}}`)
	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, body)

	require.False(t, shouldDisable, "non-NVIDIA API key must not be pulled into nvidia resource fastpath")
	_, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.False(t, ok, "non-NVIDIA API key must not be 2min blocked by nvidia resource branch")
}

// TestNVIDIAUnauthorizedFastPath_IgnoresNonNVIDIAAPIKey B1''-1 回归：
// 非 NVIDIA 上游（默认 base_url = api.openai.com）的 API-Key 账号返回
// 401 + "unauthorized" body 时，不得被 NVIDIA unauthorized 分支送 24h 冷却
// （provider isolation：unauthorized 分支必须带 isNVIDIAAccountByHostname guard）。
func TestNVIDIAUnauthorizedFastPath_IgnoresNonNVIDIAAPIKey(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       57,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey, // 默认 base_url = api.openai.com → 非 NVIDIA
		Credentials: map[string]any{
			"api_key": "sk-openai-key",
		},
	}

	body := []byte(`{"error":{"message":"unauthorized"}}`)
	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusUnauthorized, http.Header{}, body)

	require.False(t, shouldDisable, "non-NVIDIA API key must not be pulled into nvidia unauthorized fastpath")
	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.False(t, ok, "non-NVIDIA API key must not be 24h blocked by nvidia unauthorized branch")
	if ok {
		blockedUntil, isTime := value.(time.Time)
		require.True(t, isTime)
		require.Less(t, blockedUntil.Sub(time.Now()), nvidiaUnauthorizedCooldown-time.Hour,
			"non-NVIDIA 401 must not land in the nvidia 24h unauthorized cooldown")
	}
}

// TestNVIDIAAdaptiveThrottleFailure_IgnoresNonNVIDIAAccount B1''-2 回归：
// adaptive throttle 失败路径（fastpath 内 RecordNVIDIAAdaptiveThrottleOutcome）必须带
// isNVIDIAAccountByHostname guard：非 NVIDIA 账号的 429 rate 不得写入 NVIDIA
// throttle cache（也不得提前 handled 短路 generic 429 处理）。
func TestNVIDIAAdaptiveThrottleFailure_IgnoresNonNVIDIAAccount(t *testing.T) {
	cache := &countingNvidiaCache{}
	rateLimitSvc := NewRateLimitService(&rateLimitAccountRepoStub{}, nil, &config.Config{}, nil, nil)
	rateLimitSvc.SetNvidiaAdaptiveThrottleCache(cache)
	svc := &OpenAIGatewayService{rateLimitService: rateLimitSvc}
	account := &Account{
		ID:       58,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey, // 默认 base_url = api.openai.com → 非 NVIDIA
		Credentials: map[string]any{
			"api_key": "sk-openai-key",
		},
	}

	body := []byte(`{"error":{"message":"Requests per minute limit exceeded, retry in 60s"}}`)
	// 必须传 canonicalModel：否则 nvidiaAdaptiveThrottleEnabled 因 scope 为空直接 false，测不到 RED。
	_ = svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, body, "gpt-5.4")

	require.Equal(t, 0, cache.applyCount(), "non-NVIDIA 429 rate must not be recorded into NVIDIA adaptive throttle cache")
}
