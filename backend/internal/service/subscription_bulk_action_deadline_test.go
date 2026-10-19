package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type delayedLockSubscriptionRepo struct {
	*bulkActionSubscriptionRepo
	afterLock func()
}

func (r *delayedLockSubscriptionRepo) GetByIDForUpdate(ctx context.Context, id int64) (*UserSubscription, error) {
	r.afterLock()
	return r.GetByID(ctx, id)
}

func TestBulkSubscriptionAction_GuardedResetRechecksDeadlineAfterLock(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			now := time.Now()
			deadline := now.Add(time.Minute)
			repo := &delayedLockSubscriptionRepo{
				bulkActionSubscriptionRepo: &bulkActionSubscriptionRepo{
					subscriptions: map[int64]*UserSubscription{
						1: {ID: 1, UserID: 1, GroupID: 10, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour), DailyUsageUSD: 5},
					},
				},
				afterLock: func() { now = deadline.Add(delay) },
			}
			svc := NewSubscriptionService(nil, repo, nil, nil, nil)
			t.Cleanup(svc.Stop)
			svc.now = func() time.Time { return now }

			result, err := svc.BulkSubscriptionAction(context.Background(), &BulkSubscriptionActionInput{
				SubscriptionIDs: []int64{1}, Action: "reset_quota", Daily: true, ActiveOnly: true, ExecuteBefore: &deadline,
			})

			require.NoError(t, err)
			require.Empty(t, repo.mutations, "a lock wait must not extend the reset deadline")
			require.Equal(t, float64(5), repo.subscriptions[1].DailyUsageUSD)
			require.Zero(t, result.SuccessCount)
			require.Equal(t, 1, result.FailedCount)
			require.Equal(t, "reset execution deadline has passed", result.Results[0].Error)
		})
	}
}
