package service

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

// nvidiaErrorLogTruncateSuffix is appended to truncated excerpts to indicate
// truncation. Kept here (not inlined) so the truncation length budget accounts
// for it deterministically.
const nvidiaErrorLogTruncateSuffix = "...(truncated)"

// nvidiaErrorLogMaxBodyExcerptBytes is the upper bound on the redacted body
// excerpt we attach to a single NVIDIA upstream-error log line, INCLUDING the
// truncation suffix. NVIDIA 4xx/5xx bodies can be large; we cap to keep log
// lines bounded.
const nvidiaErrorLogMaxBodyExcerptBytes = 512

// NVIDIA-specific secret patterns that logredact does not cover by default.
// nvapi- is NVIDIA's official API key prefix; Bearer matches the OAuth-style
// header commonly embedded in error bodies by misconfigured proxies.
var (
	nvidiaErrorLogNVAPIKeyRE = regexp.MustCompile(`nvapi-[0-9A-Za-z_-]{16,}`)
	nvidiaErrorLogBearerRE   = regexp.MustCompile(`(?i)Bearer\s+[0-9A-Za-z._\-]{8,}`)
)

// logNvidiaUpstreamError emits a uniform, desensitized warning for any NVIDIA
// upstream failure. Every NVIDIA error path MUST funnel through this helper so
// that:
//   - account credentials (api_key, bearer, OAuth tokens, nvapi-*) never appear in raw form
//   - bodies are truncated to a bounded size
//   - log lines carry a uniform field set for grep / alerting
//
// Always pass the redacted body through logredact.RedactText even when it looks
// like a JSON error: a misconfigured proxy can embed Authorization headers in
// the error body.
func logNvidiaUpstreamError(ctx context.Context, account *Account, statusCode int, body []byte, reason string) {
	fields := []any{
		slog.String("platform", platformForLog(account)),
		slog.Int64("account_id", accountIDForLog(account)),
		slog.String("reason", reason),
		slog.Int("status_code", statusCode),
	}
	if excerpt := nvidiaErrorLogBodyExcerpt(body); excerpt != "" {
		fields = append(fields, slog.String("body_excerpt", excerpt))
	}
	slog.WarnContext(ctx, "nvidia_upstream_error", fields...)
}

// platformForLog returns the platform string for log fields, tolerating nil accounts.
func platformForLog(account *Account) string {
	if account == nil {
		return ""
	}
	return account.Platform
}

// accountIDForLog returns the account id for log fields, tolerating nil accounts.
func accountIDForLog(account *Account) int64 {
	if account == nil {
		return 0
	}
	return account.ID
}

// nvidiaErrorLogBodyExcerpt redacts and truncates a response body for safe logging.
// Returns "" when the body is empty so callers can omit the field.
//
// Order matters: NVIDIA-specific patterns run BEFORE logredact because
// logredact's rePlain truncates at the first whitespace after a known key
// (e.g. "Authorization: Bearer ABC..." becomes "Authorization: *** ABC..."),
// which would leak the token. By handling Bearer/nvapi- ourselves first we
// ensure the raw credential is gone before any other rewriter runs.
func nvidiaErrorLogBodyExcerpt(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	redacted := string(body)
	redacted = nvidiaErrorLogNVAPIKeyRE.ReplaceAllString(redacted, "nvapi-***")
	redacted = nvidiaErrorLogBearerRE.ReplaceAllString(redacted, "Bearer ***")
	redacted = logredact.RedactText(redacted, "authorization", "api_key", "apikey", "token", "secret")
	redacted = strings.TrimSpace(redacted)
	if redacted == "" {
		return ""
	}
	if len(redacted) > nvidiaErrorLogMaxBodyExcerptBytes {
		keep := nvidiaErrorLogMaxBodyExcerptBytes - len(nvidiaErrorLogTruncateSuffix)
		if keep < 0 {
			keep = 0
		}
		redacted = redacted[:keep] + nvidiaErrorLogTruncateSuffix
	}
	return redacted
}
