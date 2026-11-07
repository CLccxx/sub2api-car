package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionCostFreezeAndCredits(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	observation := CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 90}
	state := SubscriptionCostState{}
	next, ok := AdvanceSubscriptionCostCycle(state, observation, nil, time.Time{})
	require.True(t, ok)
	estimate := UpdateSubscriptionCostEstimate(&next, observation.Utilization, 1800)
	require.Equal(t, 2000.0, *estimate.EstimatedTotalCost)
	require.False(t, estimate.EstimateFrozen)
	observation.ObservedAt = now.Add(time.Second)
	observation.Utilization = 100
	next, ok = AdvanceSubscriptionCostCycle(next, observation, nil, time.Time{})
	require.True(t, ok)
	estimate = UpdateSubscriptionCostEstimate(&next, 100, 2000)
	require.Equal(t, "observed", estimate.EstimateSource)
	require.Equal(t, 2000.0, *estimate.EstimatedTotalCost)
	estimate = UpdateSubscriptionCostEstimate(&next, 100, 2200)
	require.Equal(t, 2000.0, *estimate.EstimatedTotalCost)
	require.Equal(t, 200.0, *estimate.CreditsUsedCost)
}

func TestSubscriptionCostFirstFullAndInvalidCost(t *testing.T) {
	state := SubscriptionCostState{}
	require.Nil(t, UpdateSubscriptionCostEstimate(&state, 100, math.NaN()))
	estimate := UpdateSubscriptionCostEstimate(&state, 100, 1800)
	require.Equal(t, 1600.0, *estimate.EstimatedTotalCost)
	require.Equal(t, 200.0, *estimate.CreditsUsedCost)
	require.Equal(t, "legacy_default", estimate.EstimateSource)
	for members, want := range map[int]float64{4: 500, 5: 420} {
		got, ok := carpoolWeeklyLimit(*estimate.EstimatedTotalCost, members)
		require.True(t, ok)
		require.Equal(t, want, got)
	}
}

func TestSubscriptionCostResetsAndStale(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 100}
	state, ok := AdvanceSubscriptionCostCycle(SubscriptionCostState{}, old, nil, time.Time{})
	require.True(t, ok)
	UpdateSubscriptionCostEstimate(&state, 100, 1800)
	stale := old
	stale.ObservedAt = now.Add(-time.Second)
	_, ok = AdvanceSubscriptionCostCycle(state, stale, nil, time.Time{})
	require.False(t, ok)
	receipt := &SubscriptionCostReset{Key: "reset-1", OccurredAt: now.Add(time.Second)}
	nextObs := old
	nextObs.ObservedAt = now.Add(2 * time.Second)
	next, ok := AdvanceSubscriptionCostCycle(state, nextObs, receipt, time.Time{})
	require.True(t, ok)
	require.True(t, next.PendingReset)
	require.Nil(t, UpdateSubscriptionCostEstimate(&next, 100, 100))
	require.Equal(t, receipt.OccurredAt, next.CycleStart)
	nextObs.ObservedAt = now.Add(3 * time.Second)
	nextObs.Utilization = 10
	next, ok = AdvanceSubscriptionCostCycle(next, nextObs, receipt, time.Time{})
	require.True(t, ok)
	require.False(t, next.PendingReset)
	estimate := UpdateSubscriptionCostEstimate(&next, 10, 100)
	require.Equal(t, 1000.0, *estimate.EstimatedTotalCost)
	nextObs.ObservedAt = now.Add(time.Hour)
	nextObs.ResetAt = nextObs.ObservedAt.Add(7 * 24 * time.Hour)
	nextObs.Utilization = 5
	next, ok = AdvanceSubscriptionCostCycle(next, nextObs, receipt, time.Time{})
	require.True(t, ok)
	require.Nil(t, next.FrozenBase)
	require.Equal(t, nextObs.ObservedAt, next.CycleStart)
	require.False(t, next.SeenBelow)
}

type subscriptionCostRepoSpy struct {
	SubscriptionCostRepository
	observe    func(context.Context, int64, string, CarpoolResetObservation) (*SubscriptionCostEstimate, error)
	candidates []int64
}

