package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type sharedUsageSubRepo struct {
	UserSubscriptionRepository
	sub *UserSubscription
}

func (r *sharedUsageSubRepo) GetByID(_ context.Context, id int64) (*UserSubscription, error) {
	if r.sub == nil || r.sub.ID != id {
		return nil, ErrSubscriptionNotFound
	}
	copy := *r.sub
	return &copy, nil
}

type sharedUsageAccountRepo struct {
	AccountRepository
	accounts []Account
	t        *testing.T
}

type sharedUsageTwoUsersRepo struct{ *sharedUsageSubRepo }

func (r *sharedUsageTwoUsersRepo) GetByID(ctx context.Context, id int64) (*UserSubscription, error) {
	if id != 2 {
		return r.sharedUsageSubRepo.GetByID(ctx, id)
	}
	copy := *r.sub
	copy.ID, copy.UserID = 2, 8
	return &copy, nil
}

func TestSharedSubscriptionUsageMembersShareOneSnapshot(t *testing.T) {
	svc, subs, _, quota, _, _ := sharedUsageFixture(t)
	svc.subscriptions = &sharedUsageTwoUsersRepo{subs}
	first, err := svc.Get(context.Background(), 7, 1)
	require.NoError(t, err)
	second, err := svc.Get(context.Background(), 8, 2)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, int64(1), quota.calls.Load())
	_, err = svc.Get(context.Background(), 7, 2)
	require.ErrorIs(t, err, ErrSubscriptionNotFound)
}

func (r *sharedUsageAccountRepo) ListWithFilters(_ context.Context, p pagination.PaginationParams, platform, kind, status, search string, groupID int64, privacy string) ([]Account, *pagination.PaginationResult, error) {
	require.Equal(r.t, int64(10), groupID)
	require.Equal(r.t, 2, p.PageSize)
	require.Empty(r.t, platform+kind+status+search+privacy, "do not hide disabled or unsupported extra bindings")
	return r.accounts, &pagination.PaginationResult{Total: int64(len(r.accounts))}, nil
}

type sharedUsageQuotaStub struct {
	payload      *OpenAIQuotaUsage
	err          error
	calls        atomic.Int64
	beforeReturn func()
}

func (q *sharedUsageQuotaStub) QueryUsageReadOnly(context.Context, int64) (*OpenAIQuotaUsage, error) {
	q.calls.Add(1)
	if q.beforeReturn != nil {
		q.beforeReturn()
	}
	return q.payload, q.err
}

type sharedUsageStatsStub struct {
	start time.Time
	err   error
}

func (r *sharedUsageStatsStub) GetAccountWindowStats(_ context.Context, _ int64, start time.Time) (*usagestats.AccountStats, error) {
	r.start = start
	return &usagestats.AccountStats{Cost: 12.5, UserCost: 999, Requests: 300, Tokens: 1000}, r.err
}

func sharedUsageFixture(t *testing.T) (*SharedSubscriptionUsageService, *sharedUsageSubRepo, *sharedUsageAccountRepo, *sharedUsageQuotaStub, *sharedUsageStatsStub, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	subs := &sharedUsageSubRepo{sub: &UserSubscription{ID: 1, UserID: 7, GroupID: 10, Group: &Group{ID: 10, Status: StatusActive}, Status: SubscriptionStatusActive, ExpiresAt: now.Add(24 * time.Hour)}}
	accounts := &sharedUsageAccountRepo{t: t, accounts: []Account{{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive}}}
	balance := "123.4500"
	quota := &sharedUsageQuotaStub{payload: &OpenAIQuotaUsage{
		Email: "secret@example.test", UserID: "upstream-user", AccountID: "upstream-account", FetchedAt: now.Unix(),
		RateLimit:             &OpenAIRateLimit{SecondaryWindow: &OpenAIRateLimitWindow{UsedPercent: 25, LimitWindowSeconds: 604800, ResetAt: now.Add(48 * time.Hour).Unix()}},
		RateLimitResetCredits: &OpenAIRateLimitResetCredits{AvailableCount: 1},
		Credits:               &OpenAICredits{HasCredits: true, Balance: &balance},
	}}
	stats := &sharedUsageStatsStub{}
	svc := NewSharedSubscriptionUsageService(subs, accounts, stats, quota)
	svc.now = func() time.Time { return now }
	return svc, subs, accounts, quota, stats, &now
}

