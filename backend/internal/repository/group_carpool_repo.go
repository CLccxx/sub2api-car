package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type carpoolQuotaRepository struct{ db *sql.DB }

func NewCarpoolQuotaRepository(db *sql.DB) service.CarpoolQuotaRepository {
	return &carpoolQuotaRepository{db: db}
}

func (r *carpoolQuotaRepository) ListPending(ctx context.Context, slot time.Time) ([]service.CarpoolQuotaTarget, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, carpool_members, updated_at FROM groups
		WHERE deleted_at IS NULL AND status = 'active' AND platform = 'openai'
		AND subscription_type = 'subscription' AND carpool_members BETWEEN 2 AND 5
		AND (carpool_quota_synced_at IS NULL OR carpool_quota_synced_at < $1)
		ORDER BY id`, slot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []service.CarpoolQuotaTarget
	for rows.Next() {
		var target service.CarpoolQuotaTarget
		if err := rows.Scan(&target.GroupID, &target.Members, &target.Revision); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (r *carpoolQuotaRepository) Apply(ctx context.Context, target service.CarpoolQuotaTarget, account *service.Account, limit float64, slot time.Time) (bool, error) {
	if account == nil || limit <= 0 || math.IsNaN(limit) || math.IsInf(limit, 0) {
		return false, errors.New("invalid carpool quota update")
	}
	credentials, err := json.Marshal(account.Credentials)
	if err != nil {
		return false, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Group inserts/binding changes use the same live-group lock protocol.
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id = $1
		AND carpool_members = $2 AND updated_at = $3 AND deleted_at IS NULL
		AND status = 'active' AND platform = 'openai' AND subscription_type = 'subscription'
		AND (carpool_quota_synced_at IS NULL OR carpool_quota_synced_at < $4)
		FOR UPDATE`, target.GroupID, target.Members, target.Revision, slot).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Lock bindings and identities, including unsupported or disabled extra accounts.
	rows, err := tx.QueryContext(ctx, `SELECT a.id,
		(a.platform = 'openai' AND a.type = 'oauth' AND a.status = 'active'
		 AND a.parent_account_id IS NULL AND a.credentials = $2::jsonb
		 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at > NOW())) AS eligible
		FROM accounts a JOIN account_groups ag ON ag.account_id = a.id
		WHERE ag.group_id = $1 AND a.deleted_at IS NULL ORDER BY a.id LIMIT 2
		FOR SHARE OF a, ag`, target.GroupID, string(credentials))
	if err != nil {
		return false, err
	}
	count, matches := 0, false
	for rows.Next() {
		var accountID int64
		var eligible bool
		if err := rows.Scan(&accountID, &eligible); err != nil {
			rows.Close()
			return false, err
		}
		count++
		matches = accountID == account.ID && eligible
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if count != 1 || !matches {
		return false, nil
	}
	// Deliberately never write user_subscriptions or any usage/window column.
	if _, err = tx.ExecContext(ctx, `UPDATE groups SET weekly_limit_usd = $1,
		carpool_quota_synced_at = $2, updated_at = NOW() WHERE id = $3`, limit, slot, target.GroupID); err != nil {
		return false, err
	}
	// Persist cache invalidation with the cap, even if this process dies on commit.
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_cache_invalidation_outbox (cache_key)
		SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex') FROM api_keys k
		WHERE k.group_id = $1 AND k.deleted_at IS NULL AND k.key <> ''`, target.GroupID); err != nil {
		return false, err
	}
	if err = enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventGroupChanged, nil, &target.GroupID, nil); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
