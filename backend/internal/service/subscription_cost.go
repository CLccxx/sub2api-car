package service

import (
	"context"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SubscriptionCostEstimate deliberately separates the subscription allowance
// from credit spend. Only EstimatedTotalCost may feed carpool limits.
type SubscriptionCostEstimate struct {
	EstimatedTotalCost *float64 `json:"estimated_total_cost"`
	CreditsUsedCost    *float64 `json:"credits_used_cost"`
	EstimateFrozen     bool     `json:"estimate_frozen"`
	EstimateSource     string   `json:"estimate_source,omitempty"`
}

type SubscriptionCostReset struct {
	Key        string
	OccurredAt time.Time
}

type SubscriptionCostState struct {
	Observation  CarpoolResetObservation `json:"observation"`
	CycleStart   time.Time               `json:"cycle_start"`
	ResetKey     string                  `json:"reset_key"`
	PendingReset bool                    `json:"pending_reset"`
	SeenBelow    bool                    `json:"seen_below"`
	KnownCycle   bool                    `json:"known_cycle"`
	FirstFullAt  *time.Time              `json:"first_full_at,omitempty"`
	FrozenBase   *float64                `json:"frozen_base,omitempty"`
	Source       string                  `json:"source,omitempty"`
}

// AdvanceSubscriptionCostCycle uses independent account state: changes to group
// membership never unlock a frozen allowance. A receipt clears the old cycle
// immediately but requires settled usage before a new exhaustion can be accepted.
func AdvanceSubscriptionCostCycle(previous SubscriptionCostState, next CarpoolResetObservation, receipt *SubscriptionCostReset, identityStart time.Time) (SubscriptionCostState, bool) {
	if !previous.Observation.ObservedAt.IsZero() && (next.ObservedAt.Before(previous.Observation.ObservedAt) || next.ResetAt.Before(previous.Observation.ResetAt.Add(-time.Minute))) {
		return previous, false
	}
	state := previous
	if state.CycleStart.IsZero() {
		state.CycleStart = next.ResetAt.Add(-7 * 24 * time.Hour)
		if identityStart.After(state.CycleStart) {
			state.CycleStart = identityStart
		}
	} else if next.ResetAt.After(previous.Observation.ResetAt.Add(time.Minute)) {
		start := next.ResetAt.Add(-7 * 24 * time.Hour)
		if start.Before(previous.Observation.ObservedAt.Add(-time.Minute)) || start.After(next.ObservedAt.Add(time.Minute)) {
			return previous, false
		}
		state = SubscriptionCostState{CycleStart: start, ResetKey: previous.ResetKey, KnownCycle: true}
	} else {
		next.ResetAt = previous.Observation.ResetAt
	}
	if receipt != nil && receipt.Key != state.ResetKey && !receipt.OccurredAt.After(next.ObservedAt) {
		state.ResetKey = receipt.Key
		if receipt.OccurredAt.After(state.CycleStart) {
			state.CycleStart = receipt.OccurredAt
			state.FrozenBase = nil
			state.FirstFullAt = nil
			state.Source = ""
			state.SeenBelow = false
			state.PendingReset = true
			state.KnownCycle = true
		}
	}
	if state.PendingReset && next.Utilization < 100 && next.ObservedAt.After(state.CycleStart) {
		state.PendingReset = false
	}
	state.Observation = next
	return state, true
}

func UpdateSubscriptionCostEstimate(state *SubscriptionCostState, utilization, cost float64) *SubscriptionCostEstimate {
	if state == nil || state.PendingReset || !validSharedUsageNumber(cost) || !validSharedUsageNumber(utilization) {
		return nil
	}
	if state.FrozenBase == nil {
		if utilization < 100 {
			state.SeenBelow = true
		}
		if utilization <= 0 || (cost <= 0 && (utilization < 100 || state.SeenBelow || state.KnownCycle)) {
			return nil
		}
		if utilization >= 100 {
			base := 1600.0
			state.Source = "legacy_default"
			if state.SeenBelow || state.KnownCycle {
				base = cost
				state.Source = "observed"
			}
			state.FrozenBase = &base
		} else {
			base := cost * 100 / utilization
			if !validSharedUsageNumber(base) {
				return nil
			}
			credits := 0.0
			return &SubscriptionCostEstimate{EstimatedTotalCost: &base, CreditsUsedCost: &credits, EstimateSource: "dynamic"}
		}
	}
	base := *state.FrozenBase
	credits := math.Max(0, cost-base)
	return &SubscriptionCostEstimate{EstimatedTotalCost: &base, CreditsUsedCost: &credits, EstimateFrozen: true, EstimateSource: state.Source}
}

type SubscriptionCostRepository interface {
	Observe(context.Context, int64, string, CarpoolResetObservation) (*SubscriptionCostEstimate, error)
	Track(context.Context, int64, string, CarpoolResetObservation) error
	Candidates(context.Context, int64, int) ([]int64, error)
	RegisterPending(context.Context, SubscriptionCostPendingUsage) (int64, error)
	CompletePending(context.Context, int64, int64) error
	CleanupPending(context.Context, time.Time) (int64, error)
}

const (
	subscriptionCostSampleInterval      = time.Minute
	subscriptionCostSampleHistoryLimit  = 1024
	subscriptionCostPeriodicReservation = 32
)

type SubscriptionCostService struct {
	repo       SubscriptionCostRepository
	accounts   AccountRepository
	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
	samples    chan int64
	periodic   chan int64
	pending    map[int64]struct{}
	nextSample map[int64]time.Time
	now        func() time.Time
}

func NewSubscriptionCostService(repo SubscriptionCostRepository, accounts AccountRepository) *SubscriptionCostService {
	return &SubscriptionCostService{repo: repo, accounts: accounts, samples: make(chan int64, 64), periodic: make(chan int64, 32), pending: make(map[int64]struct{}), nextSample: make(map[int64]time.Time), now: time.Now}
}

func (s *SubscriptionCostService) ObserveUsage(ctx context.Context, accountID int64, expectedIdentity string, usage *OpenAIQuotaUsage) *SubscriptionCostEstimate {
	return s.observeUsage(ctx, accountID, expectedIdentity, usage, true)
}

func (s *SubscriptionCostService) observeUsage(ctx context.Context, accountID int64, expectedIdentity string, usage *OpenAIQuotaUsage, projection bool) *SubscriptionCostEstimate {
	if s == nil || s.repo == nil || s.accounts == nil {
		return nil
	}
	account, err := s.accounts.GetByID(ctx, accountID)
	if err != nil || account == nil || !account.IsOpenAIOAuth() || account.IsShadow() {
		return nil
	}
	identity := strings.TrimSpace(account.GetCredential("chatgpt_account_id"))
	if identity == "" || (expectedIdentity != "" && strings.TrimSpace(expectedIdentity) != identity) {
		return nil
	}
	observation, ok := ReadCarpoolResetObservation(usage, identity, time.Now())
	if !ok {
		return nil
	}
	observation.CostEvidenceAt = usage.costEvidenceAt
	if !usage.costObservedAt.IsZero() {
		observation.ObservedAt = usage.costObservedAt
	}
	if !projection {
		_ = s.repo.Track(ctx, accountID, identity, observation)
		return nil
	}
	result, err := s.repo.Observe(ctx, accountID, identity, observation)
	if err != nil {
		return nil
	}
	return result
}

// Strict window lengths are essential: a five-hour full header is not proof of
// seven-day exhaustion. Unknown window lengths are left for the read-only sampler.
func subscriptionCostUsageFromHeaders(headers http.Header, identity string, observed time.Time) *OpenAIQuotaUsage {
	snapshot := ParseCodexRateLimitHeaders(headers)
	if snapshot == nil {
		return nil
	}
	resetBase := observed
	if stamp, err := http.ParseTime(headers.Get("Date")); err == nil {
		if stamp.Before(observed.Add(-2*time.Minute)) || stamp.After(observed.Add(time.Minute)) {
			return nil
		}
		resetBase = stamp
	}
	usage := &OpenAIQuotaUsage{AccountID: identity, FetchedAt: observed.Unix(), costObservedAt: observed, RateLimit: &OpenAIRateLimit{}}
	makeWindow := func(percent *float64, seconds, minutes *int) *OpenAIRateLimitWindow {
		if percent == nil || seconds == nil || minutes == nil || *minutes != 10080 {
			return nil
		}
		return &OpenAIRateLimitWindow{UsedPercent: *percent, LimitWindowSeconds: 604800, ResetAt: resetBase.Add(time.Duration(*seconds) * time.Second).Unix()}
	}
	usage.RateLimit.PrimaryWindow = makeWindow(snapshot.PrimaryUsedPercent, snapshot.PrimaryResetAfterSeconds, snapshot.PrimaryWindowMinutes)
	usage.RateLimit.SecondaryWindow = makeWindow(snapshot.SecondaryUsedPercent, snapshot.SecondaryResetAfterSeconds, snapshot.SecondaryWindowMinutes)
	return usage
}

// Eligible traffic observes only after Create confirms durable persistence.
// A best-effort fallback may still write a failed row but cannot freeze a base.
func writeSubscriptionCostUsage(ctx context.Context, repo UsageLogRepository, observer *SubscriptionCostService, account *Account, log *UsageLog, headers http.Header) bool {
	if observer == nil || repo == nil || account == nil || !account.IsOpenAIOAuth() || account.IsShadow() {
		return false
	}
	observed := UsageObservationTime(ctx)
	if observed.IsZero() {
		observed = time.Now()
	}
	identity := account.GetCredential("chatgpt_account_id")
	if log.AccountCostObservedAt != nil {
		observed = *log.AccountCostObservedAt
	}
	usage := subscriptionCostUsageFromHeaders(headers, identity, observed)
	if usage != nil && log.AccountCostStartedAt != nil {
		usage.costEvidenceAt = *log.AccountCostStartedAt
	}
	writeCtx, cancel := detachedBillingContext(ctx)
	defer cancel()
	if _, err := repo.Create(writeCtx, log); err != nil {
		return false
	}
	if log.AccountCostPendingID != nil && *log.AccountCostPendingID > 0 {
		_ = observer.repo.CompletePending(writeCtx, *log.AccountCostPendingID, log.ID)
	}
	if _, valid := ReadCarpoolResetObservation(usage, identity, time.Now()); valid {
		observer.observeUsage(writeCtx, account.ID, identity, usage, false)
	} else {
		observer.RequestSample(account.ID)
	}
	return true
}

// Start bounds both background and headerless (including WebSocket) sampling.
// Its only upstream operation is QueryUsageReadOnly; it never consumes a card.
func (s *SubscriptionCostService) Start(quota sharedUsageQuotaReader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil || quota == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		var workers sync.WaitGroup
		for i := 0; i < 4; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()

				for {
					var id int64
					// Reserve progress for the periodic scan even under sustained traffic.
					select {
					case id = <-s.periodic:
					default:
					}
					if id == 0 {
						select {
						case <-ctx.Done():
							return
						case id = <-s.periodic:
						case id = <-s.samples:
						}
					}
					readCtx, done := context.WithTimeout(ctx, 25*time.Second)
					_, _ = quota.QueryUsageReadOnly(readCtx, id)
					done()
					s.mu.Lock()
					delete(s.pending, id)
					s.nextSample[id] = s.now().Add(subscriptionCostSampleInterval)
					s.mu.Unlock()
					if ctx.Err() != nil {
						return
					}
				}
			}()
		}
		defer workers.Wait()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		var cursor int64
		scan := func() {
			s.mu.Lock()
			s.pruneSampleCooldownsLocked(s.now())
			s.mu.Unlock()
			scanCtx, done := context.WithTimeout(ctx, 5*time.Second)
			defer done()
			_, _ = s.repo.CleanupPending(scanCtx, time.Now().Add(-14*24*time.Hour))
			ids, err := s.repo.Candidates(scanCtx, cursor, 32)
			if err != nil {
				return
			}
			if len(ids) == 0 {
				cursor = 0
				return
			}
			for _, id := range ids {
				if !s.queueSample(id, true) {
					return
				}
				cursor = id
			}
			if len(ids) < 32 {
				cursor = 0
			}
		}
		scan()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				scan()
			}
		}
	}()
}

