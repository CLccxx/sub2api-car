//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type cachedQuotaResetCoordinator struct {
	calls  int
	action string
	result *OpenAIQuotaResetResult
}

func (c *cachedQuotaResetCoordinator) Run(ctx context.Context, _ int64, _, _ string, _ func(context.Context, string, string) (*OpenAIQuotaResetResult, error)) (*OpenAIQuotaResetResult, error) {
	c.calls++
	c.action = scopedAccountOperation(ctx, "")
	return c.result, nil
}

func TestScopedAdminResetAuthorizesBeforeCachedCoordinatorResult(t *testing.T) {
	for _, targeted := range []bool{false, true} {
		for _, tc := range []struct {
			name, permission string
			group            int64
			allowed          bool
		}{
			{"reset-in-scope", "openai.quota.reset", 1, true},
			{"usage-only", "accounts.usage.read", 1, false},
			{"quota-read-only", "openai.quota.read", 1, false},
			{"reset-out-of-scope", "openai.quota.reset", 2, false},
		} {
			t.Run(fmt.Sprintf("%s/targeted=%t", tc.name, targeted), func(t *testing.T) {
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, GroupIDs: []int64{tc.group}}
				coordinator := &cachedQuotaResetCoordinator{result: &OpenAIQuotaResetResult{}}
				svc := &OpenAIQuotaService{accountRepo: &stubQuotaAccountRepo{accounts: map[int64]*Account{1: account}}, resetCoordinator: coordinator}
				ctx := scopedStatisticsServiceContext(tc.permission)
				var result *OpenAIQuotaResetResult
				var err error
				if targeted {
					result, err = svc.ResetCreditTargeted(ctx, 1, "credit", "request")
				} else {
					result, err = svc.ResetCredit(ctx, 1)
				}
				if tc.allowed {
					require.NoError(t, err)
					require.Same(t, coordinator.result, result)
					require.Equal(t, 1, coordinator.calls)
					require.Equal(t, "openai.quota.reset", coordinator.action)
				} else {
					require.Nil(t, result)
					require.Equal(t, 404, infraerrors.Code(err))
					require.Zero(t, coordinator.calls, "cached reset results must not bypass current account authorization")
				}
			})
		}
	}
}

type subscriptionCostReadStub struct {
	SubscriptionCostRepository
	read func(context.Context, int64, time.Time) (*SubscriptionCostSnapshot, error)
}

func (s *subscriptionCostReadStub) Read(ctx context.Context, id int64, now time.Time) (*SubscriptionCostSnapshot, error) {
	return s.read(ctx, id, now)
}

func TestScopedAdminUsageOnlyIncludesSubscriptionCostProjection(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, GroupIDs: []int64{1}, Credentials: map[string]any{"access_token": "fixture-token", "chatgpt_account_id": "identity"}, Extra: map[string]any{
		"codex_5h_used_percent": 0, "codex_5h_reset_at": now.Add(time.Hour).Format(time.RFC3339),
		"codex_7d_used_percent": 100, "codex_7d_reset_at": now.Add(time.Hour).Format(time.RFC3339), "codex_usage_updated_at": now.Format(time.RFC3339),
	}}
	accounts := &stubQuotaAccountRepo{accounts: map[int64]*Account{1: account}}
	var usageCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method, "usage-only keys must never consume reset credits")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/usage") {
			usageCalls++
			_, _ = fmt.Fprintf(w, `{"account_id":"identity","rate_limit":{"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":%d}}}`, now.Add(time.Hour).Unix())
		} else {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer upstream.Close()
	base, credits := 2000.0, 200.0
	observations := 0
	costRepo := &subscriptionCostReadStub{read: func(ctx context.Context, id int64, _ time.Time) (*SubscriptionCostSnapshot, error) {
		observations++
		require.Equal(t, "accounts.usage.read", scopedAccountOperation(ctx, ""))
		require.Equal(t, int64(1), id)
		return &SubscriptionCostSnapshot{
			Observation: CarpoolResetObservation{ResetAt: now.Add(time.Hour)},
			Estimate:    &SubscriptionCostEstimate{EstimatedTotalCost: &base, CreditsUsedCost: &credits, EstimateFrozen: true, EstimateSource: "observed"},
		}, nil
	}}
	quota := NewOpenAIQuotaService(accounts, nil, NewOpenAITokenProvider(accounts, nil, nil), newQuotaRedirectingFactory(upstream), nil)
	quota.subscriptionCost = NewSubscriptionCostService(costRepo)
	svc := &AccountUsageService{accountRepo: accounts, openAIQuotaService: quota, cache: NewUsageCache()}
	usage, err := svc.GetUsageForAccount(scopedStatisticsServiceContext("accounts.usage.read"), account, false)
	require.NoError(t, err)
	require.Zero(t, usageCalls, "the admin projection must not trigger an extra upstream estimate query")
	require.Equal(t, 1, observations)
	require.NotNil(t, usage.SevenDay)
	require.Equal(t, &base, usage.SevenDay.EstimatedTotalCost)
	require.Equal(t, &credits, usage.SevenDay.CreditsUsedCost)
	require.True(t, usage.SevenDay.EstimateFrozen)
	require.Equal(t, "observed", usage.SevenDay.EstimateSource)
	require.Zero(t, accounts.extraUpdateCalls)
}
