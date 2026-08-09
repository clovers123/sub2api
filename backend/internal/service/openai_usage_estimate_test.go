//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEstimateOpenAIUsageFallback(t *testing.T) {
	tests := []struct {
		name         string
		inputBytes   int
		outputBytes  int
		wantInputTok int
		wantOutputTok int
	}{
		{"zero bytes maps to zero tokens", 0, 0, 0, 0},
		{"byte ratio /4 for input", 100, 0, 25, 0},
		{"byte ratio /4 for output", 0, 80, 0, 20},
		{"both sides estimated", 40, 52, 10, 13},
		{"sub-4 bytes rounds down to zero", 3, 3, 0, 0},
		{"mixed rounding truncation", 7, 9, 1, 2},
		{"large payload keeps /4 ratio", 4000, 2000, 1000, 500},
		{"unrelated usage fields stay zero", 60, 60, 15, 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateOpenAIUsageFallback(tt.inputBytes, tt.outputBytes)
			require.Equal(t, tt.wantInputTok, got.InputTokens)
			require.Equal(t, tt.wantOutputTok, got.OutputTokens)
			require.Zero(t, got.ImageInputTokens, "estimate must not fabricate image tokens")
			require.Zero(t, got.CacheCreationInputTokens)
			require.Zero(t, got.CacheReadInputTokens)
			require.Zero(t, got.ImageOutputTokens)
		})
	}
}

func TestOpenAIUsageIsEmpty(t *testing.T) {
	require.True(t, openAIUsageIsEmpty(OpenAIUsage{}))

	require.False(t, openAIUsageIsEmpty(OpenAIUsage{InputTokens: 1}))
	require.False(t, openAIUsageIsEmpty(OpenAIUsage{OutputTokens: 1}))
	require.False(t, openAIUsageIsEmpty(OpenAIUsage{InputTokens: 5, OutputTokens: 6}))
	require.False(t, openAIUsageIsEmpty(OpenAIUsage{ImageInputTokens: 2}))
	require.False(t, openAIUsageIsEmpty(OpenAIUsage{CacheReadInputTokens: 2}))
	// Cache-creation-only responses are still non-empty usage (billing-relevant).
	require.False(t, openAIUsageIsEmpty(OpenAIUsage{CacheCreationInputTokens: 2}))
}