func (s *SubscriptionCostService) RequestSample(id int64) { s.queueSample(id, false) }
func (s *SubscriptionCostService) queueSample(id int64, periodic bool) bool {
	if s == nil || id <= 0 || s.samples == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.pending[id]; exists {
		return true
	}
	now := s.now()
	s.pruneSampleCooldownsLocked(now)
	if until, exists := s.nextSample[id]; exists && until.After(now) {
		return true
	}
	// Keep recent cooldowns under memory pressure; reserving capacity for the
	// periodic scan prevents busy traffic from starving unattended accounts.
	limit := subscriptionCostSampleHistoryLimit
	if !periodic {
		limit -= subscriptionCostPeriodicReservation
	}
	if len(s.nextSample)+len(s.pending) >= limit {
		return false
	}
	queue := s.samples
	if periodic {
		queue = s.periodic
	}
	select {
	case queue <- id:
		s.pending[id] = struct{}{}
		return true
	default:
		return false
	}
}

// Caller holds mu. The cap includes queued/in-flight accounts, so completion
// cannot grow the history map past the bound or evict a still-active cooldown.
func (s *SubscriptionCostService) pruneSampleCooldownsLocked(now time.Time) {
	for id, until := range s.nextSample {
		if !until.After(now) {
			delete(s.nextSample, id)
		}
	}
}

