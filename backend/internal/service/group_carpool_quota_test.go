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
		{1374, 4, 445, true}, {1374, 5, 376, true},
		{1374.65, 4, 445, true}, {1374.65, 3, 560, true}, {1600, 5, 420, true},
		{1399.99, 4, 450, true}, {1400, 4, 450, true}, {150, 4, 137.5, true},
		{520, 4, 230, true}, {560, 4, 240, true}, {550, 4, 237.5, true},
		{600, 4, 250, true}, {550.5, 4, 240, true}, {550, 3, 283.33, true},
		{520, 2, 360, true}, {49.99, 4, 112.5, true}, {50, 4, 112.5, true},
		{50.01, 4, 115, true}, {99.99, 4, 125, true}, {100, 4, 125, true},
		{9.99, 4, 102.5, true}, {10, 4, 102.5, true}, {19.99, 4, 105, true}, {20, 4, 105, true},
		{math.Nextafter(10, 0), 4, 102.5, true}, {math.Nextafter(10, math.Inf(1)), 4, 105, true},
		{0, 4, 0, false}, {-100, 4, 0, false}, {math.NaN(), 4, 0, false},
		{math.Inf(1), 4, 0, false}, {math.Inf(-1), 4, 0, false}, {1e10, 5, 2e9 + 100, true}, {1e10 + 1, 4, 0, false},
		{1000, -1, 0, false}, {1000, 0, 0, false}, {1000, 1, 0, false}, {1000, 6, 0, false},
	} {
		t.Run(fmt.Sprintf("estimate %g members %d", test.estimate, test.members), func(t *testing.T) {
			got, ok := carpoolWeeklyLimit(test.estimate, test.members)
			require.Equal(t, test.ok, ok)
			require.Equal(t, test.expected, got)
		})
	}
}

