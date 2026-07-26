package service

import "context"

// NvidiaThrottleRate describes a rate-limiting state for one (account, model) pair.
// At the contract level the Go interface exposes the canonical names so callers don't
// need to know the wire format the Lua script expects.
//
// SpacingMs and Consecutive maps to hashed field names the Lua script understands
// (spacing_ms/consecutive_penalty). The repository adapter translates from the port
// type to the Redis hash field names.
type NvidiaThrottleRate struct {
	SpacingMs          int64 // minimum ms between successful sends
	ConsecutivePenalty int64 // consecutive success-or-failure counter
	CapacityPenaltyMs  int64 // 0 or ms to block when capacity is reached
	CapacityWindowMs   int64 // duration of the capacity window being reported
}

// NvidiaReserveResult is the result of an atomic Reserve operation.
type NvidiaCacheReserveResult struct {
	Allowed      bool
	RetryAfterMs int64 // 0 when allowed; positive ms until next eligible send
}

// NvidiaAdaptiveThrottleCache is the Redis-backed adaptive throttling store
// for NVIDIA account+model scheduling.
//
// Key format:
//
//	nvidia:adaptive:<accountID>:<hex(sha256(canonicalModel))[:16]>
//
// Lua scripts are loaded via redis.NewScript, use redis.replicate_commands(),
// and enforce strict field shapes.
type NvidiaAdaptiveThrottleCache interface {
	// Reserve atomically checks whether a send is permitted for the given
	// (accountID, canonicalModel). If allowed, it stamps next_send_at_ms and
	// refreshes the key TTL. Returns the decision and the ms until the next window
	// (0 when allowed).
	Reserve(ctx context.Context, accountID int64, canonicalModel string) (NvidiaCacheReserveResult, error)

	// Apply records a throttling rate transition (success, failed 429, or capacity
	// freeze) and persists the new spacing/penalty/capacity state in one atomic
	// EVAL. TTL is renewed on every call.
	Apply(ctx context.Context, accountID int64, canonicalModel string, rate NvidiaThrottleRate) error
}