func (s *SubscriptionCostService) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
	}
}

type usageObservationTimeKey struct{}

// WithUsageObservationTime captures request completion before deferred billing.
// It preserves the occurrence time even when a worker runs across a reset.
func WithUsageObservationTime(ctx context.Context, observed time.Time) context.Context {
	return context.WithValue(ctx, usageObservationTimeKey{}, observed)
}
func UsageObservationTime(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	observed, _ := ctx.Value(usageObservationTimeKey{}).(time.Time)
	return observed
}

func subscriptionCostAccountingTime(ctx context.Context, account *Account, pricingAt time.Time, headers http.Header, fallback time.Time) time.Time {
	if account == nil || !account.IsOpenAIOAuth() || account.IsShadow() {
		return fallback
	}
	if captured, ok := ctx.Value(subscriptionCostStartKey{}).(time.Time); ok && !captured.IsZero() {
		return captured
	}
	// Request time is preferred: a response completed after a reset can still
	// belong to a request dispatched under the preceding account/cycle.
	if !pricingAt.IsZero() && !pricingAt.After(fallback) {
		return pricingAt
	}
	if stamp, err := http.ParseTime(headers.Get("Date")); err == nil && !stamp.After(fallback) {
		return stamp
	}
	if captured := UsageObservationTime(ctx); !captured.IsZero() && !captured.After(fallback) {
		return captured
	}
	return fallback
}

func stampSubscriptionCostAttribution(ctx context.Context, log *UsageLog, account *Account, pricingAt time.Time, headers http.Header) {
	if log == nil || account == nil || !account.IsOpenAIOAuth() || account.IsShadow() {
		return
	}
	observed := UsageObservationTime(ctx)
	if observed.IsZero() {
		observed = log.CreatedAt
	}
	started := subscriptionCostAccountingTime(ctx, account, pricingAt, headers, observed)
	identity := strings.TrimSpace(account.GetCredential("chatgpt_account_id"))
	hash := ""
	if identity != "" {
		hash = CarpoolResetIdentityHash(identity)
	}
	if ticket, ok := ctx.Value(subscriptionCostPendingKey{}).(int64); ok {
		log.AccountCostPendingID = &ticket
	}
	log.AccountCostStartedAt = &started
	log.AccountCostObservedAt = &observed
	log.AccountCostIdentityHash = &hash
}
