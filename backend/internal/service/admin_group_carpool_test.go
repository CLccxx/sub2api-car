//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCarpoolGroupEnable(t *testing.T) {
	daily, monthly := 20.0, 800.0
	repo := &groupPlatformRepoStub{group: &Group{ID: 7, Name: "old", Platform: PlatformOpenAI,
		SubscriptionType: SubscriptionTypeSubscription, DailyLimitUSD: &daily, MonthlyLimitUSD: &monthly}}
	svc := &adminServiceImpl{groupRepo: repo}
	var input UpdateGroupInput
	require.NoError(t, json.Unmarshal([]byte(`{"CarpoolMembers":4}`), &input))
	got, err := svc.UpdateGroup(context.Background(), 7, &input)
	require.NoError(t, err)
	require.Equal(t, "4 人车分组", got.Name)
	require.Equal(t, "4 人均分额度", got.Description)
	require.NotNil(t, got.WeeklyLimitUSD)
	require.Equal(t, 400.0, *got.WeeklyLimitUSD)
	require.Nil(t, got.DailyLimitUSD)
	require.Nil(t, got.MonthlyLimitUSD)
}

func TestCarpoolGroupInvalidConfiguration(t *testing.T) {
	for _, test := range []struct{ name, platform, kind, input string }{
		{"negative", PlatformOpenAI, SubscriptionTypeSubscription, `{"CarpoolMembers":-1}`},
		{"one", PlatformOpenAI, SubscriptionTypeSubscription, `{"CarpoolMembers":1}`},
		{"too many", PlatformOpenAI, SubscriptionTypeSubscription, `{"CarpoolMembers":6}`},
		{"platform", PlatformAnthropic, SubscriptionTypeSubscription, `{"CarpoolMembers":4}`},
		{"standard", PlatformOpenAI, SubscriptionTypeStandard, `{"CarpoolMembers":4}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &groupPlatformRepoStub{group: &Group{ID: 7, Name: "old", Platform: test.platform, SubscriptionType: test.kind}}
			var input UpdateGroupInput
			require.NoError(t, json.Unmarshal([]byte(test.input), &input))
			_, err := (&adminServiceImpl{groupRepo: repo}).UpdateGroup(context.Background(), 7, &input)
			require.Error(t, err)
			require.Nil(t, repo.updated)
		})
	}
}

func TestCarpoolGroupPreservesExistingCapWhenSizeChanges(t *testing.T) {
	for _, initialSize := range []int{0, 3, 4} {
		cap := 325.0
		g := &Group{ID: 7, Name: "old", Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, WeeklyLimitUSD: &cap}
		g.CarpoolMembers = initialSize
		repo := &groupPlatformRepoStub{group: g}
		var input UpdateGroupInput
		require.NoError(t, json.Unmarshal([]byte(`{"CarpoolMembers":4,"WeeklyLimitUSD":400}`), &input))
		got, err := (&adminServiceImpl{groupRepo: repo}).UpdateGroup(context.Background(), 7, &input)
		require.NoError(t, err)
		require.Equal(t, 325.0, *got.WeeklyLimitUSD, "size changes must not overwrite a nonzero cap")
		require.True(t, got.PreserveCarpoolWeeklyLimit)
	}
}

func TestCarpoolGroupZeroCapAndDisable(t *testing.T) {
	zero := 0.0
	repo := &groupPlatformRepoStub{group: &Group{ID: 7, Name: "old", Platform: PlatformOpenAI,
		SubscriptionType: SubscriptionTypeSubscription, CarpoolMembers: 4, WeeklyLimitUSD: &zero}}
	svc := &adminServiceImpl{groupRepo: repo}
	got, err := svc.UpdateGroup(context.Background(), 7, &UpdateGroupInput{})
	require.NoError(t, err)
	require.Equal(t, 400.0, *got.WeeklyLimitUSD)
	cap := 325.0
	repo.group.WeeklyLimitUSD = &cap
	off := 0
	got, err = svc.UpdateGroup(context.Background(), 7, &UpdateGroupInput{CarpoolMembers: &off})
	require.NoError(t, err)
	require.Zero(t, got.CarpoolMembers)
	require.False(t, got.PreserveCarpoolWeeklyLimit)
	require.Equal(t, 325.0, *got.WeeklyLimitUSD)
}

func TestCarpoolGroupCreate(t *testing.T) {
	for _, supplied := range []float64{0, 325} {
		repo := &groupRepoStubForAdmin{}
		svc := &adminServiceImpl{groupRepo: repo}
		got, err := svc.CreateGroup(context.Background(), &CreateGroupInput{Name: "old", Platform: PlatformOpenAI,
			RateMultiplier: 1, SubscriptionType: SubscriptionTypeSubscription, CarpoolMembers: 3, WeeklyLimitUSD: &supplied})
		require.NoError(t, err)
		require.Equal(t, "3 人车分组", got.Name)
		if supplied == 0 {
			require.Equal(t, 533.33, *got.WeeklyLimitUSD)
		} else {
			require.Equal(t, supplied, *got.WeeklyLimitUSD)
		}
	}
}
