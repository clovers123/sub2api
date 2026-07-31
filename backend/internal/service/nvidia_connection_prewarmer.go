package service

import (
	"context"
	"log"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// NVIDIA 连接预热器相关常量
const (
	// nvidiaPrewarmMaxConcurrency: 单轮预热的最大并发数
	nvidiaPrewarmMaxConcurrency = 4
	// nvidiaPrewarmRoundTimeout: 单轮预热（含账号查询 + 全部探测请求）总超时
	nvidiaPrewarmRoundTimeout = 60 * time.Second
	// nvidiaPrewarmDefaultIntervalSeconds: 周期保活默认间隔（秒）
	nvidiaPrewarmDefaultIntervalSeconds = 240
)

// nvidiaPrewarmAccountLister 预热器所需的账号查询能力（AccountRepository 子集）。
type nvidiaPrewarmAccountLister interface {
	ListByPlatform(ctx context.Context, platform string) ([]Account, error)
}

// NVIDIAConnectionPrewarmerSnapshot 预热器运行状态快照（用于观测/测试）。
type NVIDIAConnectionPrewarmerSnapshot struct {
	// Rounds 已完成的预热轮次
	Rounds int64 `json:"rounds"`
	// LastRoundProxies 上一轮去重后的代理数（含直连）
	LastRoundProxies int `json:"last_round_proxies"`
	// SucceededTotal 累计预热成功次数
	SucceededTotal int64 `json:"succeeded_total"`
	// FailedTotal 累计预热失败次数（传输层错误）
	FailedTotal int64 `json:"failed_total"`
	// LastRoundAt 上一轮完成时间（零值表示尚未执行过）
	LastRoundAt time.Time `json:"last_round_at"`
}

// NVIDIAConnectionPrewarmer NVIDIA 上游连接预热器。
//
// 启动时对所有 NVIDIA API-Key 账号使用的代理（去重，含直连）各发送一次
// 无凭据探测请求，完成 TCP+TLS+H2 建连；随后按配置间隔周期保活。
// 连接池缓存键为 host+proxy（与账号无关），因此每个代理只需预热一次
// 即可让共享池覆盖全部账号。
type NVIDIAConnectionPrewarmer struct {
	upstream    NVIDIAConnectionPrewarmUpstream
	accountRepo nvidiaPrewarmAccountLister
	enabled     bool
	interval    time.Duration

	stopCh    chan struct{}
	stopOnce  sync.Once
	startOnce sync.Once
	stopped   atomic.Bool
	wg        sync.WaitGroup

	mu       sync.Mutex
	snapshot NVIDIAConnectionPrewarmerSnapshot
}

// NewNVIDIAConnectionPrewarmer 构造 NVIDIA 连接预热器。
//
// 参数:
//   - upstream: 预热能力（repository 的 httpUpstreamService 实现）
//   - accountRepo: 账号查询（取 openai 平台账号并携带代理信息）
//   - enabled: 是否启用（gateway.nvidia_shared_connection_pool.prewarm_enabled）
//   - intervalSeconds: 保活间隔秒数；0 表示仅启动时预热一次，不开定时器；
//     负数视为未配置，回退默认值 240。
func NewNVIDIAConnectionPrewarmer(
	upstream NVIDIAConnectionPrewarmUpstream,
	accountRepo nvidiaPrewarmAccountLister,
	enabled bool,
	intervalSeconds int,
) *NVIDIAConnectionPrewarmer {
	if intervalSeconds < 0 {
		intervalSeconds = nvidiaPrewarmDefaultIntervalSeconds
	}
	return &NVIDIAConnectionPrewarmer{
		upstream:    upstream,
		accountRepo: accountRepo,
		enabled:     enabled,
		interval:    time.Duration(intervalSeconds) * time.Second,
		stopCh:      make(chan struct{}),
	}
}

// Start 启动预热器：立即执行一轮预热，interval > 0 时另起后台保活循环。
// 未启用或依赖缺失时，或者已启动 / 已停止后，均为 no-op。
func (p *NVIDIAConnectionPrewarmer) Start() {
	if p == nil || !p.enabled || p.upstream == nil || p.accountRepo == nil || p.stopped.Load() {
		return
	}
	p.startOnce.Do(func() {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.runOnce()
			if p.interval <= 0 {
				// interval=0：仅启动预热一次，不做周期保活。
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
func (p *NVIDIAConnectionPrewarmer) Stop() {
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
func (p *NVIDIAConnectionPrewarmer) Snapshot() NVIDIAConnectionPrewarmerSnapshot {
	if p == nil {
		return NVIDIAConnectionPrewarmerSnapshot{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshot
}

// runOnce 执行一轮预热：查询账号 → 去重代理 → 并发（≤4）预热。
func (p *NVIDIAConnectionPrewarmer) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaPrewarmRoundTimeout)
	defer cancel()

	proxies, err := p.collectProxyURLs(ctx)
	if err != nil {
		log.Printf("[NVIDIAPrewarm] list accounts failed")
		return
	}
	if len(proxies) == 0 {
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
	for _, proxyURL := range proxies {
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
		go func(proxy string) {
			defer roundWG.Done()
			defer func() { <-sem }()
			err := p.upstream.PrewarmNVIDIAConnection(ctx, proxy)
			countMu.Lock()
			defer countMu.Unlock()
			if err != nil {
				failed++
				// 不打印原始 error（可能含代理 URL 凭据）；轮次汇总行会报告失败计数。
				return
			}
			succeeded++
		}(proxyURL)
	}
	roundWG.Wait()
	p.recordRound(len(proxies), succeeded, failed)
	if failed > 0 {
		log.Printf("[NVIDIAPrewarm] round done: proxies=%d succeeded=%d failed=%d", len(proxies), succeeded, failed)
	}
}

// collectProxyURLs 查询 openai 平台账号，筛选 NVIDIA API-Key 账号并去重代理 URL。
// 返回值中空字符串表示直连。
func (p *NVIDIAConnectionPrewarmer) collectProxyURLs(ctx context.Context) ([]string, error) {
	accounts, err := p.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	proxies := make([]string, 0, 4)
	for i := range accounts {
		account := &accounts[i]
		if !isNVIDIAPrewarmCandidate(account) {
			continue
		}
		proxyURL := ""
		if account.ProxyID != nil && account.Proxy != nil {
			proxyURL = account.Proxy.URL()
		}
		if _, ok := seen[proxyURL]; ok {
			continue
		}
		seen[proxyURL] = struct{}{}
		proxies = append(proxies, proxyURL)
	}
	return proxies, nil
}

// recordRound 更新快照计数。
func (p *NVIDIAConnectionPrewarmer) recordRound(proxies int, succeeded, failed int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot.Rounds++
	p.snapshot.LastRoundProxies = proxies
	p.snapshot.SucceededTotal += succeeded
	p.snapshot.FailedTotal += failed
	p.snapshot.LastRoundAt = time.Now()
}

// isNVIDIAPrewarmCandidate 判断账号是否为 NVIDIA 预热候选：
// openai 平台 + API-Key 类型 + base_url 主机为 NVIDIA 官方 API 主机。
// （ListByPlatform 已过滤 StatusActive。）
func isNVIDIAPrewarmCandidate(account *Account) bool {
	if account == nil {
		return false
	}
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey {
		return false
	}
	baseURL := account.GetBaseURL()
	if baseURL == "" {
		return false
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return parsed.Hostname() == nvidiaAPIHostname
}
