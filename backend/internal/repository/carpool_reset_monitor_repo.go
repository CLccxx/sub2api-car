package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// Source is a compatibility convenience for a single-account installation.
// Production monitoring enumerates Sources and never chooses one global group.
func (r *carpoolResetMonitorRepository) Sources(ctx context.Context) ([]service.CarpoolResetSource, error) {
	return carpoolResetSources(ctx, r.db)
}

// Every selected group has exactly one undeleted account binding. A second
// inactive or otherwise unusable account still makes the group ambiguous.
const carpoolAccountSubscriptionGroups = `SELECT g.id FROM groups g
 JOIN account_groups ag ON ag.group_id=g.id
 WHERE ag.account_id=$1 AND g.deleted_at IS NULL AND g.status='active'
 AND g.platform='openai' AND g.subscription_type='subscription'
 AND NOT EXISTS(SELECT 1 FROM account_groups other JOIN accounts a ON a.id=other.account_id
   WHERE other.group_id=g.id AND other.account_id<>$1 AND a.deleted_at IS NULL)`

func carpoolResetSources(ctx context.Context, q carpoolResetQuerier) ([]service.CarpoolResetSource, error) {
	rows, err := q.QueryContext(ctx, `SELECT min(g.id),a.id,btrim(a.credentials->>'chatgpt_account_id'),
 (SELECT count(*) FROM carpool_reset_account_changes c WHERE c.account_id=a.id)
 FROM accounts a JOIN account_groups ag ON ag.account_id=a.id JOIN groups g ON g.id=ag.group_id
 WHERE a.deleted_at IS NULL AND a.platform='openai' AND a.type='oauth' AND a.status='active'
 AND a.parent_account_id IS NULL AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND btrim(COALESCE(a.credentials->>'chatgpt_account_id',''))<>''
 AND g.deleted_at IS NULL AND g.status='active' AND g.platform='openai' AND g.subscription_type='subscription'
 AND NOT EXISTS(SELECT 1 FROM account_groups other JOIN accounts b ON b.id=other.account_id
   WHERE other.group_id=g.id AND b.deleted_at IS NULL AND b.id<>a.id)
 GROUP BY a.id,a.credentials->>'chatgpt_account_id' ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var sources []service.CarpoolResetSource
	for rows.Next() {
		var source service.CarpoolResetSource
		if err = rows.Scan(&source.GroupID, &source.AccountID, &source.UpstreamAccountID, &source.Revision); err != nil {
			return nil, err
		}
		source.ScopeVersion = 1
		source.IdentityHash = service.CarpoolResetIdentityHash(source.UpstreamAccountID)
		sources = append(sources, source)
	}
	return sources, rows.Err()
}
func carpoolResetSource(ctx context.Context, q carpoolResetQuerier) (*service.CarpoolResetSource, error) {
	sources, err := carpoolResetSources(ctx, q)
	if err != nil || len(sources) != 1 {
		return nil, err
	}
	return &sources[0], nil
}
func carpoolResetSourceForAccount(ctx context.Context, q carpoolResetQuerier, account int64) (*service.CarpoolResetSource, error) {
	sources, err := carpoolResetSources(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		if source.AccountID == account {
			return &source, nil
		}
	}
	return nil, nil
}
func sameCarpoolResetSource(a, b *service.CarpoolResetSource) bool {
	return a != nil && b != nil && a.AccountID == b.AccountID && a.IdentityHash == b.IdentityHash && a.Revision == b.Revision && a.ScopeVersion == b.ScopeVersion
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
	sources, err := carpoolResetSources(ctx, tx)
	if err != nil {
		return err
	}
	// Recheck under the source predicate lock, so a stale caller cannot clear a
	// newly valid account. No other account's pending work is cancelled.
	rows, err := tx.QueryContext(ctx, `SELECT account_id FROM carpool_reset_account_monitors`)
	if err != nil {
		return err
	}
	var absent []int64
	for rows.Next() {
		var account int64
		if err = rows.Scan(&account); err != nil {
			_ = rows.Close()
			return err
		}
		found := false
		for _, source := range sources {
			if source.AccountID == account {
				found = true
				break
			}
		}
		if !found {
			absent = append(absent, account)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, account := range absent {
		if _, err = tx.ExecContext(ctx, `UPDATE carpool_reset_account_monitors SET baseline=NULL,updated_at=NOW() WHERE account_id=$1`, account); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE carpool_subscription_reset_events SET status='cancelled',cancel_reason='source_unavailable',completed_at=NOW() WHERE status='pending' AND reason<>'vote' AND account_id=$1`, account); err != nil {
			return err
		}
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
	var lockID int
	if err = tx.QueryRowContext(ctx, `SELECT id FROM carpool_reset_monitor WHERE id=1 FOR UPDATE`).Scan(&lockID); err != nil {
		return nil, err
	}
	var identity string
	var revision int64
	var scopeVersion int
	var baselineJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT identity_hash,baseline,source_revision,scope_version FROM carpool_reset_account_monitors WHERE account_id=$1`, source.AccountID).Scan(&identity, &baselineJSON, &revision, &scopeVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	current, err := carpoolResetSourceForAccount(ctx, tx, source.AccountID)
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
	baselineMatches := scopeVersion == source.ScopeVersion && revision == source.Revision
	if scopeVersion == 0 && len(baselineJSON) > 0 {
		var globalRevision int64
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM carpool_reset_source_changes`).Scan(&globalRevision); err != nil {
			return nil, err
		}
		baselineMatches = revision == globalRevision
	}
	if identity == source.IdentityHash && baselineMatches && len(baselineJSON) > 0 {
		if err = json.Unmarshal(baselineJSON, &previous); err != nil {
			return nil, err
		}
	}
	if previous == nil {
		if _, err = tx.ExecContext(ctx, `UPDATE carpool_subscription_reset_events SET status='cancelled',cancel_reason='source_changed',completed_at=NOW() WHERE status='pending' AND reason<>'vote' AND account_id=$1`, source.AccountID); err != nil {
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO carpool_reset_account_monitors(group_id,account_id,identity_hash,baseline,source_revision,scope_version) VALUES($1,$2,$3,$4::jsonb,$5,1) ON CONFLICT(account_id) DO UPDATE SET group_id=EXCLUDED.group_id,identity_hash=EXCLUDED.identity_hash,baseline=EXCLUDED.baseline,source_revision=EXCLUDED.source_revision,scope_version=1,updated_at=NOW()`, source.GroupID, source.AccountID, source.IdentityHash, string(encoded), source.Revision); err != nil {
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
		err = tx.QueryRowContext(ctx, `INSERT INTO carpool_subscription_reset_events(event_key,group_id,account_id,identity_hash,reason,observed_at,expires_at,source_revision,scope_version)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,1) ON CONFLICT(event_key) DO NOTHING RETURNING id`, service.CarpoolResetEventKey(source, observation, reason), source.GroupID, source.AccountID, source.IdentityHash, reason, observation.ObservedAt, observation.ObservedAt.Add(service.CarpoolResetEventLifetime), source.Revision).Scan(&eventID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			if _, err = tx.ExecContext(ctx, `INSERT INTO carpool_subscription_reset_targets(event_id,subscription_id,user_id,group_id,starts_at,binding_revision)
 SELECT $2,s.id,s.user_id,s.group_id,s.starts_at,(SELECT count(*) FROM carpool_reset_group_changes c WHERE c.group_id=s.group_id) FROM user_subscriptions s WHERE s.deleted_at IS NULL AND s.status='active' AND s.starts_at<=$3 AND s.expires_at>$3 AND s.group_id IN (`+carpoolAccountSubscriptionGroups+`) ORDER BY s.id`, source.AccountID, eventID, observation.ObservedAt); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE carpool_subscription_reset_events SET status='cancelled',cancel_reason='superseded',completed_at=NOW()
				WHERE status='pending' AND reason<>'vote' AND id<>$1 AND observed_at<=$2 AND account_id=$3`, eventID, observation.ObservedAt, source.AccountID); err != nil {
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
