package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func tradeQuotaSubscription(base, adjustment, frozen, used float64) *UserSubscription {
	return &UserSubscription{
		ID: 10, UserID: 20, GroupID: 30, Status: SubscriptionStatusActive,
		StartsAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
		WeeklyWindowStart: ptrTime(time.Now().Add(-time.Hour)),
		WeeklyUsageUSD:    used, QuotaAdjustmentUSD: adjustment, QuotaFrozenUSD: frozen,
		Group: &Group{ID: 30, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription,
			CarpoolMembers: 4, WeeklyLimitUSD: &base},
	}
}

func TestCarpoolSubscriptionQuota_ListingAndDynamicBase(t *testing.T) {
	sub := tradeQuotaSubscription(400, 0, 50, 0)
	require.Equal(t, 350.0, *sub.EffectiveWeeklyLimitUSD(sub.Group))
	require.True(t, sub.CheckWeeklyLimit(sub.Group, 350))
	require.False(t, sub.CheckWeeklyLimit(sub.Group, 350.01))

	staleAuthGroup := *sub.Group
	sub.Group.WeeklyLimitUSD = ptrFloat64(370)
	require.Equal(t, 320.0, *sub.EffectiveWeeklyLimitUSD(&staleAuthGroup))
	require.False(t, sub.CheckWeeklyLimit(&staleAuthGroup, 321))

	// Confirmation converts the hold into a sale, rather than deducting twice.
	sub.QuotaFrozenUSD = 0
	sub.QuotaAdjustmentUSD = -50
	require.Equal(t, 320.0, *sub.EffectiveWeeklyLimitUSD(sub.Group))
	buyer := tradeQuotaSubscription(370, 50, 0, 0)
	require.Equal(t, 420.0, *buyer.EffectiveWeeklyLimitUSD(buyer.Group))

	// Withdrawing releases only the listing, not actual usage or other trades.
	sub.QuotaAdjustmentUSD = 0
	sub.WeeklyUsageUSD = 100
	require.Equal(t, 270.0, *sub.EffectiveWeeklyLimitUSD(sub.Group)-sub.WeeklyUsageUSD)
}

func TestCarpoolSubscriptionQuota_ExhaustedLimitHasFiniteProgress(t *testing.T) {
	sub := tradeQuotaSubscription(30, -20, 50, 5)
	require.Equal(t, 0.0, *sub.EffectiveWeeklyLimitUSD(sub.Group))
	require.False(t, sub.CheckWeeklyLimit(sub.Group, 0))
	progress := (&SubscriptionService{}).calculateProgress(sub, sub.Group)
	require.NotNil(t, progress.Weekly)
	require.Equal(t, 0.0, progress.Weekly.LimitUSD)
	require.Equal(t, 0.0, progress.Weekly.RemainingUSD)
	require.Equal(t, 100.0, progress.Weekly.Percentage)
}

type freshCarpoolSubscriptionRepo struct {
	userSubRepoNoop
	sub   *UserSubscription
	err   error
	reads int
}

func (r *freshCarpoolSubscriptionRepo) GetActiveCarpoolSubscription(context.Context, int64, int64) (*UserSubscription, bool, error) {
	r.reads++
	return r.sub, true, r.err
}

func (r *freshCarpoolSubscriptionRepo) GetActiveByUserIDAndGroupID(context.Context, int64, int64) (*UserSubscription, error) {
	return r.sub, r.err
}

func TestCarpoolSubscriptionQuota_BypassesOldSubscriptionCaches(t *testing.T) {
	repo := &freshCarpoolSubscriptionRepo{sub: tradeQuotaSubscription(400, 0, 0, 0)}
	svc := &SubscriptionService{userSubRepo: repo}
	first, err := svc.GetActiveSubscription(context.Background(), 20, 30)
	require.NoError(t, err)
	require.Equal(t, 400.0, *first.EffectiveWeeklyLimitUSD(first.Group))
	repo.sub = tradeQuotaSubscription(370, 0, 50, 0)
	second, err := svc.GetActiveSubscription(context.Background(), 20, 30)
	require.NoError(t, err)
	require.Equal(t, 320.0, *second.EffectiveWeeklyLimitUSD(second.Group))
	require.Equal(t, 2, repo.reads)

	repo.err = errors.New("database unavailable")
	_, err = svc.GetActiveSubscription(context.Background(), 20, 30)
	require.Error(t, err, "must not fall back to stale quota on a failed authoritative read")
}

func TestCarpoolSubscriptionQuota_BillingUsesFreshLimits(t *testing.T) {
	sub := tradeQuotaSubscription(370, -50, 0, 320)
	repo := &freshCarpoolSubscriptionRepo{sub: sub}
	svc := &BillingCacheService{subRepo: repo}
	staleGroup := *sub.Group
	staleGroup.WeeklyLimitUSD = ptrFloat64(400)
	err := svc.checkSubscriptionEligibility(context.Background(), 20, &staleGroup, sub)
	require.ErrorIs(t, err, ErrWeeklyLimitExceeded)
	require.ErrorIs(t, svc.checkFixedChargeSubscription(context.Background(), 20, &staleGroup, 1), ErrWeeklyLimitExceeded)

	repo.sub = tradeQuotaSubscription(370, 50, 0, 400)
	require.NoError(t, svc.checkSubscriptionEligibility(context.Background(), 20, &staleGroup, repo.sub))
	require.NoError(t, svc.checkFixedChargeSubscription(context.Background(), 20, &staleGroup, 20))
	require.ErrorIs(t, svc.checkFixedChargeSubscription(context.Background(), 20, &staleGroup, 20.01), ErrWeeklyLimitExceeded)

	repo.err = errors.New("ledger unavailable")
	require.ErrorIs(t, svc.checkSubscriptionEligibility(context.Background(), 20, &staleGroup, repo.sub), ErrBillingServiceUnavailable)
}

func TestCarpoolSubscriptionQuota_PersonalWindowDoesNotEndAccountTrades(t *testing.T) {
	sub := tradeQuotaSubscription(400, -50, 0, 300)
	sub.WeeklyWindowStart = ptrTime(time.Now().Add(-8 * 24 * time.Hour))
	svc := &SubscriptionService{now: time.Now}
	needsMaintenance, err := svc.ValidateAndCheckLimits(sub, sub.Group)
	require.NoError(t, err)
	require.True(t, needsMaintenance)
	require.Equal(t, 0.0, sub.WeeklyUsageUSD)
	require.Equal(t, -50.0, sub.QuotaAdjustmentUSD)
	require.Equal(t, 350.0, *sub.EffectiveWeeklyLimitUSD(sub.Group))
}
