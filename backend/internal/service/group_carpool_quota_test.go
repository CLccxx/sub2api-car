package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/require"
)

func TestCarpoolWeeklyLimit(t *testing.T) {
	for _, test := range []struct {
		estimate float64
		members  int
		expected float64
		ok       bool
	}{
		{1374.65, 4, 350, true}, {1374.65, 3, 466.66, true}, {1600, 5, 320, true},
		{1399.99, 4, 350, true}, {1400, 4, 350, true}, {150, 4, 200, true},
		{520, 4, 200, true}, {560, 4, 200, true}, {550, 4, 200, true},
		{600, 4, 200, true}, {550.5, 4, 200, true}, {550, 3, 200, true},
		{520, 2, 275, true}, {49.99, 4, 200, true}, {50, 4, 200, true},
		{50.01, 4, 200, true}, {99.99, 4, 200, true}, {100, 4, 200, true},
		{9.99, 4, 200, true}, {10, 4, 200, true}, {19.99, 4, 200, true}, {20, 4, 200, true},
		{0, 4, 0, false}, {-100, 4, 0, false}, {math.NaN(), 4, 0, false},
		{math.Inf(1), 4, 0, false}, {math.Inf(-1), 4, 0, false}, {1e10, 5, 2e9, true}, {1e10 + 1, 4, 0, false},
		{1000, -1, 0, false}, {1000, 0, 0, false}, {1000, 1, 0, false}, {1000, 6, 0, false},
	} {
		got, ok := carpoolWeeklyLimit(test.estimate, test.members)
		require.Equal(t, test.ok, ok)
		require.Equal(t, test.expected, got)
	}
}

func TestCarpoolWeeklyLimitPerPersonFloor(t *testing.T) {
	for _, test := range []struct {
		members    int
		aboveFloor float64
	}{{2, 225}, {3, 216.66}, {4, 212.5}, {5, 210}} {
		t.Run(fmt.Sprintf("%d members", test.members), func(t *testing.T) {
			totalAtFloor := 200.0 * float64(test.members)
			for _, estimate := range []float64{5.33, math.SmallestNonzeroFloat64, totalAtFloor - 0.01, totalAtFloor} {
				t.Run(fmt.Sprintf("estimate %g", estimate), func(t *testing.T) {
					limit, ok := carpoolWeeklyLimit(estimate, test.members)
					require.True(t, ok)
					require.Equal(t, 200.0, limit)
				})
			}
			limit, ok := carpoolWeeklyLimit(totalAtFloor+0.01, test.members)
			require.True(t, ok)
			require.Equal(t, test.aboveFloor, limit)
		})
	}
}

func TestCarpoolScheduleShanghai(t *testing.T) {
	schedule, err := cron.ParseStandard(carpoolQuotaSchedule)
	require.NoError(t, err)
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	now := time.Date(2026, 10, 8, 23, 59, 0, 0, loc)
	for hour := 0; hour < 24; hour++ {
		for _, minute := range []int{0, 30} {
			next := schedule.Next(now.UTC()).In(loc)
			require.Equal(t, time.Date(2026, 10, 9, hour, minute, 0, 0, loc), next)
			now = next
		}
	}
	require.Equal(t, time.Date(2026, 10, 10, 0, 0, 0, 0, loc), schedule.Next(now.UTC()).In(loc))
}

type carpoolRepoStub struct {
	targets     []CarpoolQuotaTarget
	err         error
	applied     bool
	writes      int
	limit       float64
	accountID   int64
	listedSlot  time.Time
	appliedSlot time.Time
}

func (r *carpoolRepoStub) ListPending(_ context.Context, slot time.Time) ([]CarpoolQuotaTarget, error) {
	r.listedSlot = slot
	return r.targets, r.err
}
func (r *carpoolRepoStub) Apply(_ context.Context, _ CarpoolQuotaTarget, account *Account, limit float64, slot time.Time) (bool, error) {
	r.writes++
	r.limit = limit
	r.accountID = account.ID
	r.appliedSlot = slot
	return r.applied, r.err
}

func TestCarpoolQuotaRunHalfHourSlots(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	for _, test := range []struct {
		hour       int
		minute     int
		slotMinute int
	}{{0, 0, 0}, {0, 29, 0}, {0, 30, 30}, {0, 59, 30}, {1, 0, 0}, {1, 29, 0}, {1, 30, 30}, {1, 59, 30}, {23, 29, 0}, {23, 30, 30}, {23, 59, 30}} {
		observed := time.Date(2026, 10, 9, test.hour, test.minute, 0, 0, loc)
		t.Run(observed.Format("15:04"), func(t *testing.T) {
			_, _, accounts, quota, _, now := sharedUsageFixture(t)
			*now = observed.UTC()
			quota.payload.FetchedAt = now.Unix()
			quota.payload.RateLimit.SecondaryWindow.ResetAt = now.Add(48 * time.Hour).Unix()
			repo := &carpoolRepoStub{targets: []CarpoolQuotaTarget{{GroupID: 10, Members: 4, Revision: *now}}, applied: true}
			svc := NewCarpoolQuotaService(repo, accounts, quota, &carpoolStats{cost: 120}, nil)
			svc.now = func() time.Time { return *now }

			svc.run(context.Background())

			wantSlot := time.Date(2026, 10, 9, test.hour, test.slotMinute, 0, 0, loc)
			require.Equal(t, wantSlot, repo.listedSlot)
			require.Equal(t, 1, repo.writes)
			require.Equal(t, wantSlot, repo.appliedSlot)
		})
	}
}

type carpoolStats struct {
	cost        float64
	err         error
	accountID   int64
	windowStart time.Time
}

