package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func carpoolVoteTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := carpoolMonitorTestDB(t)
	_, err := db.Exec(`CREATE TABLE users(id bigint PRIMARY KEY,status text DEFAULT 'active',deleted_at timestamptz);
		INSERT INTO users(id) VALUES(1),(2),(3),(4),(5);
		UPDATE user_subscriptions SET status='active',expires_at=now()+interval '2 days',group_id=10;
		INSERT INTO user_subscriptions(id,user_id,group_id,status) VALUES(5,5,20,'active')`)
	require.NoError(t, err)
	for _, file := range []string{"241_custom_quota_reset_operations.sql", "242_custom_carpool_reset_voting.sql"} {
		migration, err := migrations.FS.ReadFile(file)
		require.NoError(t, err)
		_, err = db.Exec(string(migration))
		require.NoError(t, err)
	}
	return db
}

func createTestVote(t *testing.T, db *sql.DB) (service.CarpoolResetVoteRepository, *service.ResetVoteView) {
	t.Helper()
	r := NewCarpoolResetVoteRepository(db)
	ctx := context.Background()
	eligibility, err := r.Eligibility(ctx, 1, 1)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	view, err := r.Create(ctx, 1, 1, "00000000-0000-4000-8000-000000000001", *eligibility,
		service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(24 * time.Hour), Utilization: 20})
	require.NoError(t, err)
	require.NotNil(t, view.Proposal)
	return r, view
}

func TestCarpoolVotePostgresQuorumAndIdempotency(t *testing.T) {
	db := carpoolVoteTestDB(t)
	r, view := createTestVote(t, db)
	ctx := context.Background()
	require.Equal(t, 1, view.Proposal.AgreedCount)
	require.True(t, view.Proposal.MyVote)
	for _, user := range []int64{2, 2, 3} {
		view, err := r.Agree(ctx, user, user, view.Proposal.ID)
		require.NoError(t, err)
		require.Equal(t, "voting", view.Proposal.Status)
	}
	view, err := r.Agree(ctx, 4, 4, view.Proposal.ID)
	require.NoError(t, err)
	require.Equal(t, "executing", view.Proposal.Status)
	require.Equal(t, 4, view.Proposal.AgreedCount)
	_, err = r.Agree(ctx, 5, 5, view.Proposal.ID)
	require.Error(t, err, "another group cannot approve")
	_, err = r.View(ctx, 2, 1)
	require.ErrorIs(t, err, service.ErrSubscriptionNotFound)
	_, repeated := createTestVote(t, db)
	require.Equal(t, view.Proposal.ID, repeated.Proposal.ID)
}

func TestCarpoolVotePostgresInvalidation(t *testing.T) {
	for _, change := range []string{
		`UPDATE user_subscriptions SET expires_at=now()-interval '1 second' WHERE id=4`,
		`UPDATE groups SET carpool_members=3 WHERE id=10; UPDATE groups SET carpool_members=4 WHERE id=10`,
		`UPDATE user_subscriptions SET status='suspended' WHERE id=4; UPDATE user_subscriptions SET status='active' WHERE id=4`,
		`UPDATE users SET status='disabled' WHERE id=4; UPDATE users SET status='active' WHERE id=4`,
		`UPDATE carpool_reset_votes SET expires_at=now()-interval '1 second'`,
		`INSERT INTO carpool_account_reset_receipts(account_id,identity_hash,request_hash,occurred_at,windows_reset) VALUES(9,'identity','admin',now(),1)`,
	} {
		t.Run(change, func(t *testing.T) {
			db := carpoolVoteTestDB(t)
			r, _ := createTestVote(t, db)
			_, err := db.Exec(change)
			require.NoError(t, err)
			view, err := r.View(context.Background(), 1, 1)
			require.NoError(t, err)
			require.Contains(t, []string{"invalidated", "expired"}, view.Proposal.Status)
			require.False(t, view.Proposal.CanAgree)
		})
	}
}

func TestCarpoolVotePostgresExactMembership(t *testing.T) {
	db := carpoolVoteTestDB(t)
	_, err := db.Exec(`UPDATE user_subscriptions SET group_id=10 WHERE id=5`)
	require.NoError(t, err)
	r := NewCarpoolResetVoteRepository(db)
	view, err := r.View(context.Background(), 1, 1)
	require.NoError(t, err)
	require.Equal(t, 5, view.MemberCount)
	require.Equal(t, "member_count_mismatch", *view.BlockedReason)
	require.False(t, view.CanCreate)
}

