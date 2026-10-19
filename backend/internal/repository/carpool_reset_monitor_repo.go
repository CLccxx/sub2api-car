package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type carpoolResetMonitorRepository struct{ db *sql.DB }

type carpoolResetQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func NewCarpoolResetMonitorRepository(db *sql.DB) service.CarpoolResetMonitorRepository {
	return &carpoolResetMonitorRepository{db: db}
}

func (r *carpoolResetMonitorRepository) Source(ctx context.Context) (*service.CarpoolResetSource, error) {
	return carpoolResetSource(ctx, r.db)
}

func carpoolResetSource(ctx context.Context, exec carpoolResetQuerier) (*service.CarpoolResetSource, error) {
	query := `SELECT id FROM groups WHERE deleted_at IS NULL AND status='active'
		AND platform='openai' AND subscription_type='subscription' AND carpool_members BETWEEN 2 AND 5 ORDER BY id LIMIT 2`
	rows, err := exec.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	var groupIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		groupIDs = append(groupIDs, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil || len(groupIDs) != 1 {
		return nil, err
	}
	query = `SELECT a.id, a.platform, a.type, a.status, a.parent_account_id,
		COALESCE(a.credentials->>'chatgpt_account_id',''),
		(NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
		FROM accounts a JOIN account_groups ag ON ag.account_id=a.id
		WHERE ag.group_id=$1 AND a.deleted_at IS NULL ORDER BY a.id LIMIT 2`
	rows, err = exec.QueryContext(ctx, query, groupIDs[0])
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	count := 0
	var source *service.CarpoolResetSource
	for rows.Next() {
		count++
		var id int64
		var platform, kind, status, upstream string
		var parent sql.NullInt64
		var unexpired bool
		if err := rows.Scan(&id, &platform, &kind, &status, &parent, &upstream, &unexpired); err != nil {
			return nil, err
		}
		upstream = strings.TrimSpace(upstream)
		if platform == service.PlatformOpenAI && kind == service.AccountTypeOAuth && status == service.StatusActive && !parent.Valid && unexpired && upstream != "" {
			source = &service.CarpoolResetSource{GroupID: groupIDs[0], AccountID: id, UpstreamAccountID: upstream, IdentityHash: service.CarpoolResetIdentityHash(upstream)}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if count != 1 || source == nil {
		return nil, nil
	}
	_ = rows.Close()
	// Count committed append-only changes, not max(id): sequence allocation can
	// precede another transaction's commit and otherwise hide a late change.
	if err := exec.QueryRowContext(ctx, `SELECT count(*) FROM carpool_reset_source_changes`).Scan(&source.Revision); err != nil {
		return nil, err
	}
	return source, nil
}

func sameCarpoolResetSource(a, b *service.CarpoolResetSource) bool {
	return a != nil && b != nil && a.GroupID == b.GroupID && a.AccountID == b.AccountID && a.IdentityHash == b.IdentityHash && a.Revision == b.Revision
}

func (r *carpoolResetMonitorRepository) beginSourceGuardedTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='10s'`); err == nil {
		// Protect the whole sole-source predicate, including excluded/deleted rows
		// and new bindings. Never wait behind normal account/group writes.
		_, err = tx.ExecContext(ctx, `LOCK TABLE groups, accounts, account_groups IN SHARE MODE NOWAIT`)
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (r *carpoolResetMonitorRepository) ClearBaseline(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.beginSourceGuardedTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO carpool_reset_monitor(id) VALUES(1) ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	var id int
	if err = tx.QueryRowContext(ctx, `SELECT id FROM carpool_reset_monitor WHERE id=1 FOR UPDATE`).Scan(&id); err != nil {
		return err
	}
	current, err := carpoolResetSource(ctx, tx)
	if err != nil || current != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE carpool_reset_monitor SET baseline=NULL, updated_at=NOW() WHERE id=1`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE carpool_subscription_reset_events SET status='cancelled',cancel_reason='source_unavailable',completed_at=NOW() WHERE status='pending' AND reason<>'vote'`); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *carpoolResetMonitorRepository) Observe(ctx context.Context, source service.CarpoolResetSource, observation service.CarpoolResetObservation) (*service.CarpoolResetEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.beginSourceGuardedTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO carpool_reset_monitor(id) VALUES(1) ON CONFLICT DO NOTHING`); err != nil {
		return nil, err
	}
	var groupID, accountID sql.NullInt64
	var identity sql.NullString
	var revision int64
	var baselineJSON []byte
	if err = tx.QueryRowContext(ctx, `SELECT group_id,account_id,identity_hash,baseline,source_revision FROM carpool_reset_monitor WHERE id=1 FOR UPDATE`).Scan(&groupID, &accountID, &identity, &baselineJSON, &revision); err != nil {
		return nil, err
	}
	current, err := carpoolResetSource(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !sameCarpoolResetSource(current, &source) {
		return nil, nil
	}
	if blocked, err := carpoolVoteObservationBlocked(ctx, tx, source.AccountID); err != nil || blocked {
		return nil, err
	}
	var previous *service.CarpoolResetBaseline
	if groupID.Int64 == source.GroupID && accountID.Int64 == source.AccountID && identity.String == source.IdentityHash && revision == source.Revision && len(baselineJSON) > 0 {
		if err = json.Unmarshal(baselineJSON, &previous); err != nil {
			return nil, err
		}
	}
	if previous == nil {
		if _, err = tx.ExecContext(ctx, `UPDATE carpool_subscription_reset_events SET status='cancelled',cancel_reason='source_changed',completed_at=NOW() WHERE status='pending' AND reason<>'vote'`); err != nil {
			return nil, err
		}
	}
	var receipt service.CarpoolResetReceipt
	err = tx.QueryRowContext(ctx, `SELECT id,occurred_at FROM carpool_account_reset_receipts
		WHERE account_id=$1 AND identity_hash=$2 AND occurred_at<=$3 ORDER BY id DESC LIMIT 1`, source.AccountID, source.IdentityHash, observation.ObservedAt).Scan(&receipt.ID, &receipt.OccurredAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		observation.Receipt = &receipt
	}
	next, reason := service.AdvanceCarpoolResetBaseline(previous, observation)
	encoded, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE carpool_reset_monitor SET group_id=$1,account_id=$2,identity_hash=$3,baseline=$4::jsonb,source_revision=$5,updated_at=NOW() WHERE id=1`, source.GroupID, source.AccountID, source.IdentityHash, string(encoded), source.Revision); err != nil {
		return nil, err
	}
	var event *service.CarpoolResetEvent
	if reason != "" {
		canonical, err := carpoolVoteObservedEvent(ctx, tx, source, observation, reason)
		if err != nil {
			return nil, err
		}
		if canonical > 0 {
			return nil, tx.Commit()
		}
		resetTime := observation.ResetAt.Add(-7 * 24 * time.Hour)
		if reason == "receipt" && observation.Receipt != nil {
			resetTime = observation.Receipt.OccurredAt
		}
		if err = invalidateCarpoolVotes(ctx, tx, source.AccountID, "window_reset", resetTime); err != nil {
			return nil, err
		}
		var eventID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO carpool_subscription_reset_events(event_key,group_id,account_id,identity_hash,reason,observed_at,expires_at,source_revision)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(event_key) DO NOTHING RETURNING id`, service.CarpoolResetEventKey(source, observation, reason), source.GroupID, source.AccountID, source.IdentityHash, reason, observation.ObservedAt, observation.ObservedAt.Add(service.CarpoolResetEventLifetime), source.Revision).Scan(&eventID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			if _, err = tx.ExecContext(ctx, `INSERT INTO carpool_subscription_reset_targets(event_id,subscription_id,user_id,group_id,starts_at)
				SELECT $1,id,user_id,group_id,starts_at FROM user_subscriptions WHERE deleted_at IS NULL AND status='active' AND expires_at>$2 ORDER BY id`, eventID, observation.ObservedAt); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE carpool_subscription_reset_events SET status='cancelled',cancel_reason='superseded',completed_at=NOW()
				WHERE status='pending' AND reason<>'vote' AND id<>$1 AND observed_at<=$2`, eventID, observation.ObservedAt); err != nil {
				return nil, err
			}
			event = &service.CarpoolResetEvent{ID: eventID, Status: "pending", Reason: reason}
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return event, nil
}

func (r *carpoolResetMonitorRepository) RecordResetReceipt(ctx context.Context, accountID int64, identity, request string, occurred time.Time, windows int) error {
	if accountID <= 0 || identity == "" || request == "" || windows <= 0 {
		return errors.New("invalid reset receipt")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO carpool_reset_monitor(id) VALUES(1) ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	var id int
	if err = tx.QueryRowContext(ctx, `SELECT id FROM carpool_reset_monitor WHERE id=1 FOR UPDATE`).Scan(&id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO carpool_account_reset_receipts(account_id,identity_hash,request_hash,occurred_at,windows_reset)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT(account_id,identity_hash,request_hash) DO NOTHING`, accountID, identity, request, occurred.UTC().Truncate(time.Second), windows)
	if err != nil {
		return err
	}
	if inserted, _ := result.RowsAffected(); inserted > 0 {
		if err = invalidateCarpoolVotes(ctx, tx, accountID, "admin_reset", occurred); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *carpoolResetMonitorRepository) PendingEvents(ctx context.Context) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM carpool_subscription_reset_events WHERE status='pending' ORDER BY id LIMIT 20`)
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
