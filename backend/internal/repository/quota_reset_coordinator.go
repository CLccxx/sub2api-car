package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

const quotaResetStorageTimeout = 5 * time.Second

type quotaResetCoordinator struct {
	db *sql.DB
}

type quotaResetOperation struct {
	requestID     string
	creditID      string
	status        string
	result        []byte
	identityHash  string
	dispatchCount int64
}

// NewQuotaResetCoordinator coordinates all credit consumption through PostgreSQL.
func NewQuotaResetCoordinator(db *sql.DB) service.QuotaResetCoordinator {
	return &quotaResetCoordinator{db: db}
}

// Reconcile only affirmative receipts. The vote worker also runs without active
// proposals, so an automatic reset can recover after a restart or disabled switch.
// No upstream request or new consume operation is issued by this scan.
func reconcileConfirmedQuotaResetOperations(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, quotaResetStorageTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT o.account_id,o.request_id,r.request_hash
		FROM quota_reset_operations o JOIN carpool_account_reset_receipts r
		ON r.account_id=o.account_id AND r.identity_hash=o.identity_hash
		WHERE o.status='pending' AND r.windows_reset>0 ORDER BY o.account_id`)
	if err != nil {
		return quotaResetStorageError()
	}
	type target struct {
		accountID int64
		requestID string
	}
	var targets []target
	for rows.Next() {
		var candidate target
		var requestHash string
		if err := rows.Scan(&candidate.accountID, &candidate.requestID, &requestHash); err != nil {
			_ = rows.Close()
			return quotaResetStorageError()
		}
		if requestHash == service.CarpoolResetIdentityHash(candidate.requestID) {
			targets = append(targets, candidate)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return quotaResetStorageError()
	}
	for _, candidate := range targets {
		if err := reconcileConfirmedQuotaResetOperation(ctx, db, candidate.accountID, candidate.requestID); err != nil {
			return err
		}
	}
	return nil
}

func reconcileConfirmedQuotaResetOperation(ctx context.Context, db *sql.DB, accountID int64, requestID string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return quotaResetStorageError()
	}
	defer func() { _ = tx.Rollback() }()
	var acquired bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1::text,824602))`, strconv.FormatInt(accountID, 10)).Scan(&acquired); err != nil {
		return quotaResetStorageError()
	}
	if !acquired {
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE quota_reset_operations o SET status='succeeded',
		result=jsonb_build_object('code','ok','windows_reset',r.windows_reset),completed_at=r.occurred_at
		FROM carpool_account_reset_receipts r WHERE o.account_id=$1 AND o.request_id=$2 AND o.status='pending'
		AND r.account_id=o.account_id AND r.identity_hash=o.identity_hash AND r.request_hash=$3 AND r.windows_reset>0`,
		accountID, requestID, service.CarpoolResetIdentityHash(requestID))
	if err != nil {
		return quotaResetStorageError()
	}
	if err := tx.Commit(); err != nil {
		return quotaResetStorageError()
	}
	return nil
}

func (r *quotaResetCoordinator) Run(ctx context.Context, accountID int64, creditID, requestID string, execute func(context.Context, string, string) (*service.OpenAIQuotaResetResult, error)) (*service.OpenAIQuotaResetResult, error) {
	if r == nil || r.db == nil {
		return nil, quotaResetStorageError()
	}
	if accountID <= 0 || strings.TrimSpace(requestID) == "" || execute == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_QUOTA_RESET_INVALID", "a valid account, request identity and reset callback are required")
	}
	storageCtx, cancel := context.WithTimeout(ctx, quotaResetStorageTimeout)
	defer cancel()
	conn, err := r.db.Conn(storageCtx)
	if err != nil {
		return nil, quotaResetStorageError()
	}
	// Hash the complete int64 identity with a dedicated namespace, never an int32 cast.
	lockKey := strconv.FormatInt(accountID, 10)
	var acquired bool
	err = conn.QueryRowContext(storageCtx,
		`SELECT pg_try_advisory_lock(hashtextextended($1::text, 824602))`, lockKey).Scan(&acquired)
	if err != nil {
		// Acquisition may have succeeded before the response was lost. Do not pool it.
		discardQuotaResetConnection(conn)
		return nil, quotaResetStorageError()
	}
	if !acquired {
		_ = conn.Close()
		return nil, quotaResetBusy()
	}
	defer releaseQuotaResetLock(ctx, conn, lockKey)

	op, err := prepareQuotaResetOperation(storageCtx, conn, accountID, creditID, requestID)
	if err != nil {
		return nil, err
	}
	if op.status != "pending" {
		var result service.OpenAIQuotaResetResult
		if json.Unmarshal(op.result, &result) != nil || quotaResetOutcome(&result) != op.status {
			return nil, quotaResetStorageError()
		}
		return &result, nil
	}
	identity, err := quotaResetAccountIdentity(storageCtx, conn, accountID)
	if err != nil {
		return nil, err
	}
	if op.identityHash == "" || op.identityHash != identity {
		return nil, quotaResetBusy()
	}
	cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The intent transaction is committed, while its session lock remains held.
	result, err := execute(ctx, op.creditID, op.requestID)
	if err != nil {
		return nil, err
	}
	status := quotaResetOutcome(result)
	if status == "pending" {
		return nil, quotaResetBusy()
	}
	// A replay denial cannot disprove consumption by an earlier unknown attempt.
	if status == "failed" && op.dispatchCount > 1 && result.Code != "vote_invalidated" {
		return nil, quotaResetBusy()
	}
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), quotaResetStorageTimeout)
	defer persistCancel()
	if status == "failed" && result.Code == "vote_invalidated" {
		if err := quotaResetVoteNeverDispatched(persistCtx, conn, accountID, op.requestID); err != nil {
			return nil, err
		}
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, quotaResetStorageError()
	}
	// A disconnected caller must not prevent saving an already completed consume.
	updated, err := conn.ExecContext(persistCtx, `UPDATE quota_reset_operations
		SET status=$1,result=$2::jsonb,completed_at=clock_timestamp()
		WHERE account_id=$3 AND request_id=$4 AND credit_id=$5 AND status='pending'`,
		status, string(payload), accountID, op.requestID, op.creditID)
	if err != nil {
		return nil, quotaResetStorageError()
	}
	count, err := updated.RowsAffected()
	if err != nil || count != 1 {
		return nil, quotaResetStorageError()
	}
	return result, nil
}

func prepareQuotaResetOperation(ctx context.Context, conn *sql.Conn, accountID int64, creditID, requestID string) (*quotaResetOperation, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, quotaResetStorageError()
	}
	defer func() { _ = tx.Rollback() }()
	op := &quotaResetOperation{}
	fresh := false
	mismatchedPending := false
	err = tx.QueryRowContext(ctx, `SELECT request_id, credit_id, status, result, identity_hash, dispatch_count FROM quota_reset_operations
		WHERE account_id=$1 AND request_id=$2`, accountID, requestID).
		Scan(&op.requestID, &op.creditID, &op.status, &op.result, &op.identityHash, &op.dispatchCount)
	switch {
	case err == nil:
		if op.creditID != creditID {
			return nil, quotaResetBusy()
		}
	case errors.Is(err, sql.ErrNoRows):
		err = tx.QueryRowContext(ctx, `SELECT request_id, credit_id, status, result, identity_hash, dispatch_count FROM quota_reset_operations
			WHERE account_id=$1 AND status='pending'`, accountID).
			Scan(&op.requestID, &op.creditID, &op.status, &op.result, &op.identityHash, &op.dispatchCount)
		switch {
		case err == nil:
			// Only legacy generic requests can recover a prior generic identity.
			// A different targeted request may reconcile an affirmative receipt,
			// but must never dispatch or adopt the prior operation's result.
			mismatchedPending = creditID != "" || op.creditID != ""
		case errors.Is(err, sql.ErrNoRows):
			op = &quotaResetOperation{requestID: requestID, creditID: creditID, status: "pending", dispatchCount: 1}
			fresh = true
		default:
			return nil, quotaResetStorageError()
		}
	default:
		return nil, quotaResetStorageError()
	}
	if err := guardQuotaResetVote(ctx, tx, accountID, op.requestID); err != nil {
		return nil, err
	}
	if !fresh && op.status == "pending" {
		// The affirmative receipt is written immediately after upstream success.
		// Recover it without another consume, even if eligibility changed meanwhile.
		var windows int
		var occurred time.Time
		err := tx.QueryRowContext(ctx, `SELECT windows_reset,occurred_at FROM carpool_account_reset_receipts
			WHERE account_id=$1 AND identity_hash=$2 AND request_hash=$3`, accountID, op.identityHash, service.CarpoolResetIdentityHash(op.requestID)).Scan(&windows, &occurred)
		if err == nil {
			op.result, _ = json.Marshal(service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: windows})
			_, err = tx.ExecContext(ctx, `UPDATE quota_reset_operations SET status='succeeded',result=$3::jsonb,completed_at=$4
				WHERE account_id=$1 AND request_id=$2 AND status='pending'`, accountID, op.requestID, string(op.result), occurred)
			if err != nil {
				return nil, quotaResetStorageError()
			}
			op.status = "succeeded"
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, quotaResetStorageError()
		}
	}
	if mismatchedPending {
		if op.status == "succeeded" {
			if err := tx.Commit(); err != nil {
				return nil, quotaResetStorageError()
			}
		}
		return nil, quotaResetBusy()
	}
	if op.status == "pending" {
		identity, err := quotaResetAccountIdentity(ctx, tx, accountID)
		if err != nil {
			return nil, err
		}
		if fresh {
			op.identityHash = identity
			_, err = tx.ExecContext(ctx, `INSERT INTO quota_reset_operations(account_id,request_id,credit_id,identity_hash,dispatch_count)
				VALUES($1,$2,$3,$4,1)`, accountID, requestID, creditID, op.identityHash)
		} else {
			if op.identityHash == "" || op.identityHash != identity {
				return nil, quotaResetBusy()
			}
			err = tx.QueryRowContext(ctx, `UPDATE quota_reset_operations SET dispatch_count=dispatch_count+1
				WHERE account_id=$1 AND request_id=$2 AND credit_id=$3 AND identity_hash=$4
				AND status='pending' AND dispatch_count=$5 RETURNING dispatch_count`,
				accountID, op.requestID, op.creditID, op.identityHash, op.dispatchCount).Scan(&op.dispatchCount)
		}
		if err != nil {
			var pgErr *pq.Error
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, quotaResetBusy()
			}
			return nil, quotaResetStorageError()
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, quotaResetStorageError()
	}
	return op, nil
}

func guardQuotaResetVote(ctx context.Context, tx *sql.Tx, accountID int64, requestID string) error {
	var installed bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('carpool_reset_votes') IS NOT NULL`).Scan(&installed); err != nil {
		return quotaResetStorageError()
	}
	if !installed {
		return nil
	}
	// An upstream success does not release the vote until local application finishes.
	var blocked bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM carpool_reset_votes
		WHERE account_id=$1 AND request_id IS DISTINCT FROM $2 AND dispatched_at IS NOT NULL
		AND status IN ('executing','awaiting_confirmation','applying'))`, accountID, requestID).Scan(&blocked)
	if err != nil {
		return quotaResetStorageError()
	}
	if blocked {
		return quotaResetBusy()
	}
	return nil
}

type quotaResetIdentityReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func quotaResetAccountIdentity(ctx context.Context, q quotaResetIdentityReader, accountID int64) (string, error) {
	var identity string
	err := q.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(btrim(credentials->>'chatgpt_account_id'),''),
		NULLIF(btrim(credentials->>'organization_id'),''),'') FROM accounts
		WHERE id=$1 AND deleted_at IS NULL`, accountID).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", quotaResetBusy()
	}
	if err != nil {
		return "", quotaResetStorageError()
	}
	if strings.TrimSpace(identity) == "" {
		return "", quotaResetBusy()
	}
	return service.CarpoolResetIdentityHash(identity), nil
}