func TestCarpoolVotePostgresConfiguredSizes(t *testing.T) {
	for _, size := range []int{2, 3, 5} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			db := carpoolVoteTestDB(t)
			_, err := db.Exec(`UPDATE groups SET carpool_members=$1 WHERE id=10`, size)
			require.NoError(t, err)
			_, err = db.Exec(`UPDATE user_subscriptions SET group_id=CASE WHEN user_id<=$1 THEN 10 ELSE 20 END`, size)
			require.NoError(t, err)
			r, v := createTestVote(t, db)
			for u := int64(2); u <= int64(size); u++ {
				v, err = r.Agree(context.Background(), u, u, v.Proposal.ID)
				require.NoError(t, err)
				if u < int64(size) {
					require.Equal(t, "voting", v.Proposal.Status)
				}
			}
			require.Equal(t, "executing", v.Proposal.Status)
			require.Equal(t, size, v.Proposal.AgreedCount)
		})
	}
}

func TestCarpoolVotePostgresMonitorAndLocalRecovery(t *testing.T) {
	db := carpoolVoteTestDB(t)
	r, view := createTestVote(t, db)
	ctx := context.Background()
	for _, u := range []int64{2, 3, 4} {
		_, err := r.Agree(ctx, u, u, view.Proposal.ID)
		require.NoError(t, err)
	}
	p, err := r.Load(ctx, view.Proposal.ID)
	require.NoError(t, err)
	monitor := NewCarpoolResetMonitorRepository(db)
	_, err = monitor.Observe(ctx, *p.Eligibility.Source, p.Observation)
	require.NoError(t, err)
	ok, err := r.Prepare(ctx, p.ID, p.Observation, "card-one", "stable-request")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = r.Dispatch(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, ok)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	o := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(7 * 24 * time.Hour), Utilization: 1}
	event, err := monitor.Observe(ctx, *p.Eligibility.Source, o)
	require.NoError(t, err)
	require.Nil(t, event, "monitor must not race in-flight consume")
	_, err = db.Exec(`INSERT INTO quota_reset_operations(account_id,request_id,credit_id,status,result,completed_at)
		VALUES(9,'stable-request','card-one','succeeded','{"code":"ok","windows_reset":1}',$1)`, now.Add(10*time.Minute))
	require.NoError(t, err)
	eventID, err := r.Succeed(ctx, p.ID, &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1})
	require.NoError(t, err)
	event, err = monitor.ApplyEvent(ctx, eventID, now, now)
	require.NoError(t, err)
	require.Equal(t, "completed", event.Status)
	require.Equal(t, 5, event.ResetCount)
	_, err = db.Exec(`UPDATE user_subscriptions SET weekly_usage_usd=7 WHERE id=1`)
	require.NoError(t, err)
	_, err = monitor.Observe(ctx, *p.Eligibility.Source, o)
	require.NoError(t, err)
	var events int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM carpool_subscription_reset_events`).Scan(&events))
	require.Equal(t, 1, events)
	_, err = monitor.ApplyEvent(ctx, eventID, now.Add(time.Minute), now)
	require.NoError(t, err)
	var used float64
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=1`).Scan(&used))
	require.Equal(t, 7.0, used)
	items, err := monitor.PendingInvalidations(ctx, 100)
	require.NoError(t, err)
	for _, item := range items {
		require.NoError(t, monitor.CompleteInvalidation(ctx, item.EventID, item.SubscriptionID))
	}
	require.NoError(t, r.Complete(ctx, p.ID))
	view, err = r.View(ctx, 1, 1)
	require.NoError(t, err)
	require.Equal(t, "completed", view.Proposal.Status)
}