func TestSharedSubscriptionUsageProjection(t *testing.T) {
	svc, _, _, quota, stats, now := sharedUsageFixture(t)
	result, err := svc.Get(context.Background(), 7, 1)
	require.NoError(t, err)
	require.Equal(t, "ready", result.Status)
	require.Equal(t, float64(25), result.SevenDay.Utilization)
	require.Equal(t, float64(50), *result.SevenDay.EstimatedTotalCost)
	require.Equal(t, now.Add(-5*24*time.Hour), stats.start)
	require.Equal(t, 1, *result.ResetCredits)
	require.Equal(t, "123.4500", *result.Credits.Balance)
	require.Equal(t, *now, *result.UpdatedAt)
	require.Equal(t, now.Add(time.Minute), *result.RefreshAvailableAt)
	body, err := json.Marshal(result)
	require.NoError(t, err)
	for _, secret := range []string{"secret@", "upstream-", "account_id", "user_id", "email", "tokens", "requests", "user_cost", "credentials", "five_hour"} {
		require.NotContains(t, string(body), secret)
	}
	require.Equal(t, int64(1), quota.calls.Load())
}

func TestSharedSubscriptionUsageAuthorizationBeforeCache(t *testing.T) {
	for _, scenario := range []string{"other user", "expired", "revoked", "deleted", "deleted group"} {
		t.Run(scenario, func(t *testing.T) {
			svc, subs, _, quota, _, now := sharedUsageFixture(t)
			_, err := svc.Get(context.Background(), 7, 1)
			require.NoError(t, err)
			userID := int64(7)
			switch scenario {
			case "other user":
				userID = 8
			case "expired":
				subs.sub.ExpiresAt = *now
			case "revoked":
				subs.sub.Status = SubscriptionStatusRevoked
			case "deleted":
				subs.sub.DeletedAt = now
			case "deleted group":
				subs.sub.Group = nil
			}
			result, err := svc.Get(context.Background(), userID, 1)
			require.Error(t, err)
			require.Equal(t, 404, infraerrors.Code(err))
			require.Nil(t, result)
			require.Equal(t, int64(1), quota.calls.Load())
		})
	}
}

func TestSharedSubscriptionUsageBindingStates(t *testing.T) {
	for _, scenario := range []string{"not_configured", "multiple_accounts", "unsupported_account"} {
		t.Run(scenario, func(t *testing.T) {
			svc, _, accounts, quota, _, _ := sharedUsageFixture(t)
			switch scenario {
			case "not_configured":
				accounts.accounts = nil
			case "multiple_accounts":
				accounts.accounts = append(accounts.accounts, Account{ID: 12, Status: "disabled"})
			case "unsupported_account":
				accounts.accounts[0].Type = AccountTypeAPIKey
			}
			result, err := svc.Get(context.Background(), 7, 1)
			require.NoError(t, err)
			require.Equal(t, scenario, result.Status)
			require.Nil(t, result.SevenDay)
			require.Zero(t, quota.calls.Load())
		})
	}
}

func TestSharedSubscriptionUsageRechecksAfterSlowRead(t *testing.T) {
	for _, change := range []string{"revoked", "binding changed"} {
		t.Run(change, func(t *testing.T) {
			svc, subs, accounts, quota, _, _ := sharedUsageFixture(t)
			quota.beforeReturn = func() {
				if change == "revoked" {
					subs.sub.Status = SubscriptionStatusRevoked
				} else {
					accounts.accounts[0].ID = 12
				}
			}
			result, err := svc.Get(context.Background(), 7, 1)
			require.Error(t, err)
			require.Nil(t, result)
		})
	}
}

