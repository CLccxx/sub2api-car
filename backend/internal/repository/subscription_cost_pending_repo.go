package repository

import (
	"context"
	"database/sql"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"strings"
	"time"
)

func (r *subscriptionCostRepository) RegisterPending(ctx context.Context, pending service.SubscriptionCostPendingUsage) (int64, error) {
	if pending.AccountID <= 0 || strings.TrimSpace(pending.Identity) == "" {
		return 0, errors.New("missing accounting identity")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var accountID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE id=$1 AND platform='openai' AND type='oauth' AND parent_account_id IS NULL AND deleted_at IS NULL AND btrim(COALESCE(credentials->>'chatgpt_account_id',''))=$2 FOR UPDATE`, pending.AccountID, strings.TrimSpace(pending.Identity)).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var changed bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM subscription_cost_account_epochs WHERE account_id=$1 AND identity_started_at>$2)`, accountID, pending.StartedAt).Scan(&changed)
	if err != nil {
		return 0, err
	}
	if changed {
		return 0, nil
	}
	var utilization, resetAt any
	if pending.Observation != nil {
		utilization = pending.Observation.Utilization
		resetAt = pending.Observation.ResetAt
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO subscription_cost_pending_usage(account_id,identity_hash,started_at,observed_at,utilization,reset_at) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, accountID, service.CarpoolResetIdentityHash(pending.Identity), pending.StartedAt, pending.ObservedAt, utilization, resetAt).Scan(&id)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (r *subscriptionCostRepository) CompletePending(ctx context.Context, pendingID, logID int64) error {
	if pendingID <= 0 || logID <= 0 {
		return nil
	}
	// Create returns the durable existing row ID for a deduplicated billing write.
	_, err := r.db.ExecContext(ctx, `UPDATE subscription_cost_pending_usage p SET usage_log_id=u.id FROM usage_logs u WHERE p.id=$1 AND u.id=$2 AND p.account_id=u.account_id AND p.identity_hash=u.account_cost_identity_hash`, pendingID, logID)
	return err
}

func (r *subscriptionCostRepository) CleanupPending(ctx context.Context, before time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx, `WITH expired AS (SELECT id FROM subscription_cost_pending_usage WHERE observed_at<$1 AND started_at<$1 ORDER BY observed_at,id LIMIT 10000)
 DELETE FROM subscription_cost_pending_usage p USING expired WHERE p.id=expired.id`, before)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
