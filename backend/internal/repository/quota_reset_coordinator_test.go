package repository

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func quotaResetMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	})
	return db, mock
}

func quotaResetExpectLock(mock sqlmock.Sqlmock, acquired bool) {
	mock.ExpectQuery(`SELECT pg_try_advisory_lock\(hashtextextended\(\$1::text, 824602\)\)`).
		WithArgs("9").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(acquired))
}

func quotaResetExpectUnlock(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT pg_advisory_unlock\(hashtextextended\(\$1::text, 824602\)\)`).
		WithArgs("9").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
}

func quotaResetRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"request_id", "credit_id", "status", "result", "identity_hash", "dispatch_count"})
}

func quotaResetExpectNoVote(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT to_regclass\('carpool_reset_votes'\) IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"installed"}).AddRow(false))
}

func quotaResetExpectIdentity(mock sqlmock.Sqlmock, identity string) {
	mock.ExpectQuery(`SELECT COALESCE\(NULLIF\(btrim\(credentials->>'chatgpt_account_id'\).*organization_id.*FROM accounts`).
		WithArgs(int64(9)).WillReturnRows(sqlmock.NewRows([]string{"identity"}).AddRow(identity))
}

func quotaResetExpectCommittedIntent(mock sqlmock.Sqlmock) {
	quotaResetExpectLock(mock, true)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT request_id, credit_id, status, result, identity_hash, dispatch_count FROM quota_reset_operations`).
		WithArgs(int64(9), "request-a").WillReturnRows(quotaResetRows())
	mock.ExpectQuery(`SELECT request_id, credit_id, status, result, identity_hash, dispatch_count FROM quota_reset_operations`).
		WithArgs(int64(9)).WillReturnRows(quotaResetRows())
	quotaResetExpectNoVote(mock)
	quotaResetExpectIdentity(mock, "shared")
	mock.ExpectExec(`INSERT INTO quota_reset_operations`).
		WithArgs(int64(9), "request-a", "credit-a", service.CarpoolResetIdentityHash("shared")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func quotaResetExpectIntent(mock sqlmock.Sqlmock) {
	quotaResetExpectCommittedIntent(mock)
	quotaResetExpectIdentity(mock, "shared")
}

func quotaResetAssertBusy(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, http.StatusConflict, infraerrors.Code(err))
	require.Equal(t, "OPENAI_QUOTA_RESET_BUSY", infraerrors.Reason(err))
	require.NotContains(t, err.Error(), "credit-a")
	require.NotContains(t, err.Error(), "request-a")
}

func TestQuotaResetCoordinatorRejectsConcurrentCaller(t *testing.T) {
	db, mock := quotaResetMock(t)
	quotaResetExpectLock(mock, false)
	called := false
	result, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			called = true
			return nil, nil
		})
	quotaResetAssertBusy(t, err)
	require.Nil(t, result)
	require.False(t, called)
}

func TestQuotaResetCoordinatorCommitsBeforeExecute(t *testing.T) {
	db, mock := quotaResetMock(t)
	quotaResetExpectIntent(mock)
	want := &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 2}
	result, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(ctx context.Context, creditID, requestID string) (*service.OpenAIQuotaResetResult, error) {
			require.NoError(t, mock.ExpectationsWereMet(), "intent must already be committed")
			require.Equal(t, "credit-a", creditID)
			require.Equal(t, "request-a", requestID)
			require.NoError(t, ctx.Err())
			mock.ExpectExec(`UPDATE quota_reset_operations`).
				WithArgs("succeeded", sqlmock.AnyArg(), int64(9), "request-a", "credit-a").
				WillReturnResult(sqlmock.NewResult(0, 1))
			quotaResetExpectUnlock(mock)
			return want, nil
		})
	require.NoError(t, err)
	require.Equal(t, want, result)
}

