package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func quotaResetVoteTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := quotaResetTestDB(t)
	_, err := db.Exec(`CREATE TABLE users(id bigint PRIMARY KEY,status text,deleted_at timestamptz)`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("242_custom_carpool_reset_voting.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO carpool_reset_votes(group_id,account_id,created_by,creation_key,
		eligibility,observation,required_count,credit_id,request_id)
		VALUES(10,9,1,'00000000-0000-4000-8000-000000000001','{}','{}',2,'credit-a','request-a')`)
	require.NoError(t, err)
	return db
}

func TestQuotaResetCoordinatorPostgresDispatchedVoteBlocksUntilLocalCompletion(t *testing.T) {
	for _, status := range []string{"executing", "awaiting_confirmation", "applying"} {
		for _, hasEvent := range []bool{false, true} {
			name := status + "/without_event"
			if hasEvent {
				name = status + "/with_event"
			}
			t.Run(name, func(t *testing.T) {
				db := quotaResetVoteTestDB(t)
				_, err := db.Exec(`UPDATE carpool_reset_votes SET status=$1,dispatched_at=now()`, status)
				require.NoError(t, err)
				if hasEvent {
					_, err = db.Exec(`INSERT INTO carpool_subscription_reset_events(id,event_key,group_id,account_id,
						identity_hash,reason,observed_at,expires_at) VALUES(100,'guard-event',10,9,'identity','vote',now(),now()+interval '1 hour');
						UPDATE carpool_reset_votes SET event_id=100`)
					require.NoError(t, err)
				}
				want := &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}
				_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
					func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) { return want, nil })
				require.NoError(t, err, "the owning vote can consume its authorized credit")
				quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "succeeded")
				result, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a", quotaResetNoExecute(t))
				require.NoError(t, err)
				require.Equal(t, want, result)
				for _, credit := range []string{"", "auto-credit"} {
					_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, credit, "other-request", quotaResetNoExecute(t))
					quotaResetAssertBusy(t, err)
				}
				_, err = db.Exec(`UPDATE carpool_reset_votes SET status='completed'`)
				require.NoError(t, err)
				_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "", "admin-request",
					func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) { return want, nil })
				require.NoError(t, err)
			})
		}
	}
}

func TestQuotaResetCoordinatorPostgresPendingIdentityChangeBlocksConsumption(t *testing.T) {
	for _, creditID := range []string{"", "credit-a"} {
		t.Run("credit="+creditID, func(t *testing.T) {
			db := quotaResetTestDB(t)
			_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, creditID, "request-a",
				func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
					return nil, context.DeadlineExceeded
				})
			require.ErrorIs(t, err, context.DeadlineExceeded)
			var identity string
			require.NoError(t, db.QueryRow(`SELECT identity_hash FROM quota_reset_operations WHERE account_id=9`).Scan(&identity))
			require.Equal(t, service.CarpoolResetIdentityHash("shared"), identity)
			_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"replacement"}' WHERE id=9`)
			require.NoError(t, err)
			_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, creditID, "request-a", quotaResetNoExecute(t))
			quotaResetAssertBusy(t, err)
			if creditID == "" {
				_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "", "new-admin-request", quotaResetNoExecute(t))
				quotaResetAssertBusy(t, err)
			}
			quotaResetAssertOperation(t, db, 9, "request-a", creditID, "pending")
			_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"shared","access_token":"refreshed"}' WHERE id=9`)
			require.NoError(t, err)
			_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, creditID, "request-a",
				func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
					return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
				})
			require.NoError(t, err, "token refresh without upstream identity change is safe")
		})
	}
}

func TestQuotaResetCoordinatorPostgresLegacyOrganizationIdentity(t *testing.T) {
	db := quotaResetTestDB(t)
	_, err := db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"  ","organization_id":"legacy-shared"}' WHERE id=9`)
	require.NoError(t, err)
	called := false
	_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "", "legacy-request",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			called = true
			return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
		})
	require.NoError(t, err)
	require.True(t, called)
}

func TestQuotaResetCoordinatorPostgresReceiptRecoversWithoutRedispatch(t *testing.T) {
	db := quotaResetTestDB(t)
	ctx := context.Background()
	_, err := NewQuotaResetCoordinator(db).Run(ctx, 9, "card", "request", func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
		return nil, context.DeadlineExceeded
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, NewCarpoolResetMonitorRepository(db).RecordResetReceipt(ctx, 9, service.CarpoolResetIdentityHash("shared"), service.CarpoolResetIdentityHash("request"), time.Now(), 1))
	_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"changed"}' WHERE id=9`)
	require.NoError(t, err)
	result, err := NewQuotaResetCoordinator(db).Run(ctx, 9, "card", "request", quotaResetNoExecute(t))
	require.NoError(t, err)
	require.Equal(t, "ok", result.Code)
	require.Equal(t, 1, result.WindowsReset)
}

