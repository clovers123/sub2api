package service

import (
	"strconv"
	"time"

	"github.com/dgraph-io/ristretto"
)

// nvidiaThrottleL1TTL 是 L1 节流快照缓存 TTL。
// L2 (Redis) 通常 30-60s，L1 短于 1/4 L2 → L1 过期后仍可由 L2 兜底；
// 但 L2 被 AIMD 释放后 L1 最长卡 8s, 不会久阻塞恢复。
const nvidiaThrottleL1TTL = 8 * time.Second

// nvidiaThrottleL1MaxEntries 是 L1 最大条目数。30 账号 × 10 model 量级足够。
const nvidiaThrottleL1MaxEntries = 512

// nvidiaThrottleL1 是进程内 L1 cache, 命中即拒绝调度, 无 Redis RTT.
// 命中时机：上游 429/503 capacity → 调度器收到 block 后 → 同步写 L1
// 失效机制：TTL 8s 自然过期，不主动删 L1 (避免跨 goroutine race, v1 简化实现)
type nvidiaThrottleL1 struct {
	cache *ristretto.Cache
}

func newNvidiaThrottleL1() *nvidiaThrottleL1 {
	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: nvidiaThrottleL1MaxEntries * 10,
		MaxCost:     nvidiaThrottleL1MaxEntries,
		BufferItems: 64,
	})
	if err != nil || cache == nil {
		return nil
	}
	return &nvidiaThrottleL1{cache: cache}
}

func keyNvidiaThrottleL1(accountID int64, canonicalModel string) string {
	// \x00 作为分隔符，保证 canonicalModel 注入"@" 不会产生 wrong key 碰撞
	return canonicalModel + "\x00" + strconv.FormatInt(accountID, 10)
}

// IsBlocked 命中返回 true, 即调度器应直接返回 block, 不查 L2 也不查上游.
// L1 不可用时返回 false, 调用方降级到 L2.
func (l1 *nvidiaThrottleL1) IsBlocked(accountID int64, canonicalModel string) bool {
	if l1 == nil || l1.cache == nil {
		return false
	}
	_, ok := l1.cache.Get(keyNvidiaThrottleL1(accountID, canonicalModel))
	return ok
}

// MarkBlocked 同步写 L1, 带 8s TTL. 调用方在 L2 命中或上游真 429/capacity 后调用.
func (l1 *nvidiaThrottleL1) MarkBlocked(accountID int64, canonicalModel string) {
	if l1 == nil || l1.cache == nil {
		return
	}
	l1.cache.SetWithTTL(keyNvidiaThrottleL1(accountID, canonicalModel), struct{}{}, 1, nvidiaThrottleL1TTL)
}
