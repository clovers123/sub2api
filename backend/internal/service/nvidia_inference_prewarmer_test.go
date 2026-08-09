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
	mu           sync.Mutex
	calls        []inferencePrewarmCall
	failNext     bool
	rejectNext   bool
	rejectAlways bool
}

type inferencePrewarmCall struct {
	proxyURL string
	baseURL  string
	apiKey   string
	model    string
}

func (f *fakeInferencePrewarmUpstream) PrewarmNVIDIAInference(ctx context.Context, proxyURL, baseURL, apiKey, model string) (NVIDIAInferencePrewarmOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, inferencePrewarmCall{proxyURL, baseURL, apiKey, model})
	if f.failNext {
		f.failNext = false
		return NVIDIAInferencePrewarmRejected, errors.New("transport error")
	}
	if f.rejectNext {
		f.rejectNext = false
		return NVIDIAInferencePrewarmRejected, nil
	}
	if f.rejectAlways {
		return NVIDIAInferencePrewarmRejected, nil
	}
	return NVIDIAInferencePrewarmAccepted, nil
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

// fakePrewarmBlockChecker 按账号 ID 返回封锁状态。
type fakePrewarmBlockChecker struct {
	blocked map[int64]bool
}

func (f *fakePrewarmBlockChecker) IsNvidiaPrewarmBlocked(accountID int64) bool {
	return f.blocked != nil && f.blocked[accountID]
}

func nvidiaInferencePrewarmTestAccount(id int64, baseURL, apiKey string, mapping map[string]any) Account {
	return Account{
		ID:          id,
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

// TestNVIDIAInferencePrewarmer_PeriodicIntervalRunsMultipleRounds 回归死锁:
// runOnce 的 defer 顺序若先等 watcherDone 再 stopCancel，正常完成路径会永久阻塞
// （stopCtx 未取消、stopCh 未关闭，watcher 永不退出），periodic 预热第一轮后卡死。
// interval=1s 下应能观察到第二轮（Rounds>=2 且 upstream 至少 2 次调用）。
func TestNVIDIAInferencePrewarmer_PeriodicIntervalRunsMultipleRounds(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{}
	account := nvidiaInferencePrewarmTestAccount(1, "https://integrate.api.nvidia.com/v1", "key-1", map[string]any{
		"user-model": "nvidia/model-a",
	})
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{account}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 1, "", nil, nil)
	p.Start()
	defer p.Stop()

	// 第一轮（启动即执行）+ 第二轮（ticker 1s 后）必须在 3s 内到达。
	waitForRounds(t, p, 2)
	require.GreaterOrEqual(t, upstream.count(), 2, "periodic prewarm must run more than one round")
}

// TestNVIDIAInferencePrewarmer_ConcurrentStartStopSerialized 回归 Start/Stop TOCTOU:
// Start 检查 stopped==false 与 wg.Add(1) 之间若 Stop 完成（含 wg.Wait），
// 会导致 Stop 返回后 goroutine 才启动。lifecycleMu 串行化后并发调用不得 panic，
// 且 Stop 返回后不得再有新增预热调用。
func TestNVIDIAInferencePrewarmer_ConcurrentStartStopSerialized(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"base_url": "https://integrate.api.nvidia.com/v1", "api_key": "key-1",
		"model_mapping": map[string]any{"user-model": "nvidia/model-a"},
	}}
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{*account}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "", nil, nil)
	// 交替并发 Start/Stop：若存在 WaitGroup 并发 Add/Wait 竞态，-race 会捕获。
	for i := 0; i < 5; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			p.Start()
		}()
		go func() {
			defer wg.Done()
			p.Stop()
		}()
		wg.Wait()
	}
	// Stop 已返回：等一小段确认没有 goroutine latently 启动新轮次。
	before := upstream.count()
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, before, upstream.count(), "no new prewarm calls may appear after Stop returns")
}

func TestNVIDIAInferencePrewarmer_OneShotRoundsFiresForEachNVIDIAAccount(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{}
	account := nvidiaInferencePrewarmTestAccount(1, "https://integrate.api.nvidia.com/v1", "key-1", map[string]any{
		"user-model": "nvidia/model-a",
	})
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{account}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "", nil, nil)
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

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "nvidia/override-model", nil, nil)
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

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "", nil, nil)
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

	p := NewNVIDIAInferencePrewarmer(upstream, repo, false, 0, "", nil, nil)
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

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "", nil, nil)
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

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "", nil, nil)
	p.Start()
	waitForRounds(t, p, 1)
	p.Stop()
	// 已停止后 Start 不应再执行轮次。
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()
	require.Equal(t, 1, upstream.count())
}

