package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestQuotaResetCoordinatorPostgresNewRequestRecoversConfirmedOperation(t *testing.T) {
	for _, creditID := range []string{"", "credit-a", "credit-b"} {
		t.Run("credit="+creditID, func(t *testing.T) {
			db := quotaResetTestDB(t)
			ctx := context.Background()
			coordinator := NewQuotaResetCoordinator(db)
			_, err := coordinator.Run(ctx, 9, "credit-a", "request-a", func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
				return nil, context.DeadlineExceeded
			})
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NoError(t, NewCarpoolResetMonitorRepository(db).RecordResetReceipt(ctx, 9,
				service.CarpoolResetIdentityHash("shared"), service.CarpoolResetIdentityHash("request-a"), time.Now(), 1))

			// A later automatic cycle or administrator does not know the old identity.
			// Recover its receipt, but never report that old success as request-b's reset.
			result, err := coordinator.Run(ctx, 9, creditID, "request-b", quotaResetNoExecute(t))
			quotaResetAssertBusy(t, err)
			require.Nil(t, result)
			quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "succeeded")
			var count int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM quota_reset_operations WHERE request_id='request-b'`).Scan(&count))
			require.Zero(t, count, "receipt reconciliation must not create or dispatch another operation")

			// A separately retried request can proceed after the durable repair.
			called := false
			_, err = coordinator.Run(ctx, 9, creditID, "request-b", func(_ context.Context, credit, request string) (*service.OpenAIQuotaResetResult, error) {
				called = true
				require.Equal(t, creditID, credit)
				require.Equal(t, "request-b", request)
				return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
			})
			require.NoError(t, err)
			require.True(t, called)
		})
	}
}

func TestCarpoolVotePendingReconcilesConfirmedResetsWithoutProposals(t *testing.T) {
	for _, receipt := range []struct {
		name, identity, request, want string
	}{
		{"matching", "shared", "request-a", "succeeded"},
		{"wrong identity", "other", "request-a", "pending"},
		{"wrong request", "shared", "other", "pending"},
	} {
		t.Run(receipt.name, func(t *testing.T) {
			db := carpoolMonitorConcurrentPool(t, carpoolVoteTestDB(t))
			ctx := context.Background()
			_, err := NewQuotaResetCoordinator(db).Run(ctx, 9, "credit-a", "request-a", func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
				return nil, context.DeadlineExceeded
			})
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NoError(t, NewCarpoolResetMonitorRepository(db).RecordResetReceipt(ctx, 9,
				service.CarpoolResetIdentityHash(receipt.identity), service.CarpoolResetIdentityHash(receipt.request), time.Now(), 1))
			_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"replacement"}' WHERE id=9`)
			require.NoError(t, err)

			ids, err := NewCarpoolResetVoteRepository(db).Pending(ctx)
			require.NoError(t, err)
			require.Empty(t, ids)
			quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", receipt.want)
		})
	}
}

func TestCarpoolVotePendingDoesNotReconcileWhileConsumeIsLocked(t *testing.T) {
	db := carpoolMonitorConcurrentPool(t, carpoolVoteTestDB(t))
	ctx := context.Background()
	_, err := NewQuotaResetCoordinator(db).Run(ctx, 9, "credit-a", "request-a", func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
		return nil, context.DeadlineExceeded
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, NewCarpoolResetMonitorRepository(db).RecordResetReceipt(ctx, 9,
		service.CarpoolResetIdentityHash("shared"), service.CarpoolResetIdentityHash("request-a"), time.Now(), 1))
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended('9',824602))`)
	require.NoError(t, err)
	defer conn.ExecContext(ctx, `SELECT pg_advisory_unlock_all()`)
	repo := NewCarpoolResetVoteRepository(db)
	_, err = repo.Pending(ctx)
	require.NoError(t, err)
	quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "pending")
	_, err = conn.ExecContext(ctx, `SELECT pg_advisory_unlock_all()`)
	require.NoError(t, err)
	_, err = repo.Pending(ctx)
	require.NoError(t, err)
	quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "succeeded")
}