func TestCarpoolWeeklyLimitAddsBonusWithoutMinimumFloor(t *testing.T) {
	for _, test := range []struct {
		members  int
		expected float64
	}{{2, 105}, {3, 103.33}, {4, 102.5}, {5, 102}} {
		t.Run(fmt.Sprintf("%d members", test.members), func(t *testing.T) {
			for _, estimate := range []float64{5.33, math.SmallestNonzeroFloat64, 10} {
				t.Run(fmt.Sprintf("estimate %g", estimate), func(t *testing.T) {
					limit, ok := carpoolWeeklyLimit(estimate, test.members)
					require.True(t, ok)
					require.Equal(t, test.expected, limit)
				})
			}
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
		for _, minute := range []int{0, 10, 20, 30, 40, 50} {
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
	identities  []string
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
	r.identities = append(r.identities, account.GetCredential("chatgpt_account_id"))
	r.appliedSlot = slot
	return r.applied, r.err
}

func TestCarpoolQuotaRunSamplesEveryTenMinutesAndSyncsHalfHourly(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	for _, test := range []struct {
		minute     int
		slotMinute int
		groupSync  bool
	}{{0, 0, true}, {9, 0, true}, {10, 10, false}, {29, 20, false}, {30, 30, true}, {39, 30, true}, {40, 40, false}, {59, 50, false}} {
		observed := time.Date(2026, 10, 9, 23, test.minute, 0, 0, loc)
		t.Run(observed.Format("15:04"), func(t *testing.T) {
			_, _, accounts, quota, _, now := sharedUsageFixture(t)
			*now = observed.UTC()
			quota.payload.FetchedAt = now.Unix()
			quota.payload.RateLimit.SecondaryWindow.ResetAt = now.Add(48 * time.Hour).Unix()
			repo := &carpoolRepoStub{targets: []CarpoolQuotaTarget{{GroupID: 10, Members: 4, Revision: *now}}, applied: true}
			wantSlot := time.Date(2026, 10, 9, 23, test.slotMinute, 0, 0, loc)
			var sampledIDs []int64
			reader := carpoolQuotaReaderFunc(func(ctx context.Context, id int64) (*OpenAIQuotaUsage, error) {
				require.Equal(t, wantSlot, subscriptionCostSlot(ctx))
				sampledIDs = append(sampledIDs, id)
				return quota.QueryUsageReadOnly(ctx, id)
			})
			svc := NewCarpoolQuotaService(repo, accounts, reader, nil, nil)
			svc.cost = NewSubscriptionCostService(carpoolCandidateStub{ids: []int64{9, 12}})
			svc.now = func() time.Time { return *now }

			svc.run(context.Background())

			require.Equal(t, []int64{9, 12}, sampledIDs, "each account should be sampled once, including the grouped account")
			if test.groupSync {
				require.Equal(t, wantSlot, repo.listedSlot)
				require.Equal(t, 1, repo.writes)
				require.Equal(t, wantSlot, repo.appliedSlot)
			} else {
				require.True(t, repo.listedSlot.IsZero())
				require.Zero(t, repo.writes)
			}
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
			estimate := 1374.65
			quota.payload.SubscriptionCost = &SubscriptionCostEstimate{EstimatedTotalCost: &estimate, EstimateSource: "dynamic"}
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
				quota.payload.SubscriptionCost = nil
			case "missing":
				quota.payload.RateLimit = nil
			case "zero":
				quota.payload.RateLimit.SecondaryWindow.UsedPercent = 0
				quota.payload.SubscriptionCost = nil
			case "small":
				stats.cost = 1
				estimate = 4
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
				require.Equal(t, 445.0, repo.limit)
			case "small":
				require.Equal(t, 1, repo.writes)
				require.Equal(t, 102.5, repo.limit)
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
	estimate := 1000.0 / 0.99
	quota.payload.SubscriptionCost = &SubscriptionCostEstimate{EstimatedTotalCost: &estimate, EstimateSource: "dynamic"}
	repo := &carpoolRepoStub{targets: []CarpoolQuotaTarget{{GroupID: 10, Members: 4, Revision: *now}}, applied: true}
	svc := NewCarpoolQuotaService(repo, accounts, quota, stats, nil)
	svc.now = func() time.Time { return *now }

	svc.run(context.Background())

	require.Equal(t, 1, repo.writes)
	require.Equal(t, 355.0, repo.limit)
	sub := &UserSubscription{WeeklyUsageUSD: 354}
	require.True(t, sub.CheckWeeklyLimit(&Group{WeeklyLimitUSD: &repo.limit}, 0))
	require.Equal(t, 354.0, sub.WeeklyUsageUSD)
}

func TestCarpoolQuotaRefreshReconnectedAccountUsesBonusWithoutMinimumFloor(t *testing.T) {
	_, _, accounts, quota, _, now := sharedUsageFixture(t)
	accounts.accounts[0].ID = 42
	window := quota.payload.RateLimit.SecondaryWindow
	window.UsedPercent = 8
	stats := &carpoolStats{cost: 5.33 * 0.08}
	estimate := 5.33
	quota.payload.SubscriptionCost = &SubscriptionCostEstimate{EstimatedTotalCost: &estimate, EstimateSource: "dynamic"}
	repo := &carpoolRepoStub{targets: []CarpoolQuotaTarget{{GroupID: 10, Members: 3, Revision: *now}}, applied: true}
	cache := &carpoolCacheSpy{}
	svc := NewCarpoolQuotaService(repo, accounts, quota, stats, cache)
	svc.now = func() time.Time { return *now }

	svc.run(context.Background())

	require.Equal(t, 1, repo.writes)
	require.Equal(t, 103.33, repo.limit)
	require.Equal(t, int64(42), repo.accountID)
	require.Zero(t, stats.accountID, "quota must use the shared subscription estimate")
	require.True(t, stats.windowStart.IsZero())
	require.Equal(t, []int64{10}, cache.ids)
	view := querySharedAccountUsage(context.Background(), accounts.accounts[0].ID, quota, stats, svc.now).view
	require.NotNil(t, view.SevenDay)
	require.NotNil(t, view.SevenDay.EstimatedTotalCost)
	require.InDelta(t, 5.33, *view.SevenDay.EstimatedTotalCost, 1e-9)
	sub := &UserSubscription{WeeklyUsageUSD: 100}
	group := &Group{WeeklyLimitUSD: &repo.limit}
	require.True(t, sub.CheckWeeklyLimit(group, 3.33))
	require.False(t, sub.CheckWeeklyLimit(group, 3.34))
	require.Equal(t, 100.0, sub.WeeklyUsageUSD)
	sub.WeeklyUsageUSD = 175
	require.False(t, sub.CheckWeeklyLimit(group, 0))
	require.Equal(t, 175.0, sub.WeeklyUsageUSD, "a lower cap must not clear existing usage")
}

func TestCarpoolQuotaSamplesSharedAccountOncePerSlot(t *testing.T) {
	_, _, accounts, quota, stats, now := sharedUsageFixture(t)
	first := accounts.accounts[0]
	first.Credentials = map[string]any{"chatgpt_account_id": "original"}
	rebound := first
	rebound.Credentials = map[string]any{"chatgpt_account_id": "rebound"}
	accounts.groups = map[int64][]Account{10: {first}, 11: {rebound}}
	repo := &carpoolRepoStub{targets: []CarpoolQuotaTarget{
		{GroupID: 10, Members: 4}, {GroupID: 11, Members: 5},
	}}
	svc := NewCarpoolQuotaService(repo, accounts, quota, stats, nil)
	svc.now = func() time.Time { return *now }
	svc.run(context.Background())
	require.Equal(t, 2, repo.writes)
	require.Equal(t, int64(1), quota.calls.Load())
	require.Equal(t, []string{"original", "original"}, repo.identities, "apply must validate the sampled identity, even after a rebind")
}

func TestSubscriptionCostFirstFullSampleLocksActualCost(t *testing.T) {
	state := &SubscriptionCostState{}
	first := UpdateSubscriptionCostEstimate(state, 103, 1530)
	require.Equal(t, 1530.0, *first.EstimatedTotalCost)
	require.Zero(t, *first.CreditsUsedCost)
	later := UpdateSubscriptionCostEstimate(state, 106, 1600)
	require.Equal(t, 1530.0, *later.EstimatedTotalCost)
	require.Equal(t, 70.0, *later.CreditsUsedCost)
}

type carpoolCandidateStub struct {
	SubscriptionCostRepository
	ids []int64
}

func (s carpoolCandidateStub) Candidates(_ context.Context, after int64, _ int) ([]int64, error) {
	candidates := s.ids
	if candidates == nil {
		candidates = []int64{1, 2, 3}
	}
	var ids []int64
	for _, id := range candidates {
		if id > after {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

type carpoolQuotaReaderFunc func(context.Context, int64) (*OpenAIQuotaUsage, error)

func (f carpoolQuotaReaderFunc) QueryUsageReadOnly(ctx context.Context, id int64) (*OpenAIQuotaUsage, error) {
	return f(ctx, id)
}

func TestCarpoolSampleRemainingResumesAfterDeadlineAndWraps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls []int64
	svc := &CarpoolQuotaService{cost: NewSubscriptionCostService(carpoolCandidateStub{}),
		quota: carpoolQuotaReaderFunc(func(_ context.Context, id int64) (*OpenAIQuotaUsage, error) {
			calls = append(calls, id)
			if len(calls) == 1 {
				cancel()
			}
			return nil, nil
		}),
	}
	svc.sampleRemainingAccounts(ctx, nil)
	require.Equal(t, []int64{1}, calls)
	calls = nil
	svc.sampleRemainingAccounts(context.Background(), map[int64]carpoolQuotaSample{3: {}})
	require.Equal(t, []int64{2, 1}, calls)
}
