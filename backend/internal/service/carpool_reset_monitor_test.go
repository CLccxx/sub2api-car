package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/require"
)

type resetMonitorRepoStub struct {
	CarpoolResetMonitorRepository
	source        *CarpoolResetSource
	err           error
	observations  int
	clears        int
	applies       int
	pending       []int64
	invalidations []CarpoolResetInvalidation
	acks          int
	applyErr      error
	ackErr        error
}

func (r *resetMonitorRepoStub) Source(context.Context) (*CarpoolResetSource, error) {
	return r.source, r.err
}
func (r *resetMonitorRepoStub) ClearBaseline(context.Context) error { r.clears++; return nil }
func (r *resetMonitorRepoStub) Observe(context.Context, CarpoolResetSource, CarpoolResetObservation) (*CarpoolResetEvent, error) {
	r.observations++
	return nil, nil
}
func (r *resetMonitorRepoStub) PendingEvents(context.Context) ([]int64, error) { return r.pending, nil }
func (r *resetMonitorRepoStub) ApplyEvent(_ context.Context, id int64, _, _ time.Time) (*CarpoolResetEvent, error) {
	r.applies++
	if r.applyErr != nil {
		return nil, r.applyErr
	}
	r.pending = nil
	return &CarpoolResetEvent{ID: id, Status: "completed", ResetCount: 4}, nil
}
func (r *resetMonitorRepoStub) PendingInvalidations(_ context.Context, limit int) ([]CarpoolResetInvalidation, error) {
	if len(r.invalidations) > limit {
		return r.invalidations[:limit], nil
	}
	return r.invalidations, nil
}
func (r *resetMonitorRepoStub) CompleteInvalidation(_ context.Context, eventID, subscriptionID int64) error {
	if r.ackErr != nil {
		return r.ackErr
	}
	r.acks++
	for i, item := range r.invalidations {
		if item.EventID == eventID && item.SubscriptionID == subscriptionID {
			r.invalidations = append(r.invalidations[:i:i], r.invalidations[i+1:]...)
			break
		}
	}
	return nil
}

func TestCarpoolResetMonitorTick(t *testing.T) {
	for _, scenario := range []string{"valid", "no source", "source error", "query failure", "missing data", "pending event", "cache retry"} {
		t.Run(scenario, func(t *testing.T) {
			now := resetMonitorObservation().ObservedAt
			repo := &resetMonitorRepoStub{source: &CarpoolResetSource{GroupID: 10, AccountID: 9, UpstreamAccountID: "shared"}}
			quota := &sharedUsageQuotaStub{payload: &OpenAIQuotaUsage{AccountID: "shared", FetchedAt: now.Unix(), RateLimit: &OpenAIRateLimit{SecondaryWindow: &OpenAIRateLimitWindow{UsedPercent: 20, LimitWindowSeconds: 604800, ResetAt: now.Add(time.Hour).Unix()}}}}
			invalidations := 0
			monitor := NewCarpoolResetMonitorService(repo, quota, func(int64, int64) error {
				invalidations++
				if scenario == "cache retry" {
					return errors.New("cache unavailable")
				}
				return nil
			})
			monitor.now = func() time.Time { return now }
			switch scenario {
			case "no source":
				repo.source = nil
			case "source error":
				repo.err = errors.New("database unavailable")
			case "query failure":
				quota.err = errors.New("upstream unavailable")
			case "missing data":
				quota.payload.RateLimit = nil
			case "pending event":
				repo.pending = []int64{3}
			case "cache retry":
				repo.invalidations = []CarpoolResetInvalidation{{EventID: 1, SubscriptionID: 1, UserID: 1, GroupID: 10}}
			}
			monitor.run(context.Background())
			if scenario == "no source" {
				require.Equal(t, 1, repo.clears)
			} else {
				require.Zero(t, repo.clears)
			}
			if scenario == "valid" || scenario == "pending event" || scenario == "cache retry" {
				require.Equal(t, 1, repo.observations)
			} else {
				require.Zero(t, repo.observations)
			}
			if scenario == "pending event" {
				require.Equal(t, 1, repo.applies)
			} else {
				require.Zero(t, repo.applies)
			}
			if scenario == "cache retry" {
				require.Positive(t, invalidations)
				require.Zero(t, repo.acks)
			}
			if scenario == "no source" || scenario == "source error" {
				require.Zero(t, quota.calls.Load())
			}
		})
	}
}

