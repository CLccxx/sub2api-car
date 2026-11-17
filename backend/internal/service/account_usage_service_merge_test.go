package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type accountUsageMergeCostRepo struct {
	snapshot *SubscriptionCostSnapshot
}

func (r *accountUsageMergeCostRepo) Read(context.Context, int64, time.Time) (*SubscriptionCostSnapshot, error) {
	return r.snapshot, nil
}

func (*accountUsageMergeCostRepo) Sample(context.Context, int64, string, CarpoolResetObservation, time.Time) (*SubscriptionCostSnapshot, error) {
	panic("reading account usage must not sample subscription cost")
}

func (*accountUsageMergeCostRepo) Candidates(context.Context, int64, int) ([]int64, error) {
	panic("reading account usage must not enumerate sampling candidates")
}

func TestAccountUsageMerge_SubscriptionEstimateKeepsObservationAndCreditSeparation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	observedAt := now.Add(-time.Hour)
	reset := now.Add(24 * time.Hour)
	allowance, credits := 120.0, 35.0
	for _, sameWindow := range []bool{true, false} {
		t.Run(fmt.Sprint(sameWindow), func(t *testing.T) {
			sampleReset := reset
			if !sameWindow {
				sampleReset = reset.Add(2 * time.Hour)
			}
			repo := &accountUsageMergeCostRepo{snapshot: &SubscriptionCostSnapshot{
				Observation: CarpoolResetObservation{ObservedAt: observedAt.Add(-time.Minute), ResetAt: sampleReset, Utilization: 100},
				Estimate: &SubscriptionCostEstimate{
					EstimatedTotalCost: &allowance, CreditsUsedCost: &credits,
					EstimateFrozen: true, EstimateSource: "observed",
				},
			}}
			svc := &AccountUsageService{openAIQuotaService: &OpenAIQuotaService{subscriptionCost: NewSubscriptionCostService(repo)}}
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
				"codex_5h_used_percent":  25.0,
				"codex_7d_used_percent":  100.0,
				"codex_7d_reset_at":      reset.Format(time.RFC3339),
				"codex_usage_updated_at": observedAt.Format(time.RFC3339),
			}}
			usage, err := svc.getOpenAIUsage(context.Background(), account, false)
			require.NoError(t, err)
			require.Equal(t, "passive", usage.Source)
			require.NotNil(t, usage.UpdatedAt)
			require.True(t, observedAt.Equal(*usage.UpdatedAt), "attaching cost estimates must not refresh quota observation time")
			require.NotNil(t, usage.SevenDay)
			require.False(t, usage.SevenDay.UtilizationUnknown)
			require.Equal(t, 100.0, usage.SevenDay.Utilization)
			if sameWindow {
				require.Equal(t, &allowance, usage.SevenDay.EstimatedTotalCost)
				require.Equal(t, &credits, usage.SevenDay.CreditsUsedCost)
				require.True(t, usage.SevenDay.EstimateFrozen)
				require.Equal(t, "observed", usage.SevenDay.EstimateSource)
			} else {
				require.Nil(t, usage.SevenDay.EstimatedTotalCost, "a different cycle's allowance must not be reused")
				require.Nil(t, usage.SevenDay.CreditsUsedCost)
			}
		})
	}
}

func TestAccountUsageMerge_UnknownQuotaDoesNotBecomeZeroAllowance(t *testing.T) {
	t.Parallel()
	svc := &AccountUsageService{
		usageLogRepo:       &usageBatchLogRepoStub{},
		openAIQuotaService: &OpenAIQuotaService{subscriptionCost: NewSubscriptionCostService(&accountUsageMergeCostRepo{})},
	}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	usage, err := svc.getOpenAIUsage(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, errorCodeQuotaRefreshFailed, usage.ErrorCode)
	require.Equal(t, "passive", usage.Source)
	require.Nil(t, usage.UpdatedAt)
	require.Nil(t, account.Extra, "failed refresh must not persist synthetic zero quota observations")
	for name, window := range map[string]*UsageProgress{"5h": usage.FiveHour, "7d": usage.SevenDay} {
		t.Run(name, func(t *testing.T) {
			require.NotNil(t, window)
			require.NotNil(t, window.WindowStats)
			require.True(t, window.UtilizationUnknown)
			require.Nil(t, window.ResetsAt)
			body, err := json.Marshal(window)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(body, &fields))
			require.Equal(t, true, fields["utilization_unknown"])
			require.Contains(t, fields, "estimated_total_cost")
			require.Nil(t, fields["estimated_total_cost"], "unknown allowance is null, never an inferred zero-dollar limit")
			require.Contains(t, fields, "credits_used_cost")
			require.Nil(t, fields["credits_used_cost"])
		})
	}
}

func TestAccountUsageMerge_ResetObservationRequiresExplicitUsage(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(time.Hour)
	for _, tc := range []struct {
		name     string
		percent  string
		observed bool
	}{
		{name: "missing"},
		{name: "null", percent: `"used_percent":null,`},
		{name: "explicit zero", percent: `"used_percent":0,`, observed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage OpenAIQuotaUsage
			body := fmt.Sprintf(`{"account_id":"shared-account","rate_limit":{"secondary_window":{%s"limit_window_seconds":604800,"reset_at":%d}}}`, tc.percent, reset.Unix())
			require.NoError(t, json.Unmarshal([]byte(body), &usage))
			usage.FetchedAt = now.Unix()
			observation, ok := ReadCarpoolResetObservation(&usage, "shared-account", now)
			require.Equal(t, tc.observed, ok)
			if ok {
				previous := &CarpoolResetBaseline{Observation: CarpoolResetObservation{
					ObservedAt: now.Add(-time.Minute), ResetAt: reset, Utilization: 95,
				}, Since: now.Add(-time.Hour)}
				_, reason := AdvanceCarpoolResetBaseline(previous, observation)
				require.Empty(t, reason, "even an explicit percentage drop is not proof of a reset without a receipt or advancing window")
			}
		})
	}
}
