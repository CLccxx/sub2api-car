package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func quotaResetTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := carpoolMonitorTestDB(t)
	migration, err := migrations.FS.ReadFile("241_custom_quota_reset_operations.sql")
	require.NoError(t, err)
	for range 2 {
		_, err := db.Exec(string(migration))
		require.NoError(t, err, "migration must be safe to reapply")
	}
	return carpoolMonitorConcurrentPool(t, db)
}

func quotaResetAssertOperation(t *testing.T, db *sql.DB, accountID int64, requestID, creditID, status string) {
	t.Helper()
	var gotCredit, gotStatus string
	var created time.Time
	var completed sql.NullTime
	var result []byte
	require.NoError(t, db.QueryRow(`SELECT credit_id,status,result,created_at,completed_at
		FROM quota_reset_operations WHERE account_id=$1 AND request_id=$2`, accountID, requestID).
		Scan(&gotCredit, &gotStatus, &result, &created, &completed))
	require.Equal(t, creditID, gotCredit)
	require.Equal(t, status, gotStatus)
	require.False(t, created.IsZero())
	require.Equal(t, status != "pending", completed.Valid)
	if status == "pending" {
		require.Empty(t, result)
	} else {
		require.NotEmpty(t, result)
	}
}

func quotaResetNoExecute(t *testing.T) func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
	t.Helper()
	return func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
		t.Error("unexpected second consumption callback")
		return nil, errors.New("unexpected callback")
	}
}

func TestQuotaResetCoordinatorPostgresReplayAndDurableIntent(t *testing.T) {
	db := quotaResetTestDB(t)
	want := &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 2,
		Credit: &service.OpenAIQuotaResetCredit{ID: "credit-a", RedeemedAt: "2026-10-08T00:00:00Z"}}
	result, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(_ context.Context, creditID, requestID string) (*service.OpenAIQuotaResetResult, error) {
			require.Equal(t, "credit-a", creditID)
			require.Equal(t, "request-a", requestID)
			quotaResetAssertOperation(t, db, 9, requestID, creditID, "pending")
			// The callback may safely perform other DB work: no intent transaction is held.
			var inTransaction int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_stat_activity
				WHERE datname=current_database() AND pid<>pg_backend_pid()
				AND state='idle in transaction' AND query LIKE '%quota_reset_operations%'`).Scan(&inTransaction))
			require.Zero(t, inTransaction)
			return want, nil
		})
	require.NoError(t, err)
	require.Equal(t, want, result)
	quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "succeeded")
	result, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a", quotaResetNoExecute(t))
	require.NoError(t, err)
	require.Equal(t, want, result, "a new coordinator replays the durable result after restart")
	_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-b", "request-a", quotaResetNoExecute(t))
	quotaResetAssertBusy(t, err)
}

func TestQuotaResetCoordinatorPostgresUnknownRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *service.OpenAIQuotaResetResult
		err    error
	}{
		{name: "transport failure", err: context.DeadlineExceeded},
		{name: "nil result"},
		{name: "missing code", result: &service.OpenAIQuotaResetResult{}},
		{name: "ok without windows", result: &service.OpenAIQuotaResetResult{Code: "ok"}},
		{name: "error with result", result: &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, err: errors.New("connection lost")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := quotaResetTestDB(t)
			_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
				func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
					return tc.result, tc.err
				})
			require.Error(t, err)
			quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "pending")
			for _, attempt := range [][2]string{{"credit-b", "request-b"}, {"credit-a", "request-b"}, {"credit-b", "request-a"}, {"", "admin-request"}, {"", "request-a"}} {
				_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, attempt[0], attempt[1], quotaResetNoExecute(t))
				quotaResetAssertBusy(t, err)
			}
			_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
				func(_ context.Context, creditID, requestID string) (*service.OpenAIQuotaResetResult, error) {
					require.Equal(t, "credit-a", creditID)
					require.Equal(t, "request-a", requestID)
					return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
				})
			require.NoError(t, err)
			quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "succeeded")
		})
	}
}

func TestQuotaResetCoordinatorPostgresGenericResumesOnlyGeneric(t *testing.T) {
	db := quotaResetTestDB(t)
	_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "", "admin-original",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			return nil, context.DeadlineExceeded
		})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "proposal-request", quotaResetNoExecute(t))
	quotaResetAssertBusy(t, err)
	_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "", "admin-new",
		func(_ context.Context, creditID, requestID string) (*service.OpenAIQuotaResetResult, error) {
			require.Empty(t, creditID)
			require.Equal(t, "admin-original", requestID)
			return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
		})
	require.NoError(t, err)
	quotaResetAssertOperation(t, db, 9, "admin-original", "", "succeeded")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM quota_reset_operations`).Scan(&count))
	require.Equal(t, 1, count, "resume must not create an intent with the new administrator request identity")
}

func TestQuotaResetCoordinatorPostgresExplicitFailureReplaysAndReleases(t *testing.T) {
	db := quotaResetTestDB(t)
	want := &service.OpenAIQuotaResetResult{Code: "credit_not_available"}
	result, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) { return want, nil })
	require.NoError(t, err)
	require.Equal(t, want, result)
	quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "failed")
	result, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a", quotaResetNoExecute(t))
	require.NoError(t, err)
	require.Equal(t, want, result)
	_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-b", "request-b",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
		})
	require.NoError(t, err)
}

