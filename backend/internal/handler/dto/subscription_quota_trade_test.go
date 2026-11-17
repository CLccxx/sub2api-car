package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionQuotaTradeFields(t *testing.T) {
	base := 370.0
	sub := &service.UserSubscription{
		ID: 1, GroupID: 10, QuotaFrozenUSD: 50,
		Group: &service.Group{ID: 10, Platform: service.PlatformOpenAI,
			SubscriptionType: service.SubscriptionTypeSubscription, CarpoolMembers: 4, WeeklyLimitUSD: &base},
	}
	out := UserSubscriptionFromService(sub)
	require.True(t, out.QuotaTradingEnabled)
	require.Equal(t, 50.0, out.QuotaFrozenUSD)
	require.NotNil(t, out.EffectiveWeeklyLimitUSD)
	require.Equal(t, 320.0, *out.EffectiveWeeklyLimitUSD)
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "carpool_members", "member configuration remains admin-only")

	plain, err := json.Marshal(UserSubscriptionFromService(&service.UserSubscription{ID: 2}))
	require.NoError(t, err)
	for _, field := range []string{"quota_trading_enabled", "quota_adjustment_usd", "quota_frozen_usd", "effective_weekly_limit_usd"} {
		require.NotContains(t, string(plain), field, "unchanged subscriptions retain their previous API shape")
	}
}