func TestCarpoolVotePostgresConcurrentCreationAndFinalApprovals(t *testing.T) {
	db := carpoolVoteTestDB(t)
	pool := carpoolMonitorConcurrentPool(t, db)
	r := NewCarpoolResetVoteRepository(pool)
	ctx := context.Background()
	e, err := r.Eligibility(ctx, 1, 1)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	o := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 50}
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func(i int) {
			_, err := r.Create(ctx, 1, 1, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), *e, o)
			errs <- err
		}(i)
	}
	for i := 0; i < 16; i++ {
		require.NoError(t, <-errs)
	}
	view, err := r.View(ctx, 1, 1)
	require.NoError(t, err)
	var count int
	require.NoError(t, pool.QueryRow(`SELECT count(*) FROM carpool_reset_votes`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, pool.QueryRow(`SELECT count(*) FROM carpool_reset_vote_requests`).Scan(&count))
	require.Equal(t, 16, count)
	for i := 0; i < 15; i++ {
		go func(i int) { u := int64(i%3 + 2); _, err := r.Agree(ctx, u, u, view.Proposal.ID); errs <- err }(i)
	}
	for i := 0; i < 15; i++ {
		require.NoError(t, <-errs)
	}
	view, err = r.View(ctx, 1, 1)
	require.NoError(t, err)
	require.Equal(t, "executing", view.Proposal.Status)
	require.Equal(t, 4, view.Proposal.AgreedCount)
}

func TestCarpoolVotePostgresFreshWindowInvalidatesBeforeDispatch(t *testing.T) {
	db := carpoolVoteTestDB(t)
	r, v := createTestVote(t, db)
	ctx := context.Background()
	for _, u := range []int64{2, 3, 4} {
		_, err := r.Agree(ctx, u, u, v.Proposal.ID)
		require.NoError(t, err)
	}
	p, err := r.Load(ctx, v.Proposal.ID)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	ok, err := r.Prepare(ctx, p.ID, service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(7 * 24 * time.Hour), Utilization: 1}, "card", "request")
	require.NoError(t, err)
	require.False(t, ok)
	v, err = r.View(ctx, 1, 1)
	require.NoError(t, err)
	require.Equal(t, "invalidated", v.Proposal.Status)
	require.Equal(t, "window_reset", *v.Proposal.Reason)
}