func TestSharedSubscriptionUsageCacheCooldownAndFailure(t *testing.T) {
	svc, _, _, quota, _, now := sharedUsageFixture(t)
	for i := 0; i < 5; i++ {
		result, err := svc.Get(context.Background(), 7, 1)
		require.NoError(t, err)
		require.Equal(t, "ready", result.Status)
	}
	require.Equal(t, int64(1), quota.calls.Load())
	*now = now.Add(time.Minute)
	quota.err = errors.New("secret token upstream error")
	result, err := svc.Get(context.Background(), 7, 1)
	require.NoError(t, err)
	require.Equal(t, "unavailable", result.Status)
	require.Nil(t, result.UpdatedAt)
	require.Nil(t, result.SevenDay)
	require.Nil(t, result.ResetCredits)
	require.Nil(t, result.Credits)
	_, err = svc.Get(context.Background(), 7, 1)
	require.NoError(t, err)
	require.Equal(t, int64(2), quota.calls.Load(), "failed refreshes must also be rate limited")
}

func TestSharedSubscriptionUsageConcurrentReads(t *testing.T) {
	svc, _, _, quota, _, _ := sharedUsageFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := svc.Get(context.Background(), 7, 1)
			if err != nil || result == nil || result.Status != "ready" {
				t.Errorf("result=%v error=%v", result, err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int64(1), quota.calls.Load())
}

func TestSharedSubscriptionUsageMissingAndInvalidMetrics(t *testing.T) {
	for _, scenario := range []string{"empty", "zero", "invalid utilization", "expired window", "stats failure", "primary weekly", "invalid balance"} {
		t.Run(scenario, func(t *testing.T) {
			svc, _, _, quota, stats, now := sharedUsageFixture(t)
			switch scenario {
			case "empty":
				quota.payload = &OpenAIQuotaUsage{FetchedAt: now.Unix()}
			case "zero":
				quota.payload.RateLimit.SecondaryWindow.UsedPercent = 0
			case "invalid utilization":
				quota.payload.RateLimit.SecondaryWindow.UsedPercent = math.NaN()
			case "expired window":
				quota.payload.RateLimit.SecondaryWindow.ResetAt = now.Unix()
			case "stats failure":
				stats.err = errors.New("db secret")
			case "primary weekly":
				quota.payload.RateLimit.PrimaryWindow, quota.payload.RateLimit.SecondaryWindow = quota.payload.RateLimit.SecondaryWindow, nil
			case "invalid balance":
				balance := "not-a-number"
				quota.payload.Credits.Balance = &balance
			}
			result, err := svc.Get(context.Background(), 7, 1)
			require.NoError(t, err)
			switch scenario {
			case "empty":
				require.Equal(t, "no_data", result.Status)
				require.Nil(t, result.SevenDay)
				require.Nil(t, result.ResetCredits)
				require.Nil(t, result.Credits)
			case "invalid utilization", "expired window":
				require.Nil(t, result.SevenDay)
			case "zero", "stats failure":
				require.Nil(t, result.SevenDay.EstimatedTotalCost)
			case "primary weekly":
				require.Equal(t, float64(25), result.SevenDay.Utilization)
			case "invalid balance":
				require.Nil(t, result.Credits.Balance)
			}
		})
	}
}

func TestSharedSubscriptionUsageExpiresCachedObservations(t *testing.T) {
	svc, _, _, quota, _, now := sharedUsageFixture(t)
	quota.payload.RateLimit.SecondaryWindow.ResetAt = now.Add(10 * time.Second).Unix()
	quota.payload.RateLimitResetCredits.Credits = []OpenAIRateLimitResetCreditDetail{{ExpiresAt: now.Add(10 * time.Second).Format(time.RFC3339)}}
	_, err := svc.Get(context.Background(), 7, 1)
	require.NoError(t, err)
	*now = now.Add(11 * time.Second)
	result, err := svc.Get(context.Background(), 7, 1)
	require.NoError(t, err)
	require.Nil(t, result.SevenDay, "past reset timestamp is not proof of zero usage")
	require.Nil(t, result.ResetCredits, "expired cached credit count needs a new observation")
	require.Equal(t, int64(1), quota.calls.Load())
}

func TestSharedSubscriptionUsageMissingFieldsAreNotZero(t *testing.T) {
	for _, body := range []string{
		`{"credits":{},"rate_limit_reset_credits":{},"rate_limit":{"secondary_window":{"limit_window_seconds":604800,"reset_at":4070908800}}}`,
		`{"credits":{"has_credits":null,"unlimited":null},"rate_limit_reset_credits":{"available_count":null},"rate_limit":{"secondary_window":{"used_percent":null,"limit_window_seconds":604800,"reset_at":4070908800}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			svc, _, _, quota, _, _ := sharedUsageFixture(t)
			quota.payload = &OpenAIQuotaUsage{}
			require.NoError(t, json.Unmarshal([]byte(body), quota.payload))
			result, err := svc.Get(context.Background(), 7, 1)
			require.NoError(t, err)
			require.Nil(t, result.SevenDay)
			require.Nil(t, result.ResetCredits)
			require.Nil(t, result.Credits)
			require.Equal(t, "no_data", result.Status)
		})
	}
	t.Run("explicit zero remains valid", func(t *testing.T) {
		svc, _, _, quota, _, _ := sharedUsageFixture(t)
		quota.payload = &OpenAIQuotaUsage{}
		require.NoError(t, json.Unmarshal([]byte(`{"credits":{"has_credits":false,"unlimited":false},"rate_limit_reset_credits":{"available_count":0},"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":4070908800}}}`), quota.payload))
		result, err := svc.Get(context.Background(), 7, 1)
		require.NoError(t, err)
		require.NotNil(t, result.SevenDay)
		require.Zero(t, result.SevenDay.Utilization)
		require.NotNil(t, result.ResetCredits)
		require.Zero(t, *result.ResetCredits)
		require.NotNil(t, result.Credits)
		require.False(t, result.Credits.HasCredits)
	})
}

func TestSharedSubscriptionUsageCapacityPreservesCooldown(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			svc, _, accounts, quota, _, now := sharedUsageFixture(t)
			if failed {
				quota.err = errors.New("upstream unavailable")
			}
			first, err := svc.Get(context.Background(), 7, 1)
			require.NoError(t, err)
			quota.err = nil
			for id := int64(1000); id < 1000+sharedUsageCacheLimit-1; id++ {
				accounts.accounts[0].ID = id
				_, err := svc.Get(context.Background(), 7, 1)
				require.NoError(t, err)
			}
			require.Equal(t, int64(sharedUsageCacheLimit), quota.calls.Load())
			accounts.accounts[0].ID = 2000
			full, err := svc.Get(context.Background(), 7, 1)
			require.NoError(t, err)
			require.Equal(t, "unavailable", full.Status)
			require.Equal(t, int64(sharedUsageCacheLimit), quota.calls.Load(), "do not evict live cooldowns to query a new account")
			accounts.accounts[0].ID = 9
			cached, err := svc.Get(context.Background(), 7, 1)
			require.NoError(t, err)
			require.Equal(t, first.Status, cached.Status)
			require.Equal(t, int64(sharedUsageCacheLimit), quota.calls.Load())
			*now = now.Add(time.Minute)
			_, err = svc.Get(context.Background(), 7, 1)
			require.NoError(t, err)
			require.Equal(t, int64(sharedUsageCacheLimit+1), quota.calls.Load())
			require.LessOrEqual(t, len(svc.cache), sharedUsageCacheLimit)
		})
	}
}
