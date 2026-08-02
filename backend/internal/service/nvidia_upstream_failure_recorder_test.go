//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRecordNVIDIASingle5xxHeatPenalty_Classification 覆盖 N4:
//   - 500 → 10 个虚拟失败计数（nvidiaSingle5xx500HeatPenaltyCount）
//   - 502/503/504 → 5 个虚拟失败计数（nvidiaSingle5xxTransientHeatPenaltyCount）
//   - 0 或未知 → 维持 5 个（向后兼容）
//   - nil account / nil s → 安全跳过
func TestRecordNVIDIASingle5xxHeatPenalty_Classification(t *testing.T) {
	account := rawChatCompletionsNVIDIATestAccount()

	cases := []struct {
		name       string
		statusCode int
		wantPenalty int64
	}{
		{"500_internal加重10", http.StatusInternalServerError, nvidiaSingle5xx500HeatPenaltyCount},
		{"502_bad_gateway轻5", http.StatusBadGateway, nvidiaSingle5xxTransientHeatPenaltyCount},
		{"503_service_unavailable轻5", http.StatusServiceUnavailable, nvidiaSingle5xxTransientHeatPenaltyCount},
		{"504_gateway_timeout轻5", http.StatusGatewayTimeout, nvidiaSingle5xxTransientHeatPenaltyCount},
		{"0_unknown向后兼容5", 0, nvidiaSingle5xxTransientHeatPenaltyCount},
		{"520_unknown向后兼容5", 520, nvidiaSingle5xxTransientHeatPenaltyCount},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{}
			metrics := NewNVIDIASharedConnectionPoolMetrics()
			svc.SetNVIDIASharedPoolMetricsForSorting(metrics)

			recordNVIDIASingle5xxHeatPenalty(svc, account, tc.statusCode)

			snap := metrics.SnapshotAccountReuse(account.ID)
			require.Equal(t, tc.wantPenalty, snap.RecentFailCount,
				"statusCode=%d 应注入 %d 个虚拟失败计数，实注入 %d",
				tc.statusCode, tc.wantPenalty, snap.RecentFailCount)
		})
	}
}

func TestRecordNVIDIASingle5xxHeatPenalty_NilGuards(t *testing.T) {
	// nil account 不 panic
	recordNVIDIASingle5xxHeatPenalty(nil, nil, http.StatusInternalServerError)
	recordNVIDIASingle5xxHeatPenalty(&OpenAIGatewayService{}, nil, http.StatusInternalServerError)

	// nil svc.metrics 不 panic
	svc := &OpenAIGatewayService{}
	recordNVIDIASingle5xxHeatPenalty(svc, rawChatCompletionsNVIDIATestAccount(), http.StatusInternalServerError)
}

// 500 应注入比 502 更多的失败计数 → heat 排序差异显著
func TestRecordNVIDIASingle5xxHeatPenalty_500HeavierThan502(t *testing.T) {
	metrics := NewNVIDIASharedConnectionPoolMetrics()
	svcA := &OpenAIGatewayService{}
	svcA.SetNVIDIASharedPoolMetricsForSorting(metrics)

	accountA := rawChatCompletionsNVIDIATestAccount()
	accountA.ID = 1001

	accountB := rawChatCompletionsNVIDIATestAccount()
	accountB.ID = 1002

	recordNVIDIASingle5xxHeatPenalty(svcA, accountA, http.StatusInternalServerError)
	recordNVIDIASingle5xxHeatPenalty(svcA, accountB, http.StatusBadGateway)

	snapA := metrics.SnapshotAccountReuse(accountA.ID)
	snapB := metrics.SnapshotAccountReuse(accountB.ID)
	require.Greater(t, snapA.RecentFailCount, snapB.RecentFailCount,
		"500 惩罚应重于 502（10 vs 5），实 500=%d 502=%d",
		snapA.RecentFailCount, snapB.RecentFailCount)
}