func (s *subscriptionCostRepoSpy) Observe(ctx context.Context, id int64, identity string, o CarpoolResetObservation) (*SubscriptionCostEstimate, error) {
	return s.observe(ctx, id, identity, o)
}
func (s *subscriptionCostRepoSpy) Track(ctx context.Context, id int64, identity string, observation CarpoolResetObservation) error {
	_, err := s.Observe(ctx, id, identity, observation)
	return err
}
func (s *subscriptionCostRepoSpy) Candidates(context.Context, int64, int) ([]int64, error) {
	return s.candidates, nil
}

func TestSubscriptionCostRejectsInvalidObservations(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, scenario := range []string{"five-hour", "stale", "missing-percent", "wrong-identity", "shadow", "missing-reset", "NaN"} {
		t.Run(scenario, func(t *testing.T) {
			account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "identity"}}
			usage := &OpenAIQuotaUsage{AccountID: "identity", FetchedAt: now.Unix(), RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{UsedPercent: 100, LimitWindowSeconds: 604800, ResetAt: now.Add(time.Hour).Unix()}}}
			switch scenario {
			case "five-hour":
				usage.RateLimit.PrimaryWindow.LimitWindowSeconds = 18000
			case "stale":
				usage.FetchedAt = now.Add(-3 * time.Minute).Unix()
			case "missing-percent":
				usage.RateLimit.PrimaryWindow.usedPercentMissing = true
			case "wrong-identity":
				usage.AccountID = "old"
			case "shadow":
				parent := int64(10)
				account.ParentAccountID = &parent
			case "missing-reset":
				usage.RateLimit.PrimaryWindow.ResetAt = 0
			case "NaN":
				usage.RateLimit.PrimaryWindow.UsedPercent = math.NaN()
			}
			repo := &subscriptionCostRepoSpy{observe: func(context.Context, int64, string, CarpoolResetObservation) (*SubscriptionCostEstimate, error) {
				t.Fatal("invalid observations cannot reach durable state")
				return nil, nil
			}}
			svc := NewSubscriptionCostService(repo, &openAIRecordUsageAccountRepoStub{account: account})
			require.Nil(t, svc.ObserveUsage(context.Background(), 9, "identity", usage))
		})
	}
}

func TestSubscriptionCostGatewayObservesAfterBillingAndPersistence(t *testing.T) {
	for _, mode := range []string{"HTTP", "WS", "5h", "persistence-failed", "billing-failed"} {
		t.Run(mode, func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			billingRepo := &openAIRecordUsageBillingRepoStub{}
			gateway := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "identity"}}
			called := false
			repo := &subscriptionCostRepoSpy{observe: func(_ context.Context, id int64, identity string, obs CarpoolResetObservation) (*SubscriptionCostEstimate, error) {
				called = true
				require.Equal(t, 1, billingRepo.calls)
				require.Equal(t, 1, usageRepo.calls)
				require.Greater(t, usageRepo.lastLog.TotalCost, 0.0)
				state := SubscriptionCostState{SeenBelow: true}
				estimate := UpdateSubscriptionCostEstimate(&state, obs.Utilization, 1800+usageRepo.lastLog.TotalCost)
				require.Equal(t, 1800+usageRepo.lastLog.TotalCost, *estimate.EstimatedTotalCost, "the triggering row must be included in the first freeze")
				return estimate, nil
			}}
			observer := NewSubscriptionCostService(repo, &openAIRecordUsageAccountRepoStub{account: account})
			gateway.subscriptionCost = observer
			headers := http.Header{"X-Codex-Primary-Used-Percent": []string{"100"}, "X-Codex-Primary-Window-Minutes": []string{"10080"}, "X-Codex-Primary-Reset-After-Seconds": []string{"3600"}}
			switch mode {
			case "WS":
				headers = nil
			case "5h":
				headers.Set("x-codex-primary-window-minutes", "300")
			case "persistence-failed":
				usageRepo.err = errors.New("write failed")
			case "billing-failed":
				billingRepo.err = errors.New("billing failed")
			}
			err := gateway.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: &OpenAIForwardResult{RequestID: "cost-request", Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 1000}, UpstreamHeaders: headers}, APIKey: &APIKey{ID: 2}, User: &User{ID: 3}, Account: account})
			if mode == "billing-failed" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, mode == "HTTP", called)
			if mode == "WS" || mode == "5h" {
				select {
				case id := <-observer.samples:
					require.Equal(t, int64(9), id)
					require.NotNil(t, usageRepo.lastLog)
				default:
					t.Fatal("headerless and non-seven-day traffic must trigger read-only sampling after persistence")
				}
			}
		})
	}
}

