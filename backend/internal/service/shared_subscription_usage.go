package service

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"golang.org/x/sync/singleflight"
)

const sharedUsageCooldown = time.Minute
const sharedUsageCacheLimit = 256

func ProvideSharedSubscriptionUsageService(subscriptions UserSubscriptionRepository, accounts AccountRepository, stats UsageLogRepository, quota *OpenAIQuotaService) *SharedSubscriptionUsageService {
	return NewSharedSubscriptionUsageService(subscriptions, accounts, stats, quota)
}

// SharedSubscriptionUsage is an allowlisted user projection, never an Account
// or OpenAIQuotaUsage: those administrator types contain private identifiers.
type SharedSubscriptionUsage struct {
	Status             string             `json:"status"`
	UpdatedAt          *time.Time         `json:"updated_at"`
	RefreshAvailableAt *time.Time         `json:"refresh_available_at"`
	SevenDay           *SharedUsageWindow `json:"seven_day"`
	ResetCredits       *int               `json:"reset_credits"`
	Credits            *OpenAICredits     `json:"credits"`
}

type SharedUsageWindow struct {
	Utilization        float64    `json:"utilization"`
	ResetsAt           *time.Time `json:"resets_at"`
	EstimatedTotalCost *float64   `json:"estimated_total_cost"`
}

type sharedUsageQuotaReader interface {
	QueryUsageReadOnly(context.Context, int64) (*OpenAIQuotaUsage, error)
}

type sharedUsageStatsReader interface {
	GetAccountWindowStats(context.Context, int64, time.Time) (*usagestats.AccountStats, error)
}

type sharedUsageCacheEntry struct {
	view            SharedSubscriptionUsage
	creditExpiresAt *time.Time
	loading         bool
}

type SharedSubscriptionUsageService struct {
	subscriptions UserSubscriptionRepository
	accounts      AccountRepository
	stats         sharedUsageStatsReader
	quota         sharedUsageQuotaReader
	now           func() time.Time
	mu            sync.Mutex
	cache         map[int64]sharedUsageCacheEntry
	flight        singleflight.Group
}

func NewSharedSubscriptionUsageService(subscriptions UserSubscriptionRepository, accounts AccountRepository, stats sharedUsageStatsReader, quota sharedUsageQuotaReader) *SharedSubscriptionUsageService {
	return &SharedSubscriptionUsageService{
		subscriptions: subscriptions, accounts: accounts, stats: stats, quota: quota,
		now: time.Now, cache: make(map[int64]sharedUsageCacheEntry),
	}
}

func (s *SharedSubscriptionUsageService) Get(ctx context.Context, userID, subscriptionID int64) (*SharedSubscriptionUsage, error) {
	accountID, state, err := s.resolve(ctx, userID, subscriptionID)
	if err != nil {
		return nil, err
	}
	if state != "" {
		return &SharedSubscriptionUsage{Status: state}, nil
	}
	entry, ok := s.cached(accountID)
	if !ok {
		result := s.flight.DoChan(strconv.FormatInt(accountID, 10), func() (any, error) {
			if entry, ok := s.cached(accountID); ok {
				return entry, nil
			}
			if !s.reserve(accountID) {
				next := s.now().Add(sharedUsageCooldown)
				return sharedUsageCacheEntry{view: SharedSubscriptionUsage{Status: "unavailable", RefreshAvailableAt: &next}}, nil
			}
			// One visitor leaving must not cancel a read shared by other members.
			queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
			defer cancel()
			entry := s.query(queryCtx, accountID)
			s.store(accountID, entry)
			return entry, nil
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-result:
			if result.Err != nil {
				return nil, result.Err
			}
			entry, ok = result.Val.(sharedUsageCacheEntry)
			if !ok {
				return nil, infraerrors.New(503, "SHARED_USAGE_UNAVAILABLE", "Shared account usage is temporarily unavailable")
			}
		}
	}
	// Membership or the sole binding may have changed during the upstream read.
	currentID, currentState, err := s.resolve(ctx, userID, subscriptionID)
	if err != nil {
		return nil, err
	}
	if currentState != "" || currentID != accountID {
		return nil, infraerrors.Conflict("SHARED_ACCOUNT_CHANGED", "Shared account binding changed; refresh the subscription")
	}
	view := entry.view
	if view.SevenDay != nil && view.SevenDay.ResetsAt != nil && !view.SevenDay.ResetsAt.After(s.now()) {
		view.SevenDay = nil
	}
	if entry.creditExpiresAt != nil && !entry.creditExpiresAt.After(s.now()) {
		view.ResetCredits = nil
	}
	return &view, nil
}

func (s *SharedSubscriptionUsageService) resolve(ctx context.Context, userID, subscriptionID int64) (int64, string, error) {
	if userID <= 0 || subscriptionID <= 0 {
		return 0, "", ErrSubscriptionNotFound
	}
	sub, err := s.subscriptions.GetByID(ctx, subscriptionID)
	if err != nil {
		if errors.Is(err, ErrSubscriptionNotFound) {
			return 0, "", ErrSubscriptionNotFound
		}
		return 0, "", infraerrors.New(500, "SHARED_USAGE_UNAVAILABLE", "Shared account usage is unavailable")
	}
	if sub == nil || sub.UserID != userID || sub.DeletedAt != nil || sub.Status != SubscriptionStatusActive ||
		!sub.ExpiresAt.After(s.now()) || sub.Group == nil || sub.Group.Status != StatusActive {
		return 0, "", ErrSubscriptionNotFound
	}
	accounts, page, err := s.accounts.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 2}, "", "", "", "", sub.GroupID, "")
	if err != nil {
		return 0, "", infraerrors.New(500, "SHARED_USAGE_UNAVAILABLE", "Shared account usage is unavailable")
	}
	if len(accounts) > 1 || (page != nil && page.Total > 1) {
		return 0, "multiple_accounts", nil
	}
	if len(accounts) == 0 {
		return 0, "not_configured", nil
	}
	account := accounts[0]
	if !account.IsOpenAIOAuth() || account.IsShadow() {
		return 0, "unsupported_account", nil
	}
	return account.ID, "", nil
}