func (s *carpoolStats) GetAccountWindowStats(_ context.Context, accountID int64, start time.Time) (*usagestats.AccountStats, error) {
	s.accountID = accountID
	s.windowStart = start
	return &usagestats.AccountStats{Cost: s.cost}, s.err
}

type carpoolCacheSpy struct {
	APIKeyAuthCacheInvalidator
	ids []int64
}

func (s *carpoolCacheSpy) InvalidateAuthCacheByGroupID(_ context.Context, id int64) {
	s.ids = append(s.ids, id)
}

func TestCarpoolQuotaRefresh(t *testing.T) {
	for _, scenario := range []string{"success", "conflict", "upstream failure", "stats failure", "missing", "zero", "small", "stale", "no timestamp", "expired", "multiple", "none", "inactive", "unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			_, _, accounts, quota, _, now := sharedUsageFixture(t)
			stats := &carpoolStats{cost: 1374.65 / 4}
			repo := &carpoolRepoStub{targets: []CarpoolQuotaTarget{{GroupID: 10, Members: 4, Revision: *now}}, applied: true}
			cache := &carpoolCacheSpy{}
			svc := NewCarpoolQuotaService(repo, accounts, quota, stats, cache)
			svc.now = func() time.Time { return *now }
			switch scenario {
			case "conflict":
				repo.applied = false
			case "upstream failure":
				quota.err = errors.New("upstream unavailable")
			case "stats failure":
				stats.err = errors.New("stats unavailable")
			case "missing":
				quota.payload.RateLimit = nil
			case "zero":
				quota.payload.RateLimit.SecondaryWindow.UsedPercent = 0
			case "small":
				stats.cost = 1
			case "stale":
				quota.payload.FetchedAt = now.Add(-time.Hour).Unix()
			case "no timestamp":
				quota.payload.FetchedAt = 0
			case "expired":
				quota.payload.RateLimit.SecondaryWindow.ResetAt = now.Add(-time.Second).Unix()
			case "multiple":
				accounts.accounts = append(accounts.accounts, Account{ID: 11})
			case "none":
				accounts.accounts = nil
			case "inactive":
				accounts.accounts[0].Status = "inactive"
			case "unsupported":
				accounts.accounts[0].Type = AccountTypeAPIKey
			}
			svc.run(context.Background())
			switch scenario {
			case "success", "conflict":
				require.Equal(t, 1, repo.writes)
				require.Equal(t, 350.0, repo.limit)
			case "small":
				require.Equal(t, 1, repo.writes)
				require.Equal(t, 200.0, repo.limit)
			default:
				require.Zero(t, repo.writes)
			}
			if scenario == "success" || scenario == "small" {
				require.Equal(t, []int64{10}, cache.ids)
			} else {
				require.Empty(t, cache.ids)
			}
		})
	}
}

func TestCarpoolQuotaRefreshRoundedLimitPreservesAvailableQuota(t *testing.T) {
	_, _, accounts, quota, _, now := sharedUsageFixture(t)
	quota.payload.RateLimit.SecondaryWindow.UsedPercent = 99
	stats := &carpoolStats{cost: 1000}
	repo := &carpoolRepoStub{targets: []CarpoolQuotaTarget{{GroupID: 10, Members: 4, Revision: *now}}, applied: true}
	svc := NewCarpoolQuotaService(repo, accounts, quota, stats, nil)
	svc.now = func() time.Time { return *now }

	svc.run(context.Background())

	require.Equal(t, 1, repo.writes)
	require.Equal(t, 262.5, repo.limit)
	sub := &UserSubscription{WeeklyUsageUSD: 260}
	require.True(t, sub.CheckWeeklyLimit(&Group{WeeklyLimitUSD: &repo.limit}, 0))
}

func TestCarpoolQuotaRefreshReconnectedAccountUsesPerPersonFloor(t *testing.T) {
	_, _, accounts, quota, _, now := sharedUsageFixture(t)
	accounts.accounts[0].ID = 42
	window := quota.payload.RateLimit.SecondaryWindow
	window.UsedPercent = 8
	stats := &carpoolStats{cost: 5.33 * 0.08}
	repo := &carpoolRepoStub{targets: []CarpoolQuotaTarget{{GroupID: 10, Members: 3, Revision: *now}}, applied: true}
	cache := &carpoolCacheSpy{}
	svc := NewCarpoolQuotaService(repo, accounts, quota, stats, cache)
	svc.now = func() time.Time { return *now }

	svc.run(context.Background())

	require.Equal(t, 1, repo.writes)
	require.Equal(t, 200.0, repo.limit)
	require.Equal(t, int64(42), repo.accountID)
	require.Equal(t, int64(42), stats.accountID)
	require.Equal(t, time.Unix(window.ResetAt, 0).UTC().Add(-7*24*time.Hour), stats.windowStart)
	require.Equal(t, []int64{10}, cache.ids)
	view := querySharedAccountUsage(context.Background(), accounts.accounts[0].ID, quota, stats, svc.now).view
	require.NotNil(t, view.SevenDay)
	require.NotNil(t, view.SevenDay.EstimatedTotalCost)
	require.InDelta(t, 5.33, *view.SevenDay.EstimatedTotalCost, 1e-9)
	sub := &UserSubscription{WeeklyUsageUSD: 175}
	group := &Group{WeeklyLimitUSD: &repo.limit}
	require.True(t, sub.CheckWeeklyLimit(group, 25))
	require.False(t, sub.CheckWeeklyLimit(group, 25.01))
	require.Equal(t, 175.0, sub.WeeklyUsageUSD)
}