func TestQuotaResetCoordinatorPostgresAdminReceiptInvalidatesVoteInsideGuard(t *testing.T) {
	db := quotaResetVoteTestDB(t)
	_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "", "admin-request",
		func(ctx context.Context, creditID, requestID string) (*service.OpenAIQuotaResetResult, error) {
			require.Empty(t, creditID)
			_, conflict := NewQuotaResetCoordinator(db).Run(ctx, 9, "credit-a", "request-a", quotaResetNoExecute(t))
			quotaResetAssertBusy(t, conflict)
			err := NewCarpoolResetMonitorRepository(db).RecordResetReceipt(ctx, 9,
				service.CarpoolResetIdentityHash("shared"), service.CarpoolResetIdentityHash(requestID), time.Now(), 1)
			require.NoError(t, err)
			var status, reason string
			require.NoError(t, db.QueryRow(`SELECT status,reason FROM carpool_reset_votes`).Scan(&status, &reason))
			require.Equal(t, "invalidated", status, "invalidation must already be durable while the coordinator still owns the lock")
			require.Equal(t, "admin_reset", reason)
			return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
		})
	require.NoError(t, err)
	quotaResetAssertOperation(t, db, 9, "admin-request", "", "succeeded")
}

func TestQuotaResetCoordinatorPostgresAdminReceiptInvalidationIsAtomic(t *testing.T) {
	db := quotaResetVoteTestDB(t)
	_, err := db.Exec(`CREATE FUNCTION reject_vote_invalidation() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'simulated vote write failure'; END; $$;
		CREATE TRIGGER reject_vote_invalidation BEFORE UPDATE ON carpool_reset_votes
		FOR EACH ROW EXECUTE FUNCTION reject_vote_invalidation()`)
	require.NoError(t, err)
	err = NewCarpoolResetMonitorRepository(db).RecordResetReceipt(context.Background(), 9,
		service.CarpoolResetIdentityHash("shared"), service.CarpoolResetIdentityHash("admin-request"), time.Now(), 1)
	require.Error(t, err)
	var receipts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM carpool_account_reset_receipts`).Scan(&receipts))
	require.Zero(t, receipts, "a receipt must not commit separately from its vote invalidation")
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM carpool_reset_votes`).Scan(&status))
	require.Equal(t, "voting", status)
}

func TestQuotaResetCoordinatorPostgresReplayFailureCannotReleaseUnknownAttempt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		creditID    string
		code        string
		lostSuccess bool
	}{
		{name: "generic timeout then no credit", code: "no_credit"},
		{name: "targeted timeout then already used", creditID: "credit-a", code: "already_used"},
		{name: "generic success persistence failure", code: "no_credit", lostSuccess: true},
		{name: "targeted success persistence failure", creditID: "credit-a", code: "already_used", lostSuccess: true},
		{name: "unproven vote invalidation", creditID: "credit-a", code: "vote_invalidated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := quotaResetTestDB(t)
			if tc.lostSuccess {
				_, err := db.Exec(`CREATE FUNCTION reject_quota_reset_status() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN RAISE EXCEPTION 'simulated outcome write failure'; END; $$;
					CREATE TRIGGER reject_quota_reset_status BEFORE UPDATE OF status ON quota_reset_operations
					FOR EACH ROW EXECUTE FUNCTION reject_quota_reset_status()`)
				require.NoError(t, err)
			}
			_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, tc.creditID, "request-a",
				func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
					var count int
					require.NoError(t, db.QueryRow(`SELECT dispatch_count FROM quota_reset_operations WHERE account_id=9`).Scan(&count))
					require.Equal(t, 1, count, "first dispatch must already be durable")
					if tc.lostSuccess {
						return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
					}
					return nil, context.DeadlineExceeded
				})
			require.Error(t, err)
			if tc.lostSuccess {
				_, err = db.Exec(`DROP TRIGGER reject_quota_reset_status ON quota_reset_operations`)
				require.NoError(t, err)
			}
			retryID := "request-a"
			if tc.creditID == "" {
				retryID = "new-admin-request"
			}
			_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, tc.creditID, retryID,
				func(_ context.Context, creditID, requestID string) (*service.OpenAIQuotaResetResult, error) {
					require.Equal(t, tc.creditID, creditID)
					require.Equal(t, "request-a", requestID)
					var count int
					require.NoError(t, db.QueryRow(`SELECT dispatch_count FROM quota_reset_operations WHERE account_id=9`).Scan(&count))
					require.Equal(t, 2, count, "retry dispatch must already be committed")
					return &service.OpenAIQuotaResetResult{Code: tc.code}, nil
				})
			quotaResetAssertBusy(t, err)
			quotaResetAssertOperation(t, db, 9, "request-a", tc.creditID, "pending")
			_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "another-credit", "another-request", quotaResetNoExecute(t))
			quotaResetAssertBusy(t, err)
			_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, tc.creditID, retryID,
				func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
					return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
				})
			require.NoError(t, err)
			quotaResetAssertOperation(t, db, 9, "request-a", tc.creditID, "succeeded")
		})
	}
}

func TestQuotaResetCoordinatorPostgresVoteInvalidationRequiresNoDispatchProof(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		t.Run(map[bool]string{false: "never dispatched", true: "previously dispatched"}[dispatched], func(t *testing.T) {
			db := quotaResetVoteTestDB(t)
			_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
				func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
					return nil, context.DeadlineExceeded
				})
			require.ErrorIs(t, err, context.DeadlineExceeded)
			_, err = db.Exec(`UPDATE carpool_reset_votes SET status='invalidated',
				dispatched_at=CASE WHEN $1 THEN now() ELSE NULL END`, dispatched)
			require.NoError(t, err)
			result, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
				func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
					return &service.OpenAIQuotaResetResult{Code: "vote_invalidated"}, nil
				})
			if dispatched {
				quotaResetAssertBusy(t, err)
				quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "pending")
			} else {
				require.NoError(t, err)
				require.Equal(t, "vote_invalidated", result.Code)
				quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "failed")
			}
		})
	}
}
