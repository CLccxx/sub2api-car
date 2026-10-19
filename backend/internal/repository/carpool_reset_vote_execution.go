package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *carpoolResetVoteRepository) Pending(ctx context.Context) ([]int64, error) {
	if err := reconcileConfirmedQuotaResetOperations(ctx, r.db); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM carpool_reset_votes WHERE status IN ('voting','executing','applying')
		OR (status='awaiting_confirmation' AND updated_at<clock_timestamp()-interval '1 minute')
		OR (dispatched_at IS NULL AND EXISTS(SELECT 1 FROM quota_reset_operations o WHERE o.account_id=carpool_reset_votes.account_id
			AND o.request_id=carpool_reset_votes.request_id AND o.status='pending')) ORDER BY id LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *carpoolResetVoteRepository) Load(ctx context.Context, id int64) (*service.CarpoolResetVote, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := scanVote(tx.QueryRowContext(ctx, `SELECT `+voteColumns+` FROM carpool_reset_votes WHERE id=$1 FOR UPDATE`, id))
	if err != nil || p == nil {
		return p, err
	}
	current, err := voteEligibility(ctx, tx, p.Eligibility.GroupID)
	if err != nil {
		return nil, err
	}
	if err = validateVote(ctx, tx, p, current); err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

func (r *carpoolResetVoteRepository) Prepare(ctx context.Context, id int64, o service.CarpoolResetObservation, credit, request string) (bool, error) {
	return r.prepareOrDispatch(ctx, id, &o, credit, request)
}
func (r *carpoolResetVoteRepository) Dispatch(ctx context.Context, id int64) (bool, error) {
	return r.prepareOrDispatch(ctx, id, nil, "", "")
}

func (r *carpoolResetVoteRepository) prepareOrDispatch(ctx context.Context, id int64, o *service.CarpoolResetObservation, credit, request string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := scanVote(tx.QueryRowContext(ctx, `SELECT `+voteColumns+` FROM carpool_reset_votes WHERE id=$1 FOR UPDATE`, id))
	if err != nil || p == nil {
		return false, err
	}
	current, err := voteEligibility(ctx, tx, p.Eligibility.GroupID)
	if err != nil {
		return false, err
	}
	if err = validateVote(ctx, tx, p, current); err != nil {
		return false, err
	}
	if p.Status != "executing" && p.Status != "awaiting_confirmation" {
		return false, tx.Commit()
	}
	if p.Dispatched {
		// Never replay an uncertain request against a changed account identity or electorate.
		var ownReceipts int64
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM carpool_account_reset_receipts WHERE account_id=$1 AND request_hash=$2`, p.Eligibility.Source.AccountID, service.CarpoolResetIdentityHash(p.RequestID)).Scan(&ownReceipts); err != nil {
			return false, err
		}
		current.ReceiptCount -= ownReceipts
		if voteEligibilityReason(p.Eligibility, *current) != "" {
			return false, errors.New("uncertain vote requires reconciliation after context change")
		}
		return true, tx.Commit()
	}
	if o != nil {
		var now time.Time
		if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return false, err
		}
		if o.ObservedAt.Before(now.Add(-2*time.Minute)) || !o.ResetAt.After(now.Add(time.Minute)) ||
			o.ResetAt.Sub(p.Observation.ResetAt) > time.Minute || p.Observation.ResetAt.Sub(o.ResetAt) > time.Minute {
			_, err = tx.ExecContext(ctx, `UPDATE carpool_reset_votes SET status='invalidated',reason='window_reset',updated_at=clock_timestamp() WHERE id=$1`, id)
			if err != nil {
				return false, err
			}
			return false, tx.Commit()
		}
		if p.CreditID == "" {
			if credit == "" || request == "" {
				return false, errors.New("missing vote reset identity")
			}
			_, err = tx.ExecContext(ctx, `UPDATE carpool_reset_votes SET credit_id=$2,request_id=$3,request_hash=$4,updated_at=clock_timestamp() WHERE id=$1`, id, credit, request, service.CarpoolResetIdentityHash(request))
		}
	} else {
		if p.CreditID == "" || p.RequestID == "" {
			return false, errors.New("unprepared vote")
		}
		_, err = tx.ExecContext(ctx, `UPDATE carpool_reset_votes SET dispatched_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO carpool_reset_vote_targets(proposal_id,subscription_id,user_id,group_id,starts_at)
				SELECT $1,id,user_id,group_id,starts_at FROM user_subscriptions WHERE deleted_at IS NULL AND status='active'
				AND starts_at<=clock_timestamp() AND expires_at>clock_timestamp() ORDER BY id ON CONFLICT DO NOTHING`, id)
		}
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (r *carpoolResetVoteRepository) AwaitConfirmation(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE carpool_reset_votes SET status='awaiting_confirmation',reason='confirmation_pending',updated_at=clock_timestamp()
		WHERE id=$1 AND dispatched_at IS NOT NULL AND status IN ('executing','awaiting_confirmation')`, id)
	return err
}
func (r *carpoolResetVoteRepository) Fail(ctx context.Context, id int64, reason string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE carpool_reset_votes SET status='failed',reason=$2,updated_at=clock_timestamp()
		WHERE id=$1 AND status IN ('executing','awaiting_confirmation') AND (dispatched_at IS NULL OR EXISTS(
			SELECT 1 FROM quota_reset_operations o WHERE o.account_id=carpool_reset_votes.account_id
			AND o.request_id=carpool_reset_votes.request_id AND o.status='failed'))`, id, reason)
	if err != nil {
		return err
	}
	return closeUndispatchedVoteIntent(ctx, r.db, id)
}

func (r *carpoolResetVoteRepository) Succeed(ctx context.Context, id int64, result *service.OpenAIQuotaResetResult) (int64, error) {
	if result == nil || result.Code != "ok" || result.WindowsReset <= 0 {
		return 0, errors.New("reset not confirmed")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := scanVote(tx.QueryRowContext(ctx, `SELECT `+voteColumns+` FROM carpool_reset_votes WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return 0, err
	}
	if p == nil || !p.Dispatched {
		return 0, errors.New("vote was not dispatched")
	}
	if p.EventID > 0 {
		return p.EventID, nil
	}
	if p.Status != "executing" && p.Status != "awaiting_confirmation" {
		return 0, errors.New("vote already closed")
	}
	var occurred time.Time
	// The operation ledger is authoritative even when the HTTP response was lost.
	err = tx.QueryRowContext(ctx, `SELECT completed_at FROM quota_reset_operations WHERE account_id=$1 AND request_id=$2 AND status='succeeded'`, p.Eligibility.Source.AccountID, p.RequestID).Scan(&occurred)
	if err != nil {
		return 0, err
	}
	source := p.Eligibility.Source
	_, err = tx.ExecContext(ctx, `INSERT INTO carpool_account_reset_receipts(account_id,identity_hash,request_hash,occurred_at,windows_reset)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, source.AccountID, source.IdentityHash, service.CarpoolResetIdentityHash(p.RequestID), occurred, result.WindowsReset)
	if err != nil {
		return 0, err
	}
	var event int64
	err = tx.QueryRowContext(ctx, `INSERT INTO carpool_subscription_reset_events(event_key,group_id,account_id,identity_hash,reason,observed_at,expires_at,source_revision)
		VALUES($1,$2,$3,$4,'vote',$5,$5::timestamptz+interval '100 years',$6) RETURNING id`, fmt.Sprintf("vote:%d", id), source.GroupID, source.AccountID, source.IdentityHash, occurred, source.Revision).Scan(&event)
	if err != nil {
		return 0, err
	}
	// The immutable pre-dispatch snapshot prevents a recovery from including
	// subscriptions created after consumption; ApplyEvent rechecks each target.
	_, err = tx.ExecContext(ctx, `INSERT INTO carpool_subscription_reset_targets(event_id,subscription_id,user_id,group_id,starts_at)
		SELECT $1,subscription_id,user_id,group_id,starts_at FROM carpool_reset_vote_targets WHERE proposal_id=$2 ORDER BY subscription_id`, event, id)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE carpool_reset_votes SET status='applying',reason=NULL,event_id=$2,succeeded_at=$3,updated_at=clock_timestamp() WHERE id=$1`, id, event, occurred)
	if err != nil {
		return 0, err
	}
	// Keep an initial baseline when no scheduled monitor has run yet. Subsequent
	// window/receipt evidence is correlated to this event, never replayed as a reset.
	baseline := service.CarpoolResetBaseline{Observation: p.Observation, Since: p.Observation.ObservedAt}
	encoded, _ := json.Marshal(baseline)
	_, err = tx.ExecContext(ctx, `UPDATE carpool_reset_monitor SET group_id=$1,account_id=$2,identity_hash=$3,source_revision=$4,baseline=$5::jsonb
		WHERE id=1 AND (baseline IS NULL OR account_id IS DISTINCT FROM $2 OR source_revision<>$4)`, source.GroupID, source.AccountID, source.IdentityHash, source.Revision, string(encoded))
	if err != nil {
		return 0, err
	}
	return event, tx.Commit()
}