func (s *SharedSubscriptionUsageService) cached(accountID int64) (sharedUsageCacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[accountID]
	return entry, ok && !entry.loading && entry.view.RefreshAvailableAt.After(s.now())
}

// Reserve before querying: never evict an unexpired cooldown (including failed
// reads), and count in-flight accounts against the bound as well.
func (s *SharedSubscriptionUsageService) reserve(accountID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, entry := range s.cache {
		if !entry.loading && !entry.view.RefreshAvailableAt.After(now) {
			delete(s.cache, id)
		}
	}
	if len(s.cache) >= sharedUsageCacheLimit {
		return false
	}
	s.cache[accountID] = sharedUsageCacheEntry{loading: true}
	return true
}

func (s *SharedSubscriptionUsageService) store(accountID int64, entry sharedUsageCacheEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[accountID] = entry
}

func (s *SharedSubscriptionUsageService) query(ctx context.Context, accountID int64) sharedUsageCacheEntry {
	return querySharedAccountUsage(ctx, accountID, s.quota, s.stats, s.now)
}

func (s *SharedSubscriptionUsageService) Invalidate(accountID int64) {
	s.mu.Lock()
	delete(s.cache, accountID)
	s.mu.Unlock()
}

func querySharedAccountUsage(ctx context.Context, accountID int64, quota sharedUsageQuotaReader, statsReader sharedUsageStatsReader, clock func() time.Time) sharedUsageCacheEntry {
	entry := sharedUsageCacheEntry{view: SharedSubscriptionUsage{Status: "unavailable"}}
	usage, err := quota.QueryUsageReadOnly(ctx, accountID)
	now := clock()
	next := now.Add(sharedUsageCooldown)
	entry.view.RefreshAvailableAt = &next
	if err != nil || usage == nil {
		return entry
	}
	observed := now
	if usage.FetchedAt > 0 {
		observed = time.Unix(usage.FetchedAt, 0).UTC()
		entry.view.UpdatedAt = &observed
	}
	entry.view.Status = "no_data"
	if limits := usage.RateLimit; limits != nil {
		for _, window := range []*OpenAIRateLimitWindow{limits.PrimaryWindow, limits.SecondaryWindow} {
			if window == nil || window.usedPercentMissing || window.LimitWindowSeconds != 604800 || !validSharedUsageNumber(window.UsedPercent) {
				continue
			}
			var resetsAt *time.Time
			if window.ResetAt > 0 {
				reset := time.Unix(window.ResetAt, 0).UTC()
				resetsAt = &reset
			} else if window.ResetAfterSeconds > 0 && window.ResetAfterSeconds <= window.LimitWindowSeconds {
				reset := observed.Add(time.Duration(window.ResetAfterSeconds) * time.Second)
				resetsAt = &reset
			}
			if resetsAt != nil && !resetsAt.After(now) {
				continue
			}
			view := &SharedUsageWindow{Utilization: window.UsedPercent, ResetsAt: resetsAt}
			if resetsAt != nil && window.UsedPercent > 0 && statsReader != nil {
				stats, err := statsReader.GetAccountWindowStats(ctx, accountID, resetsAt.Add(-7*24*time.Hour))
				if err == nil && stats != nil && validSharedUsageNumber(stats.Cost) && stats.Cost > 0 {
					estimate := stats.Cost * 100 / window.UsedPercent
					if validSharedUsageNumber(estimate) {
						view.EstimatedTotalCost = &estimate
					}
				}
			}
			entry.view.SevenDay = view
			break
		}
	}
	if credits := usage.RateLimitResetCredits; credits != nil && !credits.availableCountMissing && credits.AvailableCount >= 0 {
		count := credits.AvailableCount
		entry.view.ResetCredits = &count
		for _, credit := range credits.Credits {
			if expiry, err := time.Parse(time.RFC3339, credit.ExpiresAt); err == nil {
				if entry.creditExpiresAt == nil || expiry.Before(*entry.creditExpiresAt) {
					entry.creditExpiresAt = &expiry
				}
			}
		}
	}
	if usage.Credits != nil && !usage.Credits.statusMissing {
		credits := *usage.Credits
		credits.Balance = nil
		if balance := usage.Credits.Balance; balance != nil {
			text := strings.TrimSpace(*balance)
			if number, err := strconv.ParseFloat(text, 64); err == nil && validSharedUsageNumber(number) {
				credits.Balance = &text
			}
		}
		entry.view.Credits = &credits
	}
	if entry.view.SevenDay != nil || entry.view.ResetCredits != nil || entry.view.Credits != nil {
		entry.view.Status = "ready"
	}
	return entry
}

func validSharedUsageNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}
