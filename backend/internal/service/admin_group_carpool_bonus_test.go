//go:build unit

package service

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestCarpoolGroupBonusCreate(t *testing.T) {
	for _, bonus := range []*float64{nil, new(100.0), new(150.0), new(0.0), new(150.25), new(0.29)} {
		t.Run(fmt.Sprint(bonus), func(t *testing.T) {
			svc := &adminServiceImpl{groupRepo: &groupRepoStubForAdmin{}}
			got, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
				Name: "carpool", Platform: PlatformOpenAI, RateMultiplier: 1,
				SubscriptionType: SubscriptionTypeSubscription, CarpoolMembers: 4, CarpoolBonusUSD: bonus,
			})
			require.NoError(t, err)
			want := DefaultCarpoolBonusUSD
			if bonus != nil {
				want = *bonus
			}
			require.Equal(t, want, got.CarpoolBonusUSD)
			require.InDelta(t, 400+want, *got.WeeklyLimitUSD, 1e-8)
		})
	}
}

func TestCarpoolGroupBonusUpdate(t *testing.T) {
	for _, bonus := range []*float64{nil, new(100.0), new(150.0), new(0.0), new(150.25), new(0.29)} {
		t.Run(fmt.Sprint(bonus), func(t *testing.T) {
			limit := 445.0
			synced := time.Date(2026, 11, 17, 3, 30, 0, 0, time.UTC)
			repo := &groupPlatformRepoStub{group: &Group{
				ID: 7, Name: "carpool", Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription,
				CarpoolMembers: 4, CarpoolBonusUSD: 100, WeeklyLimitUSD: &limit, CarpoolQuotaSyncedAt: &synced,
			}}
			cache := &carpoolCacheSpy{}
			svc := &adminServiceImpl{groupRepo: repo, authCacheInvalidator: cache}
			// The form's readonly cap is not authoritative.
			got, err := svc.UpdateGroup(context.Background(), 7, &UpdateGroupInput{CarpoolBonusUSD: bonus, WeeklyLimitUSD: new(9999.0)})
			require.NoError(t, err)
			want := 100.0
			if bonus != nil {
				want = *bonus
			}
			require.Equal(t, want, got.CarpoolBonusUSD)
			require.InDelta(t, 345+want, *got.WeeklyLimitUSD, 1e-8)
			require.True(t, got.PreserveCarpoolWeeklyLimit)
			require.Equal(t, want != 100, got.AdjustCarpoolBonus)
			require.Equal(t, &synced, got.CarpoolQuotaSyncedAt)
			require.Equal(t, []int64{7}, cache.ids)
			if bonus == nil {
				require.Nil(t, got.PreviousCarpoolBonusUSD)
			} else {
				require.Equal(t, 100.0, *got.PreviousCarpoolBonusUSD)
			}

			repo.group = got
			repeated, err := svc.UpdateGroup(context.Background(), 7, &UpdateGroupInput{CarpoolBonusUSD: bonus})
			require.NoError(t, err)
			require.Equal(t, *got.WeeklyLimitUSD, *repeated.WeeklyLimitUSD, "saving again must not add the bonus twice")
			require.False(t, repeated.AdjustCarpoolBonus)
		})
	}
}