func (r *carpoolResetVoteRepository) Complete(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE carpool_reset_votes p SET status='completed',reason=NULL,updated_at=clock_timestamp()
		WHERE p.id=$1 AND p.status='applying' AND EXISTS(SELECT 1 FROM carpool_subscription_reset_events e WHERE e.id=p.event_id AND e.status='completed')
		AND NOT EXISTS(SELECT 1 FROM carpool_subscription_reset_targets t WHERE t.event_id=p.event_id AND t.reset_applied AND t.cache_invalidated_at IS NULL)`, id)
	return err
}

// The monitor may read a window while the consume HTTP request is still in
// flight. Defer that evidence until the durable vote result can own its event.
func carpoolVoteObservationBlocked(ctx context.Context, tx *sql.Tx, accountID int64) (bool, error) {
	var installed bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('carpool_reset_votes') IS NOT NULL`).Scan(&installed); err != nil || !installed {
		return false, err
	}
	var blocked bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM carpool_reset_votes WHERE account_id=$1 AND dispatched_at IS NOT NULL
		AND status IN ('executing','awaiting_confirmation','applying') AND event_id IS NULL)`, accountID).Scan(&blocked)
	return blocked, err
}

func carpoolVoteObservedEvent(ctx context.Context, tx *sql.Tx, source service.CarpoolResetSource, o service.CarpoolResetObservation, reason string) (int64, error) {
	var installed bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('carpool_reset_votes') IS NOT NULL`).Scan(&installed); err != nil || !installed {
		return 0, err
	}
	var id int64
	var err error
	if reason == "receipt" && o.Receipt != nil {
		err = tx.QueryRowContext(ctx, `SELECT p.event_id FROM carpool_reset_votes p JOIN carpool_account_reset_receipts r ON r.id=$2 AND r.request_hash=p.request_hash
			JOIN carpool_subscription_reset_events e ON e.id=p.event_id WHERE p.account_id=$1 AND e.identity_hash=$3 LIMIT 1`, source.AccountID, o.Receipt.ID, source.IdentityHash).Scan(&id)
	} else if reason == "window" {
		start := o.ResetAt.Add(-7 * 24 * time.Hour)
		var receiptID int64
		if o.Receipt != nil {
			receiptID = o.Receipt.ID
		}
		// Persistence may recover long after upstream consumed the card. Match
		// evidence since dispatch up to local application, not just the DB receipt time.
		err = tx.QueryRowContext(ctx, `SELECT p.event_id FROM carpool_reset_votes p JOIN carpool_subscription_reset_events e ON e.id=p.event_id
			WHERE p.account_id=$1 AND e.identity_hash=$3 AND $2::timestamptz BETWEEN p.dispatched_at-interval '1 minute'
			AND COALESCE(e.completed_at,GREATEST(p.succeeded_at,clock_timestamp()))+interval '1 second'
			AND NOT EXISTS(SELECT 1 FROM carpool_account_reset_receipts r WHERE r.id=$4 AND r.request_hash<>p.request_hash)
			ORDER BY p.id DESC LIMIT 1`, source.AccountID, start, source.IdentityHash, receiptID).Scan(&id)
	} else {
		return 0, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func invalidateCarpoolVotes(ctx context.Context, tx *sql.Tx, account int64, reason string, occurred time.Time) error {
	var installed bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('carpool_reset_votes') IS NOT NULL`).Scan(&installed); err != nil || !installed {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE carpool_reset_votes SET status='invalidated',reason=$2,updated_at=clock_timestamp()
		WHERE account_id=$1 AND dispatched_at IS NULL AND status IN ('voting','executing') AND created_at<=$3`, account, reason, occurred)
	if err != nil {
		return err
	}
	return closeUndispatchedVoteIntent(ctx, tx, 0)
}

type voteIntentCloser interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func closeUndispatchedVoteIntent(ctx context.Context, exec voteIntentCloser, id int64) error {
	// Dispatch takes the same proposal/monitor locks before entering HTTP. A
	// terminal proposal with no dispatch timestamp therefore proves no spend occurred.
	_, err := exec.ExecContext(ctx, `UPDATE quota_reset_operations o SET status='failed',
		result='{"code":"vote_invalidated","windows_reset":0}'::jsonb,completed_at=clock_timestamp()
		FROM carpool_reset_votes p WHERE ($1::bigint=0 OR p.id=$1) AND o.account_id=p.account_id AND o.request_id=p.request_id
		AND o.status='pending' AND p.dispatched_at IS NULL AND p.status IN ('expired','invalidated','failed')`, id)
	return err
}