func TestQuotaResetCoordinatorUnknownOutcomesStayPending(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *service.OpenAIQuotaResetResult
		err    error
	}{
		{name: "transport error", err: context.DeadlineExceeded},
		{name: "error overrides success", result: &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, err: errors.New("lost response")},
		{name: "nil response"},
		{name: "empty response", result: &service.OpenAIQuotaResetResult{}},
		{name: "blank code", result: &service.OpenAIQuotaResetResult{Code: " "}},
		{name: "unconfirmed reset", result: &service.OpenAIQuotaResetResult{Code: "ok"}},
		{name: "contradictory response", result: &service.OpenAIQuotaResetResult{Code: "denied", WindowsReset: 1}},
		{name: "negative windows", result: &service.OpenAIQuotaResetResult{Code: "denied", WindowsReset: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := quotaResetMock(t)
			quotaResetExpectIntent(mock)
			quotaResetExpectUnlock(mock)
			calls := 0
			_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
				func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
					calls++
					return tc.result, tc.err
				})
			require.Equal(t, 1, calls, "the coordinator never retries the callback itself")
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
			} else {
				require.Error(t, err, "unknown outcomes must not look complete")
			}
		})
	}
}

func TestQuotaResetCoordinatorPersistsAfterCancellation(t *testing.T) {
	db, mock := quotaResetMock(t)
	quotaResetExpectIntent(mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	want := &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}
	result, err := NewQuotaResetCoordinator(db).Run(ctx, 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			cancel()
			mock.ExpectExec(`UPDATE quota_reset_operations`).
				WithArgs("succeeded", sqlmock.AnyArg(), int64(9), "request-a", "credit-a").
				WillReturnResult(sqlmock.NewResult(0, 1))
			quotaResetExpectUnlock(mock)
			return want, nil
		})
	require.NoError(t, err)
	require.Equal(t, want, result)
}

