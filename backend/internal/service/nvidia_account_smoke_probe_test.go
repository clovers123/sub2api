//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProbeNVIDIAAccountSmoke(t *testing.T) {
	newNVIDIAAccount := func(apiKey string) *Account {
		return &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"base_url": "https://integrate.api.nvidia.com",
				"api_key":  apiKey,
			},
		}
	}

	t.Run("ok_200_accepted", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			require.Equal(t, "/v1/models", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"z-ai/glm-5.2"}]}`))
		}))
		defer srv.Close()
		acc := newNVIDIAAccount("nvapi-test-1234567890")
		// 改 base_url 指向本地 server（保留 hostname 匹配的测试语义
		// 实际 hostname 判断用 integrate.api.nvidia.com 会走网络，这里直接换）
		acc.Credentials["base_url"] = srv.URL

		res := ProbeNVIDIAAccountSmoke(context.Background(), srv.Client(), acc)
		require.Equal(t, NVIDIASmokeProbeOutcomeOK, res.Outcome)
		require.False(t, res.SmokeProbeBlocking())
		require.Equal(t, "Bearer nvapi-test-1234567890", gotAuth)
	})

	t.Run("unauthorized_401_rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key"}}`))
		}))
		defer srv.Close()
		acc := newNVIDIAAccount("nvapi-bad-key-0000000000")
		acc.Credentials["base_url"] = srv.URL

		res := ProbeNVIDIAAccountSmoke(context.Background(), srv.Client(), acc)
		require.Equal(t, NVIDIASmokeProbeOutcomeUnauthorized, res.Outcome)
		require.True(t, res.SmokeProbeBlocking())
		require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	})

	t.Run("forbidden_403_rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		acc := newNVIDIAAccount("nvapi-bad-key-0000000000")
		acc.Credentials["base_url"] = srv.URL

		res := ProbeNVIDIAAccountSmoke(context.Background(), srv.Client(), acc)
		require.Equal(t, NVIDIASmokeProbeOutcomeUnauthorized, res.Outcome)
		require.True(t, res.SmokeProbeBlocking())
	})

	t.Run("upstream_5xx_non_blocking_warning", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		acc := newNVIDIAAccount("nvapi-test-1234567890")
		acc.Credentials["base_url"] = srv.URL

		res := ProbeNVIDIAAccountSmoke(context.Background(), srv.Client(), acc)
		require.Equal(t, NVIDIASmokeProbeOutcomeUpstreamError, res.Outcome)
		require.False(t, res.SmokeProbeBlocking())
		require.Equal(t, http.StatusServiceUnavailable, res.StatusCode)
	})

	t.Run("network_error_timeout_non_blocking", func(t *testing.T) {
		// 不启动 server → 连接拒绝即 network_error
		acc := newNVIDIAAccount("nvapi-test-1234567890")
		acc.Credentials["base_url"] = "http://127.0.0.1:1"

		client := &http.Client{Timeout: 2 * time.Second}
		res := ProbeNVIDIAAccountSmoke(context.Background(), client, acc)
		require.Equal(t, NVIDIASmokeProbeOutcomeNetworkError, res.Outcome)
		require.False(t, res.SmokeProbeBlocking())
	})

	t.Run("missing_api_key_invalid", func(t *testing.T) {
		acc := newNVIDIAAccount("")
		acc.Credentials["base_url"] = "https://integrate.api.nvidia.com"
		res := ProbeNVIDIAAccountSmoke(context.Background(), http.DefaultClient, acc)
		require.Equal(t, NVIDIASmokeProbeOutcomeInvalid, res.Outcome)
	})

	t.Run("nil_account_invalid", func(t *testing.T) {
		res := ProbeNVIDIAAccountSmoke(context.Background(), http.DefaultClient, nil)
		require.Equal(t, NVIDIASmokeProbeOutcomeInvalid, res.Outcome)
	})
}

func TestShouldSmokeProbeNVIDIAAccount(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{
			name: "nvidia_openai_apikey_true",
			account: &Account{
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"base_url": "https://integrate.api.nvidia.com", "api_key": "nvapi-x"},
			},
			want: true,
		},
		{
			name: "non_nvidia_hostname_false",
			account: &Account{
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"base_url": "https://api.openai.com", "api_key": "sk-x"},
			},
			want: false,
		},
		{
			name: "oauth_type_false",
			account: &Account{
				Platform:    PlatformOpenAI,
				Type:        AccountTypeOAuth,
				Credentials: map[string]any{"base_url": "https://integrate.api.nvidia.com"},
			},
			want: false,
		},
		{
			name: "nil_false",
			account: nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, shouldSmokeProbeNVIDIAAccount(tt.account))
		})
	}
}

func TestNVIDIASmokeProbeOutcomeString(t *testing.T) {
	require.Equal(t, "ok", NVIDIASmokeProbeOutcomeOK.String())
	require.Equal(t, "unauthorized", NVIDIASmokeProbeOutcomeUnauthorized.String())
	require.Equal(t, "upstream_error", NVIDIASmokeProbeOutcomeUpstreamError.String())
	require.Equal(t, "network_error", NVIDIASmokeProbeOutcomeNetworkError.String())
	require.Equal(t, "invalid", NVIDIASmokeProbeOutcomeInvalid.String())
}