func quotaResetVoteNeverDispatched(ctx context.Context, q quotaResetIdentityReader, accountID int64, requestID string) error {
	var installed bool
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('carpool_reset_votes') IS NOT NULL`).Scan(&installed); err != nil {
		return quotaResetStorageError()
	}
	if !installed {
		return quotaResetBusy()
	}
	var safe bool
	err := q.QueryRowContext(ctx, `SELECT COALESCE(bool_and(dispatched_at IS NULL
		AND status IN ('invalidated','expired','failed')),false) FROM carpool_reset_votes
		WHERE account_id=$1 AND request_id=$2`, accountID, requestID).Scan(&safe)
	if err != nil {
		return quotaResetStorageError()
	}
	if !safe {
		return quotaResetBusy()
	}
	return nil
}

func quotaResetOutcome(result *service.OpenAIQuotaResetResult) string {
	if result == nil || strings.TrimSpace(result.Code) == "" {
		return "pending"
	}
	if result.Code == "ok" && result.WindowsReset > 0 {
		return "succeeded"
	}
	if result.Code != "ok" && result.WindowsReset == 0 {
		return "failed"
	}
	return "pending"
}

func quotaResetBusy() error {
	return infraerrors.New(http.StatusConflict, "OPENAI_QUOTA_RESET_BUSY", "another quota reset is in progress or awaiting confirmation")
}

func quotaResetStorageError() error {
	// Database constraint details can contain raw card and request identifiers.
	return infraerrors.New(http.StatusServiceUnavailable, "OPENAI_QUOTA_RESET_COORDINATION_FAILED", "unable to persist or recover the quota reset; retry the same request")
}

func releaseQuotaResetLock(ctx context.Context, conn *sql.Conn, lockKey string) {
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), quotaResetStorageTimeout)
	defer cancel()
	var unlocked bool
	err := conn.QueryRowContext(unlockCtx,
		`SELECT pg_advisory_unlock(hashtextextended($1::text, 824602))`, lockKey).Scan(&unlocked)
	if err != nil || !unlocked {
		discardQuotaResetConnection(conn)
		return
	}
	_ = conn.Close()
}

func discardQuotaResetConnection(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}
