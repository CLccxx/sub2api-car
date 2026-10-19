package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/robfig/cron/v3"
)

const carpoolResetMonitorSchedule = "CRON_TZ=Asia/Shanghai */10 * * * *"

type CarpoolResetMonitorService struct {
	repo       CarpoolResetMonitorRepository
	quota      sharedUsageQuotaReader
	invalidate func(int64, int64) error
	now        func() time.Time
	ctx        context.Context
	cancel     context.CancelFunc
	runMu      sync.Mutex
	lifecycle  sync.Mutex
	cron       *cron.Cron
	seedDone   chan struct{}
	stopped    bool
}

func NewCarpoolResetMonitorService(repo CarpoolResetMonitorRepository, quota sharedUsageQuotaReader, invalidate func(int64, int64) error) *CarpoolResetMonitorService {
	ctx, cancel := context.WithCancel(context.Background())
	return &CarpoolResetMonitorService{repo: repo, quota: quota, invalidate: invalidate, now: time.Now, ctx: ctx, cancel: cancel}
}

func (s *CarpoolResetMonitorService) Start() {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.stopped || s.cron != nil {
		return
	}
	c := cron.New(cron.WithChain(cron.Recover(cron.DefaultLogger)))
	job := cron.FuncJob(func() { s.run(s.ctx) })
	if _, err := c.AddJob(carpoolResetMonitorSchedule, job); err != nil {
		logger.LegacyPrintf("service.carpool_reset", "schedule unavailable: %v", err)
		return
	}
	s.cron = c
	s.seedDone = make(chan struct{})
	go func() {
		defer close(s.seedDone)
		cron.Recover(cron.DefaultLogger)(job).Run()
	}()
	c.Start()
}

func (s *CarpoolResetMonitorService) Stop() {
	s.lifecycle.Lock()
	s.stopped = true
	s.cancel()
	var jobsDone <-chan struct{}
	if s.cron != nil {
		jobsDone = s.cron.Stop().Done()
	}
	seedDone := s.seedDone
	s.lifecycle.Unlock()
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	for _, done := range []<-chan struct{}{jobsDone, seedDone} {
		if done != nil {
			select {
			case <-done:
			case <-timer.C:
				return
			}
		}
	}
}

func (s *CarpoolResetMonitorService) run(parent context.Context) {
	if !s.runMu.TryLock() {
		return
	}
	defer s.runMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	if ctx.Err() != nil {
		return
	}
	s.drainInvalidations(ctx)
	source, err := s.repo.Source(ctx)
	if err != nil {
		logger.LegacyPrintf("service.carpool_reset", "resolve source failed: %v", err)
		return
	}
	if source == nil {
		if err := s.repo.ClearBaseline(ctx); err != nil {
			logger.LegacyPrintf("service.carpool_reset", "clear baseline failed: %v", err)
		}
		return
	}
	readCtx, readDone := context.WithTimeout(ctx, 30*time.Second)
	usage, err := s.quota.QueryUsageReadOnly(readCtx, source.AccountID)
	readDone()
	if err != nil {
		logger.LegacyPrintf("service.carpool_reset", "account=%d usage query failed: %v", source.AccountID, err)
	} else if observation, valid := ReadCarpoolResetObservation(usage, source.UpstreamAccountID, s.now()); valid {
		event, err := s.repo.Observe(ctx, *source, observation)
		if err != nil {
			logger.LegacyPrintf("service.carpool_reset", "account=%d observation failed: %v", source.AccountID, err)
		} else if event != nil {
			logger.LegacyPrintf("service.carpool_reset", "event=%d account=%d detected reason=%s", event.ID, source.AccountID, event.Reason)
		}
	}
	// Confirmed events survive a transient upstream failure; ApplyEvent rechecks
	// the current source and expiry before touching any subscription.
	ids, err := s.repo.PendingEvents(ctx)
	if err != nil {
		logger.LegacyPrintf("service.carpool_reset", "pending events failed: %v", err)
		return
	}
	applied := false
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		now := s.now()
		event, err := s.repo.ApplyEvent(ctx, id, now, timezone.StartOfDay(now))
		if err != nil {
			logger.LegacyPrintf("service.carpool_reset", "event=%d apply failed: %v", id, err)
			continue
		}
		if event != nil {
			applied = applied || event.Status == "completed"
			logger.LegacyPrintf("service.carpool_reset", "event=%d status=%s reset=%d skipped=%d", id, event.Status, event.ResetCount, event.SkippedCount)
		}
	}
	if applied {
		s.drainInvalidations(ctx)
	}
}

func (s *CarpoolResetMonitorService) drainInvalidations(ctx context.Context) {
	if s.invalidate == nil {
		return
	}
	// Leave time for usage observation even when a cache outage creates a backlog.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	const batchSize = 100
	for ctx.Err() == nil {
		items, err := s.repo.PendingInvalidations(ctx, batchSize)
		if err != nil {
			logger.LegacyPrintf("service.carpool_reset", "pending cache invalidations failed: %v", err)
			return
		}
		failed := false
		for _, item := range items {
			if ctx.Err() != nil {
				return
			}
			err := s.invalidate(item.UserID, item.GroupID)
			if err == nil {
				err = s.repo.CompleteInvalidation(ctx, item.EventID, item.SubscriptionID)
			}
			if err != nil {
				failed = true
				logger.LegacyPrintf("service.carpool_reset", "event=%d subscription=%d cache refresh failed: %v", item.EventID, item.SubscriptionID, err)
			}
		}
		if failed || len(items) < batchSize {
			return
		}
	}
}

func ProvideCarpoolResetMonitorService(repo CarpoolResetMonitorRepository, quota *OpenAIQuotaService, subscriptions *SubscriptionService) *CarpoolResetMonitorService {
	svc := NewCarpoolResetMonitorService(repo, quota, subscriptions.invalidateSubscriptionCaches)
	svc.Start()
	return svc
}
