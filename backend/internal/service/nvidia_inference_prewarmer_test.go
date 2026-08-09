//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeInferencePrewarmUpstream 记录调用参数的推理预热桩。
type fakeInferencePrewarmUpstream struct {
	mu       sync.Mutex
	calls    []inferencePrewarmCall
	failNext bool
}

type inferencePrewarmCall struct {
	proxyURL string
	baseURL  string
	apiKey   string
	model    string
}

func (f *fakeInferencePrewarmUpstream) PrewarmNVIDIAInference(ctx context.Context, proxyURL, baseURL, apiKey, model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, inferencePrewarmCall{proxyURL, baseURL, apiKey, model})
	if f.failNext {
		f.failNext = false
		return errors.New("transport error")
	}
	return nil
}

func (f *fakeInferencePrewarmUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeInferencePrewarmUpstream) call(i int) inferencePrewarmCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// fakeInferencePrewarmAccountRepo 返回固定账号列表。
type fakeInferencePrewarmAccountRepo struct {
	accounts []Account
}

func (f *fakeInferencePrewarmAccountRepo) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return f.accounts, nil
}

func nvidiaInferencePrewarmTestAccount(id int64, baseURL, apiKey string, mapping map[string]any) Account {
	return Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": baseURL, "api_key": apiKey, "model_mapping": mapping},
	}
}

// waitForRounds 等待预热器完成指定轮次（默认启用时 Start 立即执行一轮）。
func waitForRounds(t *testing.T, p *NVIDIAInferencePrewarmer, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.Snapshot().Rounds >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.FailNow(t, "prewarmer rounds not reached")
}

func TestNVIDIAInferencePrewarmer_OneShotRoundsFiresForEachNVIDIAAccount(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{}
	account := nvidiaInferencePrewarmTestAccount(1, "https://integrate.api.nvidia.com/v1", "key-1", map[string]any{
		"user-model": "nvidia/model-a",
	})
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{account}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "")
	p.Start()
	defer p.Stop()

	waitForRounds(t, p, 1)
	require.Equal(t, 1, upstream.count())
	call := upstream.call(0)
	require.Equal(t, "https://integrate.api.nvidia.com/v1", call.baseURL)
	require.Equal(t, "key-1", call.apiKey)
	require.Equal(t, "nvidia/model-a", call.model)
	require.Equal(t, "", call.proxyURL)

	snap := p.Snapshot()
	require.Equal(t, int64(1), snap.SucceededTotal)
	require.Equal(t, int64(0), snap.FailedTotal)
	require.Equal(t, 1, snap.LastRoundAccounts)
}

func TestNVIDIAInferencePrewarmer_ModelOverrideWins(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{}
	account := nvidiaInferencePrewarmTestAccount(1, "https://integrate.api.nvidia.com/v1", "key-1", map[string]any{
		"user-model": "nvidia/model-a",
	})
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{account}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "nvidia/override-model")
	p.Start()
	defer p.Stop()

	waitForRounds(t, p, 1)
	require.Equal(t, "nvidia/override-model", upstream.call(0).model)
}

func TestNVIDIAInferencePrewarmer_SkipsNonNVIDIAAndCredentialLess(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{}
	good := nvidiaInferencePrewarmTestAccount(1, "https://integrate.api.nvidia.com/v1", "key-1", map[string]any{
		"user-model": "nvidia/model-a",
	})
	nonNvidia := nvidiaInferencePrewarmTestAccount(2, "https://api.openai.com/v1", "key-2", map[string]any{
		"user-model": "gpt-4o",
	})
	noKey := nvidiaInferencePrewarmTestAccount(3, "https://integrate.api.nvidia.com/v1", "", map[string]any{
		"user-model": "nvidia/model-c",
	})
	noMapping := nvidiaInferencePrewarmTestAccount(4, "https://integrate.api.nvidia.com/v1", "key-4", nil)
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{good, nonNvidia, noKey, noMapping}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "")
	p.Start()
	defer p.Stop()

	waitForRounds(t, p, 1)
	require.Equal(t, 1, upstream.count())
	require.Equal(t, "key-1", upstream.call(0).apiKey)
	snap := p.Snapshot()
	require.Equal(t, 1, snap.LastRoundAccounts)
}

func TestNVIDIAInferencePrewarmer_DisabledIsNoOp(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"base_url": "https://integrate.api.nvidia.com/v1", "api_key": "key-1",
		"model_mapping": map[string]any{"user-model": "nvidia/model-a"},
	}}
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{*account}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, false, 0, "")
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()
	require.Equal(t, 0, upstream.count())
	require.Equal(t, int64(0), p.Snapshot().Rounds)
}

func TestNVIDIAInferencePrewarmer_RecordsFailures(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{failNext: true}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"base_url": "https://integrate.api.nvidia.com/v1", "api_key": "key-1",
		"model_mapping": map[string]any{"user-model": "nvidia/model-a"},
	}}
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{*account}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "")
	p.Start()
	defer p.Stop()

	waitForRounds(t, p, 1)
	snap := p.Snapshot()
	require.Equal(t, int64(0), snap.SucceededTotal)
	require.Equal(t, int64(1), snap.FailedTotal)
}

func TestNVIDIAInferencePrewarmer_StartStopRoundtrip(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{}
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
			"base_url": "https://integrate.api.nvidia.com/v1", "api_key": "key-1",
			"model_mapping": map[string]any{"user-model": "nvidia/model-a"},
		}},
	}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "")
	p.Start()
	waitForRounds(t, p, 1)
	p.Stop()
	// 已停止后 Start 不应再执行轮次。
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()
	require.Equal(t, 1, upstream.count())
}