func TestCarpoolVotePostgresLaterManualResetIsNotCoalesced(t *testing.T) {
	db := carpoolVoteTestDB(t)
	r, v := createTestVote(t, db)
	ctx := context.Background()
	for _, u := range []int64{2, 3, 4} {
		_, err := r.Agree(ctx, u, u, v.Proposal.ID)
		require.NoError(t, err)
	}
	p, err := r.Load(ctx, v.Proposal.ID)
	require.NoError(t, err)
	monitor := NewCarpoolResetMonitorRepository(db)
	_, err = monitor.Observe(ctx, *p.Eligibility.Source, p.Observation)
	require.NoError(t, err)
	ok, err := r.Prepare(ctx, p.ID, p.Observation, "card", "request")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = r.Dispatch(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, ok)
	now := time.Now().UTC()
	_, err = db.Exec(`INSERT INTO quota_reset_operations(account_id,request_id,credit_id,status,result,completed_at)
		VALUES(9,'request','card','succeeded','{"code":"ok","windows_reset":1}',$1)`, now)
	require.NoError(t, err)
	id, err := r.Succeed(ctx, p.ID, &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1})
	require.NoError(t, err)
	_, err = monitor.ApplyEvent(ctx, id, now, now)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE carpool_subscription_reset_targets SET cache_invalidated_at=now() WHERE event_id=$1`, id)
	require.NoError(t, err)
	require.NoError(t, r.Complete(ctx, p.ID))
	manual := now.Add(20 * time.Second)
	require.NoError(t, monitor.RecordResetReceipt(ctx, 9, p.Eligibility.Source.IdentityHash, "another-admin-request", manual, 1))
	event, err := monitor.Observe(ctx, *p.Eligibility.Source, service.CarpoolResetObservation{ObservedAt: manual.Add(time.Second), ResetAt: manual.Add(7 * 24 * time.Hour), Utilization: 0})
	require.NoError(t, err)
	require.NotNil(t, event, "a later acknowledged admin use owns a separate reset even before the first post-vote observation")
	require.NotEqual(t, id, event.ID)
}

func TestCarpoolVotePostgresDispatchedUncertaintyDoesNotExpire(t *testing.T) {
	db := carpoolVoteTestDB(t)
	r, v := createTestVote(t, db)
	ctx := context.Background()
	for _, u := range []int64{2, 3, 4} {
		_, err := r.Agree(ctx, u, u, v.Proposal.ID)
		require.NoError(t, err)
	}
	p, err := r.Load(ctx, v.Proposal.ID)
	require.NoError(t, err)
	ok, err := r.Prepare(ctx, p.ID, p.Observation, "card", "request")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = r.Dispatch(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, r.AwaitConfirmation(ctx, p.ID))
	require.NoError(t, r.Fail(ctx, p.ID, "no_credit"), "a late preflight cannot erase another worker's in-flight consume")
	_, err = db.Exec(`UPDATE carpool_reset_votes SET expires_at=now()-interval '1 day'`)
	require.NoError(t, err)
	v, err = r.View(ctx, 1, 1)
	require.NoError(t, err)
	require.Equal(t, "awaiting_confirmation", v.Proposal.Status)
	require.False(t, v.CanCreate)
}

func TestCarpoolVotePostgresExpiredBeforeDispatchReleasesIntent(t *testing.T) {
	db := carpoolVoteTestDB(t)
	r, v := createTestVote(t, db)
	ctx := context.Background()
	for _, u := range []int64{2, 3, 4} {
		_, err := r.Agree(ctx, u, u, v.Proposal.ID)
		require.NoError(t, err)
	}
	p, err := r.Load(ctx, v.Proposal.ID)
	require.NoError(t, err)
	ok, err := r.Prepare(ctx, p.ID, p.Observation, "card", "request")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = db.Exec(`INSERT INTO quota_reset_operations(account_id,request_id,credit_id) VALUES(9,'request','card');
		UPDATE carpool_reset_votes SET expires_at=now()-interval '1 second'`)
	require.NoError(t, err)
	v, err = r.View(ctx, 1, 1)
	require.NoError(t, err)
	require.Equal(t, "expired", v.Proposal.Status)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM quota_reset_operations`).Scan(&status))
	require.Equal(t, "failed", status)
	require.True(t, v.CanCreate, "a crash before dispatch must not permanently occupy the account")
	ok, err = r.Dispatch(ctx, p.ID)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestCarpoolVotePostgresAdminReceiptImmediatelyInvalidates(t *testing.T) {
	db := carpoolVoteTestDB(t)
	r, v := createTestVote(t, db)
	ctx := context.Background()
	monitor := NewCarpoolResetMonitorRepository(db)
	require.NoError(t, monitor.RecordResetReceipt(ctx, 9, service.CarpoolResetIdentityHash("shared"), "admin-key", time.Now(), 1))
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM carpool_reset_votes WHERE id=$1`, v.Proposal.ID).Scan(&status))
	require.Equal(t, "invalidated", status)
	v, err := r.View(ctx, 1, 1)
	require.NoError(t, err)
	require.Equal(t, "admin_reset", *v.Proposal.Reason)
}

func TestCarpoolVotePostgresOrdinaryExtensionKeepsVotes(t *testing.T) {
	db := carpoolVoteTestDB(t)
	r, _ := createTestVote(t, db)
	_, err := db.Exec(`UPDATE user_subscriptions SET expires_at=expires_at+interval '30 days' WHERE id=2`)
	require.NoError(t, err)
	v, err := r.View(context.Background(), 1, 1)
	require.NoError(t, err)
	require.Equal(t, "voting", v.Proposal.Status)
}

func TestCarpoolVotePostgresApplyAndPollLockOrder(t *testing.T) {
	db := carpoolVoteTestDB(t)
	pool := carpoolMonitorConcurrentPool(t, db)
	monitor, event, now := carpoolMonitorPendingEvent(t, pool)
	ctx := context.Background()
	blocker, err := pool.Begin()
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.Exec(`SELECT id FROM carpool_reset_monitor WHERE id=1 FOR UPDATE`)
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() { _, err := monitor.ApplyEvent(ctx, event.ID, now, now); result <- err }()
	require.Eventually(t, func() bool {
		var exists bool
		err := pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation='user_subscriptions'::regclass AND mode='RowExclusiveLock' AND granted)`).Scan(&exists)
		return err == nil && exists
	}, time.Second, 10*time.Millisecond, "application reserves write mode before waiting for the monitor")
	_, err = NewCarpoolResetVoteRepository(pool).View(ctx, 1, 1)
	require.Error(t, err, "polling fails fast while an application owns the write reservation")
	require.NoError(t, blocker.Commit())
	require.NoError(t, <-result)
}
