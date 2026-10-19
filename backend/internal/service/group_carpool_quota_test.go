package service

import (
	"context"
	"errors"
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
		{1374.65, 4, 342.5, true}, {1374.65, 3, 456.66, true}, {1600, 5, 320, true},
		{1399.99, 4, 347.5, true}, {1400, 4, 350, true}, {150, 4, 37.5, true},
		{99.99, 4, 22.5, true}, {100, 4, 25, true}, {9.99, 4, 0, false},
		{10, 4, 2.5, true}, {19.99, 4, 2.5, true}, {20, 4, 5, true},
		{0, 4, 0, false}, {-100, 4, 0, false}, {math.NaN(), 4, 0, false},
		{math.Inf(1), 4, 0, false}, {1000, 0, 0, false}, {1000, 6, 0, false},
	} {
		got, ok := carpoolWeeklyLimit(test.estimate, test.members)
		require.Equal(t, test.ok, ok)
		require.Equal(t, test.expected, got)
	}
}

func TestCarpoolScheduleShanghai(t *testing.T) {
	schedule, err := cron.ParseStandard(carpoolQuotaSchedule)
	require.NoError(t, err)
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	now := time.Date(2026, 10, 8, 23, 59, 0, 0, loc)
	for _, hour := range []int{0, 6, 12, 18} {
		next := schedule.Next(now.UTC()).In(loc)
		require.Equal(t, time.Date(2026, 10, 9, hour, 0, 0, 0, loc), next)
		now = next
	}
}

type carpoolRepoStub struct {
	targets []CarpoolQuotaTarget
	err     error
	applied bool
	writes  int
	limit   float64
}

func (r *carpoolRepoStub) ListPending(context.Context, time.Time) ([]CarpoolQuotaTarget, error) {
	return r.targets, r.err
}
func (r *carpoolRepoStub) Apply(_ context.Context, _ CarpoolQuotaTarget, _ *Account, limit float64, _ time.Time) (bool, error) {
	r.writes++
	r.limit = limit
	return r.applied, r.err
}

type carpoolStats struct {
	cost float64
	err  error
}

func (s *carpoolStats) GetAccountWindowStats(context.Context, int64, time.Time) (*usagestats.AccountStats, error) {
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
			if scenario == "success" || scenario == "conflict" {
				require.Equal(t, 1, repo.writes)
				require.Equal(t, 342.5, repo.limit)
			} else {
				require.Zero(t, repo.writes)
			}
			if scenario == "success" {
				require.Equal(t, []int64{10}, cache.ids)
			} else {
				require.Empty(t, cache.ids)
			}
		})
	}
}

func TestCarpoolQuotaRefreshTenDollarRoundingPreservesAvailableQuota(t *testing.T) {
	_, _, accounts, quota, _, now := sharedUsageFixture(t)
	quota.payload.RateLimit.SecondaryWindow.UsedPercent = 80
	stats := &carpoolStats{cost: 120}
	repo := &carpoolRepoStub{targets: []CarpoolQuotaTarget{{GroupID: 10, Members: 4, Revision: *now}}, applied: true}
	svc := NewCarpoolQuotaService(repo, accounts, quota, stats, nil)
	svc.now = func() time.Time { return *now }

	svc.run(context.Background())

	require.Equal(t, 1, repo.writes)
	require.Equal(t, 37.5, repo.limit)
	sub := &UserSubscription{WeeklyUsageUSD: 30}
	require.True(t, sub.CheckWeeklyLimit(&Group{WeeklyLimitUSD: &repo.limit}, 0))
}