func TestNVIDIAInferencePrewarmer_SuccessFeedsMetricsSnapshot(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{}
	account := &Account{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"base_url": "https://integrate.api.nvidia.com/v1", "api_key": "key-1",
		"model_mapping": map[string]any{"user-model": "nvidia/model-a"},
	}}
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{*account}}
	metrics := NewNVIDIASharedConnectionPoolMetrics()

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "", metrics, nil)
	p.Start()
	defer p.Stop()

	waitForRounds(t, p, 1)
	lastAt, success, fail := metrics.SnapshotPrewarm(5)
	require.Equal(t, int64(1), success, "successful prewarm must land in metrics success_total")
	require.Equal(t, int64(0), fail)
	require.Greater(t, lastAt, int64(0), "successful prewarm must stamp lastPrewarmAt")
}

func TestNVIDIAInferencePrewarmer_TransportFailureFeedsMetricsFailTotal(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{failNext: true}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"base_url": "https://integrate.api.nvidia.com/v1", "api_key": "key-1",
		"model_mapping": map[string]any{"user-model": "nvidia/model-a"},
	}}
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{*account}}
	metrics := NewNVIDIASharedConnectionPoolMetrics()

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "", metrics, nil)
	p.Start()
	defer p.Stop()

	waitForRounds(t, p, 1)
	lastAt, success, fail := metrics.SnapshotPrewarm(1)
	require.Equal(t, int64(1), fail, "transport failure must write metrics fail_total")
	require.Equal(t, int64(0), success)
	require.Equal(t, int64(0), lastAt, "failed prewarm must not stamp lastPrewarmAt")
}

func TestNVIDIAInferencePrewarmer_SkipsRuntimeBlockedAccounts(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{}
	free := nvidiaInferencePrewarmTestAccount(1, "https://integrate.api.nvidia.com/v1", "key-1", map[string]any{
		"user-model": "nvidia/model-a",
	})
	blocked := nvidiaInferencePrewarmTestAccount(2, "https://integrate.api.nvidia.com/v1", "key-2", map[string]any{
		"user-model": "nvidia/model-b",
	})
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{free, blocked}}
	checker := &fakePrewarmBlockChecker{blocked: map[int64]bool{2: true}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "", nil, checker)
	p.Start()
	defer p.Stop()

	waitForRounds(t, p, 1)
	require.Equal(t, 1, upstream.count(), "blocked account must not be prewarmed")
	require.Equal(t, "key-1", upstream.call(0).apiKey)
	require.Equal(t, 1, p.Snapshot().LastRoundAccounts, "blocked account excluded from round")
}

func TestNVIDIAInferencePrewarmer_NilBlockCheckerPreWarmsAll(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{}
	free := nvidiaInferencePrewarmTestAccount(1, "https://integrate.api.nvidia.com/v1", "key-1", map[string]any{
		"user-model": "nvidia/model-a",
	})
	blocked := nvidiaInferencePrewarmTestAccount(2, "https://integrate.api.nvidia.com/v1", "key-2", map[string]any{
		"user-model": "nvidia/model-b",
	})
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{free, blocked}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "", nil, nil)
	p.Start()
	defer p.Stop()

	waitForRounds(t, p, 1)
	require.Equal(t, 2, upstream.count(), "nil checker keeps legacy behavior: prewarm all candidates")
	require.Equal(t, 2, p.Snapshot().LastRoundAccounts)
}

func TestNVIDIAInferencePrewarmer_RejectedOutcomeDoesNotCountAsSuccessOrFail(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{rejectAlways: true}
	account := &Account{ID: 70, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"base_url": "https://integrate.api.nvidia.com/v1", "api_key": "key-1",
		"model_mapping": map[string]any{"user-model": "nvidia/model-a"},
	}}
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{*account}}
	metrics := NewNVIDIASharedConnectionPoolMetrics()

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "", metrics, nil)
	p.Start()
	defer p.Stop()

	waitForRounds(t, p, 1)
	lastAt, success, fail := metrics.SnapshotPrewarm(70)
	require.Equal(t, int64(0), success, "rejected (non-2xx) prewarm must not count as success")
	require.Equal(t, int64(0), fail, "rejected (non-2xx) prewarm must not accumulate failTotal")
	require.Equal(t, int64(0), lastAt, "rejected (non-2xx) prewarm must not stamp lastPrewarmAt")
}

func TestNVIDIAInferencePrewarmer_RejectedOutcomeTrackedAsRejectedInSnapshot(t *testing.T) {
	upstream := &fakeInferencePrewarmUpstream{rejectAlways: true}
	account := &Account{ID: 71, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"base_url": "https://integrate.api.nvidia.com/v1", "api_key": "key-1",
		"model_mapping": map[string]any{"user-model": "nvidia/model-a"},
	}}
	repo := &fakeInferencePrewarmAccountRepo{accounts: []Account{*account}}

	p := NewNVIDIAInferencePrewarmer(upstream, repo, true, 0, "", nil, nil)
	p.Start()
	defer p.Stop()

	waitForRounds(t, p, 1)
	snap := p.Snapshot()
	require.Equal(t, int64(0), snap.SucceededTotal, "rejected prewarm must not count as succeeded")
	require.Equal(t, int64(0), snap.FailedTotal, "rejected prewarm is not a transport failure")
	require.Equal(t, int64(1), snap.RejectedTotal, "rejected prewarm tracked separately in snapshot")
}
