package repository

import "github.com/redis/go-redis/v9"

// nvidiaAdaptiveThrottleKeyPrefix is the Redis key prefix for NVIDIA adaptive throttling.
//
// Full key format: nvidia:adaptive:<accountID>:<hex(sha256(canonicalModel))[:16]>
const nvidiaAdaptiveThrottleKeyPrefix = "nvidia:adaptive:"

// Field names inside the Redis hash used by both Reserve and Apply scripts.
// These MUST stay in sync with the Lua scripts' HMSET/HGETALL field order.
const (
	nvidiaFieldSpacingMs          = "spacing_ms"
	nvidiaFieldConsecutivePenalty = "consecutive_penalty"
	nvidiaFieldCapacityBlockUntil = "capacity_block_until_ms"
	nvidiaFieldRateBlockUntil     = "rate_block_until_ms"
	nvidiaFieldNextSendAt         = "next_send_at_ms"
)

// nvidiaThrottleTTLSec is the TTL (seconds) set on every throttle key. Each
// Reserve/Apply call refreshes this TTL via EXPIRE.
const nvidiaThrottleTTLSec = 48 * 3600 // 48 hours

// nvidiaReserveScript atomically checks and claims a send window.
//
// KEYS[1] = nvidia:adaptive:<accountID>:<hex>
// ARGV[1] = now_ms (int from redis.call('TIME'))
// ARGV[2] = TTL seconds
//
// Returns: {ALLOWED_MS, retry_ms}
//
//	{1, 0}          –allowed, send immediately
//	{0, retry_ms}   –gated, retry in this many ms
var nvidiaReserveScript = redis.NewScript(`
	redis.replicate_commands()

	local key   = KEYS[1]
	local nowMs = tonumber(ARGV[1])
	local ttl   = tonumber(ARGV[2])

	local spacingMs     = tonumber(redis.call('HGET', key, 'spacing_ms') or '0')
	local rateBlock     = tonumber(redis.call('HGET', key, 'rate_block_until_ms') or '0')
	local capacityBlock = tonumber(redis.call('HGET', key, 'capacity_block_until_ms') or '0')
	local nextSend      = tonumber(redis.call('HGET', key, 'next_send_at_ms') or '0')
	local penalty       = tonumber(redis.call('HGET', key, 'consecutive_penalty') or '0')

	local eligible = math.max(rateBlock, capacityBlock, nextSend)
	if eligible > nowMs then
		local retryMs = eligible - nowMs
		if retryMs < 0 then retryMs = 0 end
		return {0, retryMs}
	end

	redis.call('HMSET', key,
		'next_send_at_ms',         nowMs + spacingMs,
		'spacing_ms',              spacingMs,
		'consecutive_penalty',     penalty,
		'capacity_block_until_ms', capacityBlock,
		'rate_block_until_ms',     rateBlock)
	redis.call('EXPIRE', key, ttl)
	return {1, 0}
`)

// nvidiaApplyScript transitions the throttling state after a response (success,
// rate-limit signal, or capacity back-pressure).
//
// KEYS[1] = throttle key
// ARGV[1] = now_ms
// ARGV[2] = TTL seconds
// ARGV[3] = spacing_ms  (0 means do not overwrite existing)
// ARGV[4] = consecutive_penalty
// ARGV[5] = capacity_block_until_ms (0 – leave unchanged)
// ARGV[6] = rate_block_until_ms      (0 – leave unchanged)
var nvidiaApplyScript = redis.NewScript(`
	redis.replicate_commands()

	local key           = KEYS[1]
	local nowMs         = tonumber(ARGV[1])
	local ttlSec        = tonumber(ARGV[2])
	local spacingArg    = tonumber(ARGV[3])
	local penaltyArg    = tonumber(ARGV[4])
	local capacityArg   = tonumber(ARGV[5])
	local rateArg       = tonumber(ARGV[6])

	local oldSpacing  = tonumber(redis.call('HGET', key, 'spacing_ms') or 0)
	local oldCap      = tonumber(redis.call('HGET', key, 'capacity_block_until_ms') or 0)
	local oldRate     = tonumber(redis.call('HGET', key, 'rate_block_until_ms') or 0)

	local newSpacing  = oldSpacing
	if spacingArg > 0 then
		newSpacing = spacingArg
	end

	local newCap = oldCap
	if capacityArg > 0 then
		newCap = capacityArg
	end
	local newRate = oldRate
	if rateArg > 0 then
		newRate = rateArg
	end

	redis.call('HMSET', key,
		'spacing_ms',              newSpacing,
		'consecutive_penalty',     penaltyArg,
		'capacity_block_until_ms', newCap,
		'rate_block_until_ms',     newRate)
	redis.call('EXPIRE', key, ttlSec)
	return 1
`)