func TestQuotaResetCoordinatorDoesNotExecuteUncommittedIntent(t *testing.T) {
	db, mock := quotaResetMock(t)
	quotaResetExpectLock(mock, true)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT request_id, credit_id, status, result, identity_hash, dispatch_count FROM quota_reset_operations`).
		WithArgs(int64(9), "request-a").WillReturnRows(quotaResetRows())
	mock.ExpectQuery(`SELECT request_id, credit_id, status, result, identity_hash, dispatch_count FROM quota_reset_operations`).
		WithArgs(int64(9)).WillReturnRows(quotaResetRows())
	quotaResetExpectNoVote(mock)
	quotaResetExpectIdentity(mock, "shared")
	mock.ExpectExec(`INSERT INTO quota_reset_operations`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit().WillReturnError(errors.New("commit acknowledgement lost"))
	quotaResetExpectUnlock(mock)
	called := false
	_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			called = true
			return nil, nil
		})
	require.Error(t, err)
	require.False(t, called)
}

func TestQuotaResetCoordinatorPersistenceFailureIsSafe(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "write error", true: "row missing"}[missing], func(t *testing.T) {
			db, mock := quotaResetMock(t)
			quotaResetExpectIntent(mock)
			update := mock.ExpectExec(`UPDATE quota_reset_operations`)
			if missing {
				update.WillReturnResult(sqlmock.NewResult(0, 0))
			} else {
				update.WillReturnError(errors.New("private database detail: credit-a request-a"))
			}
			quotaResetExpectUnlock(mock)
			_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
				func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
					return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
				})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "credit-a")
			require.NotContains(t, err.Error(), "request-a")
		})
	}
}

func TestQuotaResetCoordinatorDiscardsUncertainLockSession(t *testing.T) {
	db, mock := quotaResetMock(t)
	mock.ExpectQuery(`SELECT pg_try_advisory_lock`).
		WillReturnError(errors.New("lock acknowledgement lost"))
	mock.ExpectClose()
	_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			t.Fatal("callback cannot run when lock ownership is unknown")
			return nil, nil
		})
	require.Error(t, err)
	require.Zero(t, db.Stats().OpenConnections, "a possibly locked session must not return to the pool")
}

func TestQuotaResetCoordinatorDiscardsFailedUnlockSession(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unlock error", true: "lock missing"}[missing], func(t *testing.T) {
			db, mock := quotaResetMock(t)
			quotaResetExpectIntent(mock)
			mock.ExpectExec(`UPDATE quota_reset_operations`).WillReturnResult(sqlmock.NewResult(0, 1))
			unlock := mock.ExpectQuery(`SELECT pg_advisory_unlock`)
			if missing {
				unlock.WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(false))
			} else {
				unlock.WillReturnError(errors.New("unlock unavailable"))
			}
			mock.ExpectClose()
			result, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
				func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
					return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
				})
			require.NoError(t, err, "the completed result is already durable")
			require.Equal(t, "ok", result.Code)
			require.Zero(t, db.Stats().OpenConnections)
		})
	}
}

func TestQuotaResetCoordinatorStorageWriteIsBounded(t *testing.T) {
	db, mock := quotaResetMock(t)
	quotaResetExpectIntent(mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mock.ExpectExec(`UPDATE quota_reset_operations`).WillDelayFor(10 * time.Second).
		WillReturnResult(sqlmock.NewResult(0, 1))
	quotaResetExpectUnlock(mock)
	started := time.Now()
	_, err := NewQuotaResetCoordinator(db).Run(ctx, 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			cancel()
			return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}, nil
		})
	require.Error(t, err)
	elapsed := time.Since(started)
	require.GreaterOrEqual(t, elapsed, 4500*time.Millisecond, "caller cancellation must not cancel persistence")
	require.Less(t, elapsed, 8*time.Second, "detached persistence must retain its five-second bound")
}

func TestQuotaResetCoordinatorDatabaseConflictFailsClosed(t *testing.T) {
	db, mock := quotaResetMock(t)
	quotaResetExpectLock(mock, true)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT request_id, credit_id, status, result, identity_hash, dispatch_count FROM quota_reset_operations`).
		WithArgs(int64(9), "request-a").WillReturnRows(quotaResetRows())
	mock.ExpectQuery(`SELECT request_id, credit_id, status, result, identity_hash, dispatch_count FROM quota_reset_operations`).
		WithArgs(int64(9)).WillReturnRows(quotaResetRows())
	quotaResetExpectNoVote(mock)
	quotaResetExpectIdentity(mock, "shared")
	mock.ExpectExec(`INSERT INTO quota_reset_operations`).WillReturnError(&pq.Error{
		Code: "23505", Detail: "private: credit-a request-a",
	})
	mock.ExpectRollback()
	quotaResetExpectUnlock(mock)
	_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			t.Fatal("a conflicting durable intent must prevent execution")
			return nil, nil
		})
	quotaResetAssertBusy(t, err)
}

func TestQuotaResetCoordinatorRejectsCorruptTerminalResult(t *testing.T) {
	for _, payload := range []string{"not json", "null", `{}`, `{"code":"ok","windows_reset":0}`, `{"code":"denied","windows_reset":0}`} {
		t.Run(payload, func(t *testing.T) {
			db, mock := quotaResetMock(t)
			quotaResetExpectLock(mock, true)
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT request_id, credit_id, status, result, identity_hash, dispatch_count FROM quota_reset_operations`).
				WithArgs(int64(9), "request-a").WillReturnRows(quotaResetRows().AddRow("request-a", "credit-a", "succeeded", payload, "", 1))
			quotaResetExpectNoVote(mock)
			mock.ExpectCommit()
			quotaResetExpectUnlock(mock)
			_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
				func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
					t.Fatal("a corrupt terminal result must not trigger another consumption")
					return nil, nil
				})
			require.Error(t, err)
		})
	}
}

func TestQuotaResetCoordinatorRechecksIdentityAfterIntentCommit(t *testing.T) {
	db, mock := quotaResetMock(t)
	quotaResetExpectCommittedIntent(mock)
	quotaResetExpectIdentity(mock, "replacement")
	quotaResetExpectUnlock(mock)
	_, err := NewQuotaResetCoordinator(db).Run(context.Background(), 9, "credit-a", "request-a",
		func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error) {
			t.Fatal("identity changed after intent commit")
			return nil, nil
		})
	quotaResetAssertBusy(t, err)
}
