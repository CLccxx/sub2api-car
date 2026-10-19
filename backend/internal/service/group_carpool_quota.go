package service

import (
	"context"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/robfig/cron/v3"
)

const carpoolQuotaSchedule = "CRON_TZ=Asia/Shanghai */30 * * * *"

type CarpoolQuotaTarget struct {
	GroupID  int64
	Members  int
	Revision time.Time
}

type CarpoolQuotaRepository interface {
	ListPending(context.Context, time.Time) ([]CarpoolQuotaTarget, error)
	Apply(context.Context, CarpoolQuotaTarget, *Account, float64, time.Time) (bool, error)
}

type CarpoolQuotaService struct {
	repo      CarpoolQuotaRepository
	accounts  AccountRepository
	quota     sharedUsageQuotaReader
	stats     sharedUsageStatsReader
	cache     APIKeyAuthCacheInvalidator
	now       func() time.Time
	cron      *cron.Cron
	ctx       context.Context
	cancel    context.CancelFunc
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewCarpoolQuotaService(repo CarpoolQuotaRepository, accounts AccountRepository, quota sharedUsageQuotaReader, stats sharedUsageStatsReader, cache APIKeyAuthCacheInvalidator) *CarpoolQuotaService {
	ctx, cancel := context.WithCancel(context.Background())
	return &CarpoolQuotaService{repo: repo, accounts: accounts, quota: quota, stats: stats, cache: cache, now: time.Now, ctx: ctx, cancel: cancel}
}

func (s *CarpoolQuotaService) Start() {
	s.startOnce.Do(func() {
		s.cron = cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DefaultLogger), cron.Recover(cron.DefaultLogger)))
		if _, err := s.cron.AddFunc(carpoolQuotaSchedule, func() { s.run(s.ctx) }); err != nil {
			logger.LegacyPrintf("service.carpool_quota", "schedule unavailable: %v", err)
			return
		}
		s.cron.Start()
	})
}

func (s *CarpoolQuotaService) Stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		if s.cron != nil {
			ctx := s.cron.Stop()
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	})
}

func (s *CarpoolQuotaService) run(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return
	}
	now := s.now().In(loc)
	slot := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute()/30*30, 0, 0, loc)
	targets, err := s.repo.ListPending(ctx, slot)
	if err != nil {
		logger.LegacyPrintf("service.carpool_quota", "list pending failed: %v", err)
		return
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		if !validCarpoolMembers(target.Members) {
			continue
		}
		readCtx, done := context.WithTimeout(ctx, 30*time.Second)
		s.refresh(readCtx, target, slot)
		done()
	}
}

func (s *CarpoolQuotaService) refresh(ctx context.Context, target CarpoolQuotaTarget, slot time.Time) {
	accounts, page, err := s.accounts.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 2}, "", "", "", "", target.GroupID, "")
	if err != nil || len(accounts) != 1 || (page != nil && page.Total != 1) {
		return
	}
	account := &accounts[0]
	if !account.IsOpenAIOAuth() || account.IsShadow() || account.Status != StatusActive ||
		(account.AutoPauseOnExpired && account.ExpiresAt != nil && !account.ExpiresAt.After(s.now())) {
		return
	}
	// This bypasses the visitor cache but never probes a model or resets quota.
	view := querySharedAccountUsage(ctx, account.ID, s.quota, s.stats, s.now).view
	now := s.now()
	window := view.SevenDay
	if view.UpdatedAt == nil || view.UpdatedAt.Before(now.Add(-2*time.Minute)) || view.UpdatedAt.After(now.Add(time.Minute)) ||
		window == nil || window.EstimatedTotalCost == nil || window.ResetsAt == nil || !window.ResetsAt.After(now) || window.ResetsAt.After(now.Add(7*24*time.Hour+time.Minute)) {
		return
	}
	limit, ok := carpoolWeeklyLimit(*window.EstimatedTotalCost, target.Members)
	if !ok {
		return
	}
	applied, err := s.repo.Apply(ctx, target, account, limit, slot)
	if err != nil {
		logger.LegacyPrintf("service.carpool_quota", "group=%d update failed: %v", target.GroupID, err)
		return
	}
	if applied {
		if s.cache != nil {
			s.cache.InvalidateAuthCacheByGroupID(ctx, target.GroupID)
		}
		logger.LegacyPrintf("service.carpool_quota", "group=%d members=%d weekly_limit=%.2f slot=%s", target.GroupID, target.Members, limit, slot.Format(time.RFC3339))
	}
}

func ProvideCarpoolQuotaService(repo CarpoolQuotaRepository, accounts AccountRepository, quota *OpenAIQuotaService, stats UsageLogRepository, keys *APIKeyService) *CarpoolQuotaService {
	svc := NewCarpoolQuotaService(repo, accounts, quota, stats, keys)
	svc.Start()
	return svc
}