func TestSubscriptionCostSamplerSeedsWithoutUIAndStops(t *testing.T) {
	sampled := make(chan int64, 1)
	repo := &subscriptionCostRepoSpy{candidates: []int64{9}}
	svc := NewSubscriptionCostService(repo, nil)
	svc.Start(subscriptionCostQuotaFunc(func(ctx context.Context, id int64) (*OpenAIQuotaUsage, error) {
		select {
		case sampled <- id:
		default:
		}
		return nil, nil
	}))
	defer svc.Stop()
	select {
	case id := <-sampled:
		require.Equal(t, int64(9), id)
	case <-time.After(time.Second):
		t.Fatal("startup must sample accounts without visits")
	}
	svc.Stop()
	select {
	case <-svc.done:
	default:
		t.Fatal("sampler must stop cleanly")
	}
}

type subscriptionCostQuotaFunc func(context.Context, int64) (*OpenAIQuotaUsage, error)

func (f subscriptionCostQuotaFunc) QueryUsageReadOnly(ctx context.Context, id int64) (*OpenAIQuotaUsage, error) {
	return f(ctx, id)
}

func TestSubscriptionCostSharedAdminAndQuotaUseSameBase(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "identity"}, Extra: map[string]any{"codex_5h_used_percent": 0, "codex_5h_reset_at": now.Add(time.Hour).Format(time.RFC3339), "codex_7d_used_percent": 100, "codex_7d_reset_at": now.Add(time.Hour).Format(time.RFC3339), "codex_usage_updated_at": now.Format(time.RFC3339)}}
	accounts := &stubQuotaAccountRepo{accounts: map[int64]*Account{9: account}}
	token := NewOpenAITokenProvider(accounts, &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "fake"}}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method, "observers must never consume cards")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/usage") {
			_, _ = fmt.Fprintf(w, `{"account_id":"identity","rate_limit":{"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":%d}}}`, now.Add(time.Hour).Unix())
		} else {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	base, credits := 2000.0, 200.0
	repo := &subscriptionCostRepoSpy{observe: func(context.Context, int64, string, CarpoolResetObservation) (*SubscriptionCostEstimate, error) {
		return &SubscriptionCostEstimate{EstimatedTotalCost: &base, CreditsUsedCost: &credits, EstimateFrozen: true, EstimateSource: "observed"}, nil
	}}
	quota := NewOpenAIQuotaService(accounts, nil, token, newQuotaRedirectingFactory(server), nil)
	quota.subscriptionCost = NewSubscriptionCostService(repo, accounts)
	stats := &subscriptionCostStatsRepo{cost: 2200}
	shared := querySharedAccountUsage(context.Background(), 9, quota, stats, time.Now).view
	require.Equal(t, 2000.0, *shared.SevenDay.EstimatedTotalCost)
	require.Equal(t, 200.0, *shared.SevenDay.CreditsUsedCost)
	admin := &AccountUsageService{accountRepo: accounts, usageLogRepo: stats, openAIQuotaService: quota}
	result, err := admin.getOpenAIUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Equal(t, *shared.SevenDay.EstimatedTotalCost, *result.SevenDay.EstimatedTotalCost)
	require.Equal(t, *shared.SevenDay.CreditsUsedCost, *result.SevenDay.CreditsUsedCost)
	_, _, bindings, _, _, clock := sharedUsageFixture(t)
	*clock = now
	target := &carpoolRepoStub{targets: []CarpoolQuotaTarget{{GroupID: 10, Members: 4}}, applied: true}
	carpool := NewCarpoolQuotaService(target, bindings, quota, stats, nil)
	carpool.run(context.Background())
	require.Equal(t, 600.0, target.limit, "2200 combined cost must never raise the 2000 subscription-based cap")
}

type subscriptionCostStatsRepo struct {
	UsageLogRepository
	cost float64
}

func (s *subscriptionCostStatsRepo) GetAccountWindowStats(context.Context, int64, time.Time) (*usagestats.AccountStats, error) {
	return &usagestats.AccountStats{Cost: s.cost}, nil
}

