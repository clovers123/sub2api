package service

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// NVIDIAInferencePrewarmerSnapshot 推理预热器运行状态快照（用于观测/测试）。
type NVIDIAInferencePrewarmerSnapshot struct {
	// Rounds 已完成的预热轮次
	Rounds int64 `json:"rounds"`
	// LastRoundAccounts 上一轮参与预热的账号数
	LastRoundAccounts int `json:"last_round_accounts"`
	// SucceededTotal 累计预热成功次数
	SucceededTotal int64 `json:"succeeded_total"`
	// FailedTotal 累计预热失败次数（传输层错误）
	FailedTotal int64 `json:"failed_total"`
	// LastRoundAt 上一轮完成时间（零值表示尚未执行过）
	LastRoundAt time.Time `json:"last_round_at"`
}

// NVIDIAInferencePrewarmer NVIDIA 上游真实推理预热器。
//
// 连接预热（NVIDIAConnectionPrewarmer）只完成 TCP+TLS+H2 建连，不携带凭据；
// 推理预热对每个 NVIDIA API-Key 账号发送一次最小推理请求（max_tokens=1），
// 触发上游模型加载/保活，避免长时间空闲后首次真实请求遭遇模型冷启动超时。
// 按配置间隔周期执行；默认启用（settings.nvidia_shared_connection_pool.
// inference_prewarm_enabled 默认 "true"）。
type NVIDIAInferencePrewarmer struct {
	upstream    NVIDIAInferencePrewarmUpstream
	accountRepo nvidiaPrewarmAccountLister
	enabled     bool
	interval    time.Duration
	model       string // 显式配置的模型；空 = 使用账号模型映射第一个目标

	stopCh    chan struct{}
	stopOnce  sync.Once
	startOnce sync.Once
	stopped   atomic.Bool
	wg        sync.WaitGroup

	mu       sync.Mutex
	snapshot NVIDIAInferencePrewarmerSnapshot
}

// NewNVIDIAInferencePrewarmer 构造 NVIDIA 推理预热器。
//
// 参数:
//   - upstream: 推理预热能力（repository 的 httpUpstreamService 实现）
//   - accountRepo: 账号查询（取 openai 平台账号并携带 NVIDIA 凭据）
//   - enabled: 是否启用（settings.inference_prewarm_enabled，默认 true）
//   - intervalSeconds: 保活间隔秒数；0 表示仅启动时预热一次，不开定时器
//   - model: 预热模型；空串表示使用账号模型映射的第一个目标
func NewNVIDIAInferencePrewarmer(
	upstream NVIDIAInferencePrewarmUpstream,
	accountRepo nvidiaPrewarmAccountLister,
	enabled bool,
	intervalSeconds int,
	model string,
) *NVIDIAInferencePrewarmer {
	return &NVIDIAInferencePrewarmer{
		upstream:    upstream,
		accountRepo: accountRepo,
		enabled:     enabled,
		interval:    time.Duration(intervalSeconds) * time.Second,
		model:       model,
		stopCh:      make(chan struct{}),
	}
}

// Start 启动预热器：立即执行一轮预热，interval > 0 时另起后台保活循环。
// 未启用或依赖缺失时，或者已启动 / 已停止后，均为 no-op。
func (p *NVIDIAInferencePrewarmer) Start() {
	if p == nil || !p.enabled || p.upstream == nil || p.accountRepo == nil {
		return
	}
	p.startOnce.Do(func() {
		if p.stopped.Load() {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.runOnce()
			if p.interval <= 0 {
				return
			}
			ticker := time.NewTicker(p.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					p.runOnce()
				case <-p.stopCh:
					return
				}
			}
		}()
	})
}

// Stop 停止预热器并等待后台协程退出（幂等）。
func (p *NVIDIAInferencePrewarmer) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		p.stopped.Store(true)
		close(p.stopCh)
	})
	p.wg.Wait()
}