func TestCarpoolGroupBonusUpdateBootstrapAndDisable(t *testing.T) {
	for _, limit := range []*float64{nil, new(0.0)} {
		repo := &groupPlatformRepoStub{group: &Group{ID: 7, Platform: PlatformOpenAI,
			SubscriptionType: SubscriptionTypeSubscription, CarpoolMembers: 4, CarpoolBonusUSD: 100, WeeklyLimitUSD: limit}}
		got, err := (&adminServiceImpl{groupRepo: repo}).UpdateGroup(context.Background(), 7, &UpdateGroupInput{CarpoolBonusUSD: new(150.0)})
		require.NoError(t, err)
		require.Equal(t, 550.0, *got.WeeklyLimitUSD)
		require.False(t, got.AdjustCarpoolBonus)
	}
	repo := &groupPlatformRepoStub{group: &Group{ID: 7, Platform: PlatformOpenAI,
		SubscriptionType: SubscriptionTypeSubscription, CarpoolMembers: 4, CarpoolBonusUSD: 100, WeeklyLimitUSD: new(445.0)}}
	got, err := (&adminServiceImpl{groupRepo: repo}).UpdateGroup(context.Background(), 7, &UpdateGroupInput{CarpoolMembers: new(0), CarpoolBonusUSD: new(150.0)})
	require.NoError(t, err)
	require.Equal(t, 150.0, got.CarpoolBonusUSD)
	require.Equal(t, 445.0, *got.WeeklyLimitUSD)
	require.False(t, got.AdjustCarpoolBonus)
}

func TestCarpoolGroupBonusRejectsInvalidValues(t *testing.T) {
	for _, bonus := range []float64{-1, 0.001, 150.251, 1e10 + 0.01, math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprint(bonus), func(t *testing.T) {
			_, err := (&adminServiceImpl{groupRepo: &groupRepoStubForAdmin{}}).CreateGroup(context.Background(), &CreateGroupInput{
				Name: "carpool", Platform: PlatformOpenAI, RateMultiplier: 1,
				SubscriptionType: SubscriptionTypeSubscription, CarpoolMembers: 4, CarpoolBonusUSD: &bonus,
			})
			require.Equal(t, "INVALID_CARPOOL_BONUS", infraerrors.Reason(err))
			repo := &groupPlatformRepoStub{group: &Group{ID: 7, Platform: PlatformOpenAI,
				SubscriptionType: SubscriptionTypeSubscription, CarpoolMembers: 4, CarpoolBonusUSD: 100, WeeklyLimitUSD: new(445.0)}}
			_, err = (&adminServiceImpl{groupRepo: repo}).UpdateGroup(context.Background(), 7, &UpdateGroupInput{CarpoolBonusUSD: &bonus})
			require.Equal(t, "INVALID_CARPOOL_BONUS", infraerrors.Reason(err))
			require.Nil(t, repo.updated)
		})
	}
}

func TestCarpoolGroupDisablePreservesOnlyOmittedCap(t *testing.T) {
	for _, explicit := range []*float64{nil, new(600.0)} {
		repo := &groupPlatformRepoStub{group: &Group{ID: 7, Platform: PlatformOpenAI, Status: StatusActive,
			SubscriptionType: SubscriptionTypeSubscription, CarpoolMembers: 4, CarpoolBonusUSD: 100, WeeklyLimitUSD: new(445.0)}}
		got, err := (&adminServiceImpl{groupRepo: repo}).UpdateGroup(context.Background(), 7, &UpdateGroupInput{CarpoolMembers: new(0), WeeklyLimitUSD: explicit})
		require.NoError(t, err)
		require.Equal(t, explicit == nil, got.PreserveCarpoolWeeklyLimit)
		require.Equal(t, &CarpoolConfig{Members: 4, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive}, got.PreviousCarpoolConfig)
		if explicit != nil {
			require.Equal(t, *explicit, *got.WeeklyLimitUSD)
		}
	}
}

func TestCarpoolGroupBonusCannotTurnLegacyCapUnlimited(t *testing.T) {
	repo := &groupPlatformRepoStub{group: &Group{ID: 7, Platform: PlatformOpenAI,
		SubscriptionType: SubscriptionTypeSubscription, CarpoolMembers: 4, CarpoolBonusUSD: 100, WeeklyLimitUSD: new(50.0)}}
	_, err := (&adminServiceImpl{groupRepo: repo}).UpdateGroup(context.Background(), 7, &UpdateGroupInput{CarpoolBonusUSD: new(0.0)})
	require.Equal(t, "CARPOOL_QUOTA_CHANGED", infraerrors.Reason(err))
	require.Nil(t, repo.updated)
}
