//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestFilterNVIDIAAvailableAccounts_SkipsSaturatedKeepsFallback 验证调度层 A1 预 gate：
//   - NVIDIA 账号的 scope 已满时从候选剔除（若还有其他可用候选）；
//   - 全部候选都被判满时保留原列表兜底，把 429 语义留给 forward 层，
//     而不是误报"无可用账号"。
func TestFilterNVIDIAAvailableAccounts_SkipsSaturatedKeepsFallback(t *testing.T) {
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				NvidiaSharedConnectionPool: config.GatewayNvidiaSharedConnectionPoolConfig{
					Enabled: true,
				},
			},
		},
		rateLimitService: &RateLimitService{nvidiaInflight: newNVIDIAInflightTracker()},
	}
	ctx := context.Background()
	const scopeModel = "meta/llama-3.1-405b-instruct"

	openAI := rawChatCompletionsTestAccount()          // 非 NVIDIA 上游
	nvidia := rawChatCompletionsNVIDIATestAccount()     // NVIDIA 上游
	nvidia2 := rawChatCompletionsNVIDIATestAccount()    // 第二个 NVIDIA 上游
	nvidia2.ID = 103
	require.True(t, nvidia2.IsOpenAIApiKey())

	// 填满两个 NVIDIA 账号的 scope（默认 MaxInflight=4）
	rl := svc.rateLimitService
	for i := 0; i < 4; i++ {
		release, ok := rl.TryAcquireNVIDIAInflight(ctx, NVIDIAThrottleScope{AccountID: nvidia.ID, CanonicalModel: scopeModel})
		require.True(t, ok, "acquire slot %d", i)
		_ = release
		release2, ok2 := rl.TryAcquireNVIDIAInflight(ctx, NVIDIAThrottleScope{AccountID: nvidia2.ID, CanonicalModel: scopeModel})
		require.True(t, ok2, "acquire slot 2 %d", i)
		_ = release2
	}

	cases := []struct {
		name    string
		cands   []*Account
		wantLen int
	}{
		{
			name:    "饱和NVIDIA账号被剔除_保留非NVIDIA",
			cands:   []*Account{nvidia, openAI},
			wantLen: 1,
		},
		{
			name:    "全部饱和_保留原列表兜底",
			cands:   []*Account{nvidia, nvidia2},
			wantLen: 2,
		},
		{
			name:    "无饱和_原样返回",
			cands:   []*Account{openAI},
			wantLen: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filterNVIDIAAvailableAccounts(ctx, svc, scopeModel, tc.cands)
			require.Len(t, out, tc.wantLen)
		})
	}
}

// TestNvidiaGatedForOpenAIScheduling_ScopeConsistency 验证预 gate 的 scope 键
// 与 forward 层 acquire 完全一致：同一 (account, canonical_model) 组合在
// 调度层判定饱和时，forward 层 TryAcquireNVIDIAInflight 也必须拒绝。
func TestNvidiaGatedForOpenAIScheduling_ScopeConsistency(t *testing.T) {
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				NvidiaSharedConnectionPool: config.GatewayNvidiaSharedConnectionPoolConfig{
					Enabled: true,
				},
			},
		},
		rateLimitService: &RateLimitService{nvidiaInflight: newNVIDIAInflightTracker()},
	}
	ctx := context.Background()
	nvidia := rawChatCompletionsNVIDIATestAccount()
	const model = "deepseek-ai/deepseek-v4-pro"

	if svc.nvidiaGatedForOpenAIScheduling(ctx, nvidia, model) {
		t.Fatal("初始状态不应被 gate")
	}

	for i := 0; i < 4; i++ {
		release, ok := svc.rateLimitService.TryAcquireNVIDIAInflight(ctx, NVIDIAThrottleScope{AccountID: nvidia.ID, CanonicalModel: model})
		require.True(t, ok)
		_ = release
	}

	require.True(t, svc.nvidiaGatedForOpenAIScheduling(ctx, nvidia, model), "scope 满后调度层应判为 gate")
}