func TestSubscriptionCostLegacyEmptyAndKnownNaturalCycle(t *testing.T) {
	state := SubscriptionCostState{}
	estimate := UpdateSubscriptionCostEstimate(&state, 100, 0)
	require.Equal(t, 1600.0, *estimate.EstimatedTotalCost)
	require.Zero(t, *estimate.CreditsUsedCost)
	require.Equal(t, "legacy_default", estimate.EstimateSource)
	now := time.Now().UTC()
	state.Observation = CarpoolResetObservation{ObservedAt: now.Add(-time.Hour), ResetAt: now}
	state.CycleStart = now.Add(-7 * 24 * time.Hour)
	next, ok := AdvanceSubscriptionCostCycle(state, CarpoolResetObservation{ObservedAt: now.Add(time.Minute), ResetAt: now.Add(7 * 24 * time.Hour), Utilization: 100}, nil, time.Time{})
	require.True(t, ok)
	estimate = UpdateSubscriptionCostEstimate(&next, 100, 1800)
	require.Equal(t, 1800.0, *estimate.EstimatedTotalCost)
	require.Equal(t, "observed", estimate.EstimateSource)
}

func TestSubscriptionCostSamplerCoalescesTrafficAndKeepsPeriodicProgress(t *testing.T) {
	sampled := make(chan int64, 100)
	svc := NewSubscriptionCostService(&subscriptionCostRepoSpy{candidates: []int64{8}}, nil)
	for i := 0; i < 1000; i++ {
		svc.RequestSample(9)
	}
	require.Len(t, svc.samples, 1, "one busy account cannot fill the queue")
	svc.Start(subscriptionCostQuotaFunc(func(ctx context.Context, id int64) (*OpenAIQuotaUsage, error) { sampled <- id; return nil, nil }))
	defer svc.Stop()
	deadline := time.After(time.Second)
	for {
		select {
		case id := <-sampled:
			if id == 8 {
				return
			}
		case <-deadline:
			t.Fatal("quiet account must make progress independently of busy traffic")
		}
	}
}

func TestSubscriptionCostDelayedAccountingKeepsOriginalTime(t *testing.T) {
	observed := time.Now().Add(-time.Minute)
	ctx := WithUsageObservationTime(context.Background(), observed)
	repo := &openAIRecordUsageLogRepoStub{inserted: true}
	gateway := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.NoError(t, gateway.RecordUsage(ctx, &OpenAIRecordUsageInput{Result: &OpenAIForwardResult{RequestID: "late", Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 1000}}, Account: account, APIKey: &APIKey{ID: 2}, User: &User{ID: 3}}))
	require.Equal(t, observed, *repo.lastLog.AccountCostObservedAt, "deferred billing cannot move old cost into a new reset/identity epoch")
	require.True(t, repo.lastLog.CreatedAt.After(observed), "storage timestamp remains unchanged")
}

func (s *subscriptionCostRepoSpy) RegisterPending(context.Context, SubscriptionCostPendingUsage) (int64, error) {
	return 1, nil
}
func (s *subscriptionCostRepoSpy) CompletePending(context.Context, int64, int64) error { return nil }

func (s *subscriptionCostRepoSpy) CleanupPending(context.Context, time.Time) (int64, error) {
	return 0, nil
}

type subscriptionCostRegistrationSpy struct {
	subscriptionCostRepoSpy
	register func(SubscriptionCostPendingUsage) (int64, error)
}

func (s *subscriptionCostRegistrationSpy) RegisterPending(_ context.Context, p SubscriptionCostPendingUsage) (int64, error) {
	return s.register(p)
}
func TestSubscriptionCostRegistersBeforeEnqueueAndFailsClosed(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			registered := false
			called := false
			repo := &subscriptionCostRegistrationSpy{register: func(p SubscriptionCostPendingUsage) (int64, error) {
				registered = true
				require.False(t, called)
				require.Equal(t, int64(9), p.AccountID)
				if fail {
					return 0, errors.New("unavailable")
				}
				return 42, nil
			}}
			observer := NewSubscriptionCostService(repo, nil)
			account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "identity"}}
			wrapped, inline := wrapSubscriptionCostAccountingTask(context.Background(), func(ctx context.Context) {
				called = true
				id, _ := ctx.Value(subscriptionCostPendingKey{}).(int64)
				if fail {
					require.Equal(t, int64(-1), id)
				} else {
					require.Equal(t, int64(42), id)
				}
			}, observer, NewUsageAccountingScope(account, nil, time.Now().Add(-time.Second)))
			require.True(t, registered)
			require.False(t, called)
			require.Equal(t, fail, inline)
			wrapped(context.Background())
			require.True(t, called)
		})
	}
}