func TestCarpoolResetMonitorTenMinuteSchedule(t *testing.T) {
	schedule, err := cron.ParseStandard(carpoolResetMonitorSchedule)
	require.NoError(t, err)
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	now := time.Date(2026, 10, 8, 11, 59, 0, 0, loc)
	for _, minute := range []int{0, 10, 20, 30, 40, 50} {
		now = schedule.Next(now).In(loc)
		require.Equal(t, 12, now.Hour())
		require.Equal(t, minute, now.Minute())
	}
}

func TestCarpoolResetMonitorPendingEventSurvivesQueryFailure(t *testing.T) {
	repo := &resetMonitorRepoStub{source: &CarpoolResetSource{GroupID: 10, AccountID: 9}, pending: []int64{1}, applyErr: errors.New("database temporarily unavailable")}
	quota := &sharedUsageQuotaStub{err: errors.New("upstream unavailable")}
	monitor := NewCarpoolResetMonitorService(repo, quota, nil)
	t.Cleanup(monitor.Stop)
	monitor.run(context.Background())
	require.Equal(t, 1, repo.applies)
	require.Zero(t, repo.observations)
	repo.applyErr = nil
	monitor.run(context.Background())
	require.Equal(t, 2, repo.applies)
	require.Empty(t, repo.pending)
	monitor.run(context.Background())
	require.Equal(t, 2, repo.applies)
}

func TestCarpoolResetMonitorCacheOutboxRetriesWithoutReset(t *testing.T) {
	repo := &resetMonitorRepoStub{ackErr: errors.New("database unavailable")}
	for i := int64(1); i <= 205; i++ {
		repo.invalidations = append(repo.invalidations, CarpoolResetInvalidation{EventID: 1, SubscriptionID: i, UserID: i, GroupID: 10})
	}
	calls := 0
	monitor := NewCarpoolResetMonitorService(repo, nil, func(int64, int64) error { calls++; return nil })
	t.Cleanup(monitor.Stop)
	monitor.run(context.Background())
	require.Equal(t, 100, calls)
	require.Zero(t, repo.acks)
	require.Len(t, repo.invalidations, 205)
	repo.ackErr = nil
	monitor.run(context.Background())
	require.Equal(t, 305, calls)
	require.Equal(t, 205, repo.acks)
	require.Empty(t, repo.invalidations)
	require.Zero(t, repo.applies, "cache retry never replays quota resets, even without a source")
}

type blockingResetMonitorQuota struct{ entered chan struct{} }

func (q *blockingResetMonitorQuota) QueryUsageReadOnly(ctx context.Context, _ int64) (*OpenAIQuotaUsage, error) {
	close(q.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCarpoolResetMonitorLifecycleAndOverlap(t *testing.T) {
	repo := &resetMonitorRepoStub{source: &CarpoolResetSource{GroupID: 10, AccountID: 9}}
	quota := &blockingResetMonitorQuota{entered: make(chan struct{})}
	monitor := NewCarpoolResetMonitorService(repo, quota, nil)
	t.Cleanup(monitor.Stop)
	monitor.Start()
	select {
	case <-quota.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not seed observation")
	}
	monitor.Start()
	// This call must be skipped while the startup observation is in flight.
	monitor.run(context.Background())
	monitor.Stop()
	select {
	case <-monitor.seedDone:
	default:
		t.Fatal("Stop did not wait for initial observation")
	}
	monitor.Start()
	require.Zero(t, repo.observations)
	stopped := NewCarpoolResetMonitorService(repo, quota, nil)
	stopped.Stop()
	stopped.Start()
	require.Nil(t, stopped.cron)
}
