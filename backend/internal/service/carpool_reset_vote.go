package service

import (
	"context"
	"errors"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/google/uuid"
)

type CarpoolResetVoteService struct {
	repo           CarpoolResetVoteRepository
	quota          sharedUsageQuotaReader
	coordinator    QuotaResetCoordinator
	monitor        CarpoolResetMonitorRepository
	monitorService *CarpoolResetMonitorService
	shared         *SharedSubscriptionUsageService
	consume        func(context.Context, int64, string, string) (*OpenAIQuotaResetResult, error)
	postReset      func(context.Context, int64) bool
	ctx            context.Context
	cancel         context.CancelFunc
	wake           chan struct{}
	done           chan struct{}
	start          sync.Once
}

func ProvideCarpoolResetVoteService(repo CarpoolResetVoteRepository, quota *OpenAIQuotaService, coordinator QuotaResetCoordinator,
	monitor CarpoolResetMonitorRepository, monitorService *CarpoolResetMonitorService, shared *SharedSubscriptionUsageService, recoverer *RateLimitService) *CarpoolResetVoteService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &CarpoolResetVoteService{repo: repo, quota: quota, coordinator: coordinator, monitor: monitor, monitorService: monitorService, shared: shared,
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}),
		consume: func(ctx context.Context, a int64, card, key string) (*OpenAIQuotaResetResult, error) {
			return quota.resetCreditRaw(ctx, a, card, key, true)
		}}
	s.postReset = func(ctx context.Context, id int64) bool {
		result := RunOpenAIQuotaResetPostProcess(ctx, id, voteReadOnlyPostReset{quota}, recoverer, nil)
		return result.AccountStateRecovered && result.CacheRefreshed
	}
	s.Start()
	return s
}

type voteReadOnlyPostReset struct{ *OpenAIQuotaService }

func (q voteReadOnlyPostReset) QueryUsage(ctx context.Context, id int64) (*OpenAIQuotaUsage, error) {
	return q.QueryUsageReadOnly(ctx, id)
}

func (s *CarpoolResetVoteService) Get(ctx context.Context, user, subscription int64) (*ResetVoteView, error) {
	v, err := s.repo.View(ctx, user, subscription)
	return v, safeResetVoteError(err)
}

func (s *CarpoolResetVoteService) GetRequest(ctx context.Context, user, subscription int64, key string) (*ResetVoteView, error) {
	v, err := s.repo.Creation(ctx, user, subscription, key)
	if err != nil {
		return nil, safeResetVoteError(err)
	}
	if v != nil {
		v.RequestResolved = true
		return v, nil
	}
	return s.Get(ctx, user, subscription)
}

func (s *CarpoolResetVoteService) Create(ctx context.Context, user, subscription int64, key string) (*ResetVoteView, error) {
	if _, err := uuid.Parse(key); err != nil {
		return nil, infraerrors.BadRequest("RESET_VOTE_INVALID_REQUEST", "Invalid request identifier")
	}
	if replay, err := s.repo.Creation(ctx, user, subscription, key); err != nil || replay != nil {
		return replay, safeResetVoteError(err)
	}
	view, err := s.repo.View(ctx, user, subscription)
	if err != nil {
		return nil, safeResetVoteError(err)
	}
	if !view.CanCreate {
		// Bind concurrent creation retries to the existing proposal as well.
		v, err := s.repo.Create(ctx, user, subscription, key, CarpoolVoteEligibility{}, CarpoolResetObservation{})
		return v, safeResetVoteError(err)
	}
	e, err := s.repo.Eligibility(ctx, user, subscription)
	if err != nil {
		return nil, safeResetVoteError(err)
	}
	if e.Source == nil || e.BlockedReason != "" {
		return s.Get(ctx, user, subscription)
	}
	u, err := s.quota.QueryUsageReadOnly(ctx, e.Source.AccountID)
	if err != nil {
		return blockedResetVote(view, "usage_unavailable"), nil
	}
	o, valid := ReadCarpoolResetObservation(u, e.Source.UpstreamAccountID, time.Now())
	if !valid || !o.ResetAt.After(time.Now().Add(time.Minute)) {
		return blockedResetVote(view, "usage_unavailable"), nil
	}
	if _, err = voteResetCandidate(u, "", time.Now()); err != nil {
		return blockedResetVote(view, "no_credit"), nil
	}
	if _, err = s.monitor.Observe(ctx, *e.Source, o); err != nil {
		return nil, safeResetVoteError(err)
	}
	v, err := s.repo.Create(ctx, user, subscription, key, *e, o)
	return v, safeResetVoteError(err)
}

func (s *CarpoolResetVoteService) Agree(ctx context.Context, user, subscription, proposal int64) (*ResetVoteView, error) {
	v, err := s.repo.Agree(ctx, user, subscription, proposal)
	if err == nil && v != nil && v.Proposal != nil && v.Proposal.Status == "executing" {
		s.signal()
	}
	return v, safeResetVoteError(err)
}