func TestSubscriptionCostAttemptStartOverridesEarlierPricingAdmission(t *testing.T) {
	pricingAt := time.Now().Add(-2 * time.Second)
	resetAt := pricingAt.Add(time.Second)
	forwardStart := resetAt.Add(500 * time.Millisecond)
	repo := &subscriptionCostRegistrationSpy{register: func(p SubscriptionCostPendingUsage) (int64, error) {
		require.Equal(t, forwardStart, p.StartedAt)
		require.True(t, p.StartedAt.After(resetAt))
		return 42, nil
	}}
	account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "identity"}}
	observer := NewSubscriptionCostService(repo, nil)
	wrapped, failed := wrapSubscriptionCostAccountingTask(context.Background(), func(ctx context.Context) {
		log := &UsageLog{CreatedAt: time.Now()}
		stampSubscriptionCostAttribution(ctx, log, account, pricingAt, nil)
		require.Equal(t, forwardStart, *log.AccountCostStartedAt, "admission before reset must not exclude an attempt dispatched after reset")
	}, observer, NewUsageAccountingScope(account, nil, forwardStart))
	require.False(t, failed)
	wrapped(context.Background())
}

func TestSubscriptionCostSamplerSuppressesCompletedSuccessAndFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			reads := make(chan int64, 128)
			svc := NewSubscriptionCostService(&subscriptionCostRepoSpy{}, nil)
			var current atomic.Int64
			current.Store(time.Now().UnixNano())
			svc.now = func() time.Time { return time.Unix(0, current.Load()) }
			svc.Start(subscriptionCostQuotaFunc(func(_ context.Context, id int64) (*OpenAIQuotaUsage, error) {
				reads <- id
				if failed {
					return nil, errors.New("upstream unavailable")
				}
				return &OpenAIQuotaUsage{}, nil
			}))
			defer svc.Stop()
			svc.RequestSample(9)
			select {
			case <-reads:
			case <-time.After(time.Second):
				t.Fatal("initial sample missing")
			}
			require.Eventually(t, func() bool { svc.mu.Lock(); defer svc.mu.Unlock(); _, pending := svc.pending[9]; return !pending }, time.Second, time.Millisecond)
			for i := 0; i < 100; i++ {
				svc.RequestSample(9)
				require.True(t, svc.queueSample(9, true), "recent reads must not block the periodic cursor")
			}
			select {
			case <-reads:
				t.Fatal("a completed sample must suppress sequential requests for one minute, including failures")
			case <-time.After(30 * time.Millisecond):
			}
			current.Add(int64(time.Minute))
			svc.RequestSample(9)
			select {
			case id := <-reads:
				require.Equal(t, int64(9), id)
			case <-time.After(time.Second):
				t.Fatal("cooldown expiry must permit another read")
			}
		})
	}
}

func TestSubscriptionCostSamplerBoundsCooldownHistoryAndReservesPeriodicCapacity(t *testing.T) {
	svc := NewSubscriptionCostService(&subscriptionCostRepoSpy{}, nil)
	now := time.Now()
	svc.now = func() time.Time { return now }
	for id := int64(1); id <= subscriptionCostSampleHistoryLimit-subscriptionCostPeriodicReservation; id++ {
		svc.nextSample[id] = now.Add(time.Minute)
	}
	require.False(t, svc.queueSample(2000, false), "traffic must preserve space for periodic accounts")
	for i := int64(0); i < subscriptionCostPeriodicReservation; i++ {
		require.True(t, svc.queueSample(3000+i, true))
	}
	require.False(t, svc.queueSample(4000, true), "history plus pending work must stay bounded")
	require.True(t, svc.queueSample(1, false), "capacity pressure must not evict a live cooldown")
	require.NotContains(t, svc.pending, int64(1))
	require.LessOrEqual(t, len(svc.nextSample)+len(svc.pending), subscriptionCostSampleHistoryLimit)
	now = now.Add(time.Minute)
	require.True(t, svc.queueSample(4000, false), "expired history must release memory and scheduling capacity")
	require.Empty(t, svc.nextSample)
}
