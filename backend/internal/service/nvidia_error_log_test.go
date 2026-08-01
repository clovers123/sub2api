//go:build unit

package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestNvidiaErrorLogBodyExcerpt_EmptyBody(t *testing.T) {
	require.Equal(t, "", nvidiaErrorLogBodyExcerpt(nil))
	require.Equal(t, "", nvidiaErrorLogBodyExcerpt([]byte{}))
}

func TestNvidiaErrorLogBodyExcerpt_NVAPIKeyRedactedBeforeGenericRedact(t *testing.T) {
	body := []byte(`{"error":"upstream returned nvapi-AbCdEf1234567890Xyz_invalid","authorization":"api_key nvapi-AbCdEf1234567890Xyz"}`)
	out := nvidiaErrorLogBodyExcerpt(body)
	require.Contains(t, out, "nvapi-***")
	require.NotContains(t, out, "nvapi-AbCdEf1234567890Xyz", "raw nvapi key must be gone")
}

func TestNvidiaErrorLogBodyExcerpt_BearerRedactedBeforeGenericRedact(t *testing.T) {
	body := []byte(`{"error":"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abc.def got rejected"}`)
	out := nvidiaErrorLogBodyExcerpt(body)
	require.Contains(t, out, "Bearer ***")
	require.NotContains(t, out, "eyJhbGciOiJIUzI1NiJ9", "raw JWT body must be gone")
}

func TestNvidiaErrorLogBodyExcerpt_TruncationKeepsUTF8Boundary(t *testing.T) {
	han := "吧"
	require.Equal(t, 3, len(han), "test fixture: '吧' must be 3 UTF-8 bytes")
	overflow := strings.Repeat(han, 200) // 600 bytes > 512-byte budget
	raw := []byte(overflow)
	out := nvidiaErrorLogBodyExcerpt(raw)
	require.True(t, strings.HasSuffix(out, nvidiaErrorLogTruncateSuffix), "must end with truncation suffix; got tail=%q", tail(out, 20))
	require.LessOrEqual(t, len(out), nvidiaErrorLogMaxBodyExcerptBytes, "excerpt must not exceed budget")

	prefix := strings.TrimSuffix(out, nvidiaErrorLogTruncateSuffix)
	require.Greater(t, len(prefix), 0)
	require.Equal(t, len(prefix)%3, 0, "kept prefix is a multiple of full 3-byte runes (no half-rune)")
	require.True(t, utf8.ValidString(prefix), "kept prefix must be valid UTF-8 (no broken rune)")
}

func TestNvidiaErrorLogBodyExcerpt_ShortBodyNotTruncated(t *testing.T) {
	body := []byte(`{"error":"not big enough"}`)
	out := nvidiaErrorLogBodyExcerpt(body)
	require.Equal(t, string(body), out)
}

// Known prod behaviour: redact pipeline waves chain — nvidiaErrorLogNVAPIKeyRE runs
// first (→ "nvapi-***"), then nvidiaErrorLogBearerRE → if Bearer was followed by an
// nvapi- key the key has already gone and only the literal "Bearer " prefix remains,
// which the generic logredact.redactText(authorization,...) final pass turns into
// "Authorization: *** ***" (replacing the literal "Bearer" token entirely). The
// expected end state is therefore NOT "Bearer ***" inside the line. Locking the
// actual prod behaviour so future refactorings keep this same ordering invariant.
func TestNvidiaErrorLogBodyExcerpt_NVAPIAfterBearerGoldenOrder(t *testing.T) {
	out := nvidiaErrorLogBodyExcerpt([]byte("Authorization: Bearer nvapi-SOMETHINGSECRET18charsGoing"))
	require.Contains(t, out, "nvapi-***", "nvapi key must be redacted")
	require.NotContains(t, out, "SOMETHING", "raw secret must be gone")
	require.Contains(t, out, "Authorization: ***", "generic redact must scrub heading later")
}

// Known prod behaviour: nvapi- regex requires [0-9A-Za-z_-]{16,} AFTER the prefix;
// inserting two consecutive "nvapi-AAAA" (only 4 letters after each prefix) does NOT
// match the 16+ char minimum and so the key is NOT redacted. Locking prod contract.
func TestNvidiaErrorLogBodyExcerpt_ShortNvapiSuffixNOTRedacted(t *testing.T) {
	out := nvidiaErrorLogBodyExcerpt([]byte("nvapi-AAAAnvapi-AAAA"))
	require.Equal(t, "nvapi-AAAAnvapi-AAAA", out, "short (<16 chars after nvapi-) is left as-is by prod regex")
}

func TestLogNVIDIAUpstreamError_DoesNotPanicOnNilAccount(t *testing.T) {
	require.NotPanics(t, func() {
		logNVIDIAUpstreamError(context.Background(), nil, http.StatusInternalServerError, []byte("oops"), nvidiaReason5xxUpstream)
	})
}

func TestPlatformForLog_ToleratesNil(t *testing.T) {
	require.Equal(t, "", platformForLog(nil))
	require.Equal(t, PlatformOpenAI, platformForLog(&Account{Platform: PlatformOpenAI}))
}

func TestAccountIDForLog_ToleratesNil(t *testing.T) {
	require.Equal(t, int64(0), accountIDForLog(nil))
	require.Equal(t, int64(42), accountIDForLog(&Account{ID: 42}))
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