func TestQuotaResetCoordinatorPostgresConcurrentAndFullWidthAccount(t *testing.T) {
	db := quotaResetTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	defer func() {
		close(release)
		require.NoError(t, <-done)
	}()
	go func() {
		_, err := NewQuotaResetCoordinator(db).Run(ctx, 9, "credit-a", "request-a",
			func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
				close(entered)
				select {
				case <-release:
					return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			})
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("callback never entered")
	}
	for _, attempt := range [][2]string{{"credit-a", "request-a"}, {"credit-b", "request-b"}, {"", "admin-request"}} {
		_, err := NewQuotaResetCoordinator(db).Run(ctx, 9, attempt[0], attempt[1], quotaResetNoExecute(t))
		quotaResetAssertBusy(t, err)
	}
	// A 32-bit truncation would alias this account to account 9.
	for _, other := range []int64{10, 9 + 1<<32, 1<<63 - 1} {
		_, err := db.Exec(`INSERT INTO accounts(id,credentials) VALUES($1,'{"chatgpt_account_id":"other"}')`, other)
		require.NoError(t, err)
		_, err = NewQuotaResetCoordinator(db).Run(ctx, other, "credit-other", "request-a",
			func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
				return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
			})
		require.NoError(t, err)
	}
}

func TestQuotaResetCoordinatorPostgresPersistenceFailureRecovery(t *testing.T) {
	db := quotaResetTestDB(t)
	_, err := db.Exec(`CREATE FUNCTION reject_quota_reset_outcome() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'simulated persistence failure'; END; $$;
		CREATE TRIGGER reject_quota_reset_outcome BEFORE UPDATE ON quota_reset_operations
		FOR EACH ROW EXECUTE FUNCTION reject_quota_reset_outcome()`)
	require.NoError(t, err)
	_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
		})
	require.Error(t, err)
	quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "pending")
	_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-b", "request-b", quotaResetNoExecute(t))
	quotaResetAssertBusy(t, err)
	_, err = db.Exec(`DROP TRIGGER reject_quota_reset_outcome ON quota_reset_operations`)
	require.NoError(t, err)
	_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(_ context.Context, creditID, requestID string) (*service.OpenAIQuotaResetResult, error) {
			require.Equal(t, "credit-a", creditID)
			require.Equal(t, "request-a", requestID)
			return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
		})
	require.NoError(t, err)
	quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "succeeded")
}

func TestQuotaResetCoordinatorPostgresStorageTimeoutLeavesRecoverableIntent(t *testing.T) {
	db := quotaResetTestDB(t)
	var blocker *sql.Tx
	defer func() {
		if blocker != nil {
			_ = blocker.Rollback()
		}
	}()
	_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			var err error
			blocker, err = db.Begin()
			require.NoError(t, err)
			_, err = blocker.Exec(`SELECT 1 FROM quota_reset_operations WHERE account_id=9 FOR UPDATE`)
			require.NoError(t, err)
			return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
		})
	require.Error(t, err)
	require.NoError(t, blocker.Rollback())
	quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "pending")
	_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-b", "request-b", quotaResetNoExecute(t))
	quotaResetAssertBusy(t, err)
	_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
		})
	require.NoError(t, err)
	quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "succeeded")
}

func TestQuotaResetCoordinatorPostgresCancellationAndPanic(t *testing.T) {
	db := quotaResetTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := NewQuotaResetCoordinator(db).Run(ctx, 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			cancel()
			return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
		})
	require.NoError(t, err)
	quotaResetAssertOperation(t, db, 9, "request-a", "credit-a", "succeeded")
	require.Panics(t, func() {
		_, _ = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-b", "request-b",
			func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
				panic("callback panic")
			})
	})
	quotaResetAssertOperation(t, db, 9, "request-b", "credit-b", "pending")
	_, err = NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-b", "request-b",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
		})
	require.NoError(t, err, "panic cleanup must release the session advisory lock")
}

func TestQuotaResetCoordinatorPostgresMigrationConstraints(t *testing.T) {
	db := quotaResetTestDB(t)
	_, err := db.Exec(`INSERT INTO quota_reset_operations(account_id,request_id,credit_id) VALUES(9,'request-a','credit-a')`)
	require.NoError(t, err)
	for i, statement := range []string{
		`INSERT INTO quota_reset_operations(account_id,request_id,credit_id) VALUES(9,'request-b','credit-b')`,
		`INSERT INTO quota_reset_operations(account_id,request_id,credit_id,status,result,completed_at) VALUES(9,'request-a','credit-a','failed','{"code":"denied"}',now())`,
		`INSERT INTO quota_reset_operations(account_id,request_id,credit_id,status) VALUES(10,'request-b','credit-b','bogus')`,
	} {
		t.Run(fmt.Sprintf("constraint_%d", i), func(t *testing.T) {
			_, err := db.Exec(statement)
			require.Error(t, err)
		})
	}
}