func safeResetVoteError(err error) error {
	if err == nil || errors.Is(err, ErrSubscriptionNotFound) {
		return err
	}
	return infraerrors.New(503, "RESET_VOTE_UNAVAILABLE", "Reset voting is temporarily unavailable; refresh and try again")
}
func blockedResetVote(v *ResetVoteView, reason string) *ResetVoteView {
	v.CanCreate = false
	v.BlockedReason = &reason
	return v
}

func voteResetCandidate(u *OpenAIQuotaUsage, original string, now time.Time) (string, error) {
	if u == nil || u.RateLimitResetCredits == nil || u.RateLimitResetCredits.availableCountMissing {
		return "", errors.New("credit availability unknown")
	}
	if original != "" {
		for _, c := range u.autoResetCandidates {
			expiry, err := time.Parse(time.RFC3339, c.ExpiresAt)
			if c.ID == original && err == nil && expiry.After(now.Add(time.Minute)) {
				return original, nil
			}
		}
		return "", errors.New("original credit unavailable")
	}
	c, err := selectOpenAIAutoResetCandidate(u.autoResetCandidates, u.RateLimitResetCredits.AvailableCount, nil, "")
	if err != nil {
		return "", err
	}
	expiry, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil || !expiry.After(now.Add(time.Minute)) {
		return "", errors.New("credit expiration too close")
	}
	return c.ID, nil
}

func (s *CarpoolResetVoteService) signal() {
	if s.wake != nil {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}
func (s *CarpoolResetVoteService) Start() {
	s.start.Do(func() {
		go func() {
			defer close(s.done)
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				s.run()
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
				case <-s.wake:
				}
			}
		}()
	})
}
func (s *CarpoolResetVoteService) Stop() {
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(6 * time.Second):
	}
}

func (s *CarpoolResetVoteService) run() {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	ids, err := s.repo.Pending(ctx)
	if err != nil {
		logger.LegacyPrintf("service.carpool_vote", "pending scan failed: %v", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.process(ctx, id)
	}
}

func (s *CarpoolResetVoteService) process(ctx context.Context, id int64) {
	p, err := s.repo.Load(ctx, id)
	if err != nil || p == nil {
		return
	}
	if p.Status == "applying" {
		s.apply(ctx, p)
		return
	}
	if p.Status != "executing" && p.Status != "awaiting_confirmation" {
		return
	}
	if !p.Dispatched {
		u, err := s.quota.QueryUsageReadOnly(ctx, p.Eligibility.Source.AccountID)
		if err != nil {
			return
		}
		o, valid := ReadCarpoolResetObservation(u, p.Eligibility.Source.UpstreamAccountID, time.Now())
		if !valid {
			return
		}
		card, err := voteResetCandidate(u, p.CreditID, time.Now())
		if err != nil {
			_ = s.repo.Fail(ctx, id, "no_credit")
			return
		}
		key := p.RequestID
		if key == "" {
			key = uuid.NewString()
		}
		ok, err := s.repo.Prepare(ctx, id, o, card, key)
		if err != nil || !ok {
			return
		}
		p, err = s.repo.Load(ctx, id)
		if err != nil || p == nil {
			return
		}
	}
	result, err := s.coordinator.Run(ctx, p.Eligibility.Source.AccountID, p.CreditID, p.RequestID,
		func(callCtx context.Context, card, key string) (*OpenAIQuotaResetResult, error) {
			ok, err := s.repo.Dispatch(callCtx, id)
			if err != nil {
				return nil, err
			}
			if !ok {
				return &OpenAIQuotaResetResult{Code: "vote_invalidated"}, nil
			}
			return s.consume(callCtx, p.Eligibility.Source.AccountID, card, key)
		})
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err != nil || result == nil {
		_ = s.repo.AwaitConfirmation(writeCtx, id)
		return
	}
	if result.Code != "ok" || result.WindowsReset <= 0 {
		if result.Code != "vote_invalidated" {
			_ = s.repo.Fail(writeCtx, id, "reset_failed")
		}
		return
	}
	event, err := s.repo.Succeed(writeCtx, id, result)
	if err != nil {
		logger.LegacyPrintf("service.carpool_vote", "proposal=%d local confirmation pending: %v", id, err)
		return
	}
	p.EventID = event
	applyCtx, applyDone := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer applyDone()
	s.apply(applyCtx, p)
}

func (s *CarpoolResetVoteService) apply(ctx context.Context, p *CarpoolResetVote) {
	if s.monitor == nil {
		return
	}
	now := time.Now()
	event, err := s.monitor.ApplyEvent(ctx, p.EventID, now, timezone.StartOfDay(now))
	if err != nil || event == nil || event.Status != "completed" {
		return
	}
	if s.monitorService != nil {
		s.monitorService.drainInvalidations(ctx)
	}
	if s.postReset != nil && !s.postReset(ctx, p.Eligibility.Source.AccountID) {
		return
	}
	if s.shared != nil {
		s.shared.Invalidate(p.Eligibility.Source.AccountID)
	}
	if err = s.repo.Complete(ctx, p.ID); err != nil {
		logger.LegacyPrintf("service.carpool_vote", "proposal=%d completion pending: %v", p.ID, err)
	}
}