// Snapshot 返回当前运行状态快照。
func (p *NVIDIAInferencePrewarmer) Snapshot() NVIDIAInferencePrewarmerSnapshot {
	if p == nil {
		return NVIDIAInferencePrewarmerSnapshot{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshot
}

// runOnce 执行一轮预热：查询账号 → 对每个候选账号并发（≤4）发送最小推理请求。
// 上下文由 stopCh 驱动：Stop 时立即取消本轮所有在途预热请求。
func (p *NVIDIAInferencePrewarmer) runOnce() {
	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-p.stopCh:
			stopCancel()
		case <-stopCtx.Done():
		}
	}()
	defer func() { <-watcherDone }()
	ctx, cancel := context.WithTimeout(stopCtx, nvidiaPrewarmRoundTimeout)
	defer cancel()

	accounts, err := p.collectAccounts(ctx)
	if err != nil {
		log.Printf("[NVIDIAInferencePrewarm] list accounts failed")
		return
	}
	if len(accounts) == 0 {
		p.recordRound(0, 0, 0)
		return
	}

	sem := make(chan struct{}, nvidiaPrewarmMaxConcurrency)
	var (
		roundWG   sync.WaitGroup
		succeeded int64
		failed    int64
		countMu   sync.Mutex
	)
	for i := range accounts {
		account := &accounts[i]
		select {
		case <-p.stopCh:
			roundWG.Wait()
			return
		case <-ctx.Done():
			roundWG.Wait()
			return
		case sem <- struct{}{}:
		}
		roundWG.Add(1)
		go func(account *Account) {
			defer roundWG.Done()
			defer func() { <-sem }()
			err := p.prewarmAccount(ctx, account)
			countMu.Lock()
			defer countMu.Unlock()
			if err != nil {
				failed++
				return
			}
			succeeded++
		}(account)
	}
	roundWG.Wait()
	p.recordRound(len(accounts), succeeded, failed)
	if failed > 0 {
		log.Printf("[NVIDIAInferencePrewarm] round done: accounts=%d succeeded=%d failed=%d", len(accounts), succeeded, failed)
	}
}

// collectAccounts 查询 openai 平台 NVIDIA API-Key 账号（凭据与模型可解析者）。
func (p *NVIDIAInferencePrewarmer) collectAccounts(ctx context.Context) ([]Account, error) {
	accounts, err := p.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	candidates := make([]Account, 0, 8)
	for i := range accounts {
		account := &accounts[i]
		if !isNVIDIAPrewarmCandidate(account) {
			continue
		}
		if strings.TrimSpace(account.GetCredential("api_key")) == "" {
			continue
		}
		if p.resolveModel(account) == "" {
			// 模型无法解析（无显式配置且账号无模型映射）时跳过该账号。
			continue
		}
		candidates = append(candidates, *account)
	}
	return candidates, nil
}

// prewarmAccount 对单个账号发送最小推理请求。
func (p *NVIDIAInferencePrewarmer) prewarmAccount(ctx context.Context, account *Account) error {
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	return p.upstream.PrewarmNVIDIAInference(
		ctx,
		proxyURL,
		account.GetBaseURL(),
		account.GetCredential("api_key"),
		p.resolveModel(account),
	)
}

// resolveModel 解析预热模型：显式配置优先，否则取账号模型映射第一个目标。
func (p *NVIDIAInferencePrewarmer) resolveModel(account *Account) string {
	if strings.TrimSpace(p.model) != "" {
		return p.model
	}
	if account == nil {
		return ""
	}
	mapping := account.GetModelMapping()
	if len(mapping) == 0 {
		return ""
	}
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return mapping[keys[0]]
}

// recordRound 更新快照计数。
func (p *NVIDIAInferencePrewarmer) recordRound(accounts int, succeeded, failed int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot.Rounds++
	p.snapshot.LastRoundAccounts = accounts
	p.snapshot.SucceededTotal += succeeded
	p.snapshot.FailedTotal += failed
	p.snapshot.LastRoundAt = time.Now()
}