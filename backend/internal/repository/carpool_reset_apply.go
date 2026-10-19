package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *carpoolResetMonitorRepository) ApplyEvent(ctx context.Context, eventID int64, now, dailyStart time.Time) (*service.CarpoolResetEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.beginSourceGuardedTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Match voting's table-before-monitor order. Without this reservation a
	// status poll can hold SHARE while waiting for the monitor we already own.
	if _, err = tx.ExecContext(ctx, `LOCK TABLE user_subscriptions IN ROW EXCLUSIVE MODE NOWAIT`); err != nil {
		return nil, err
	}
	// Use the same lock order as observation/cancellation.
	var monitorID int
	if err = tx.QueryRowContext(ctx, `SELECT id FROM carpool_reset_monitor WHERE id=1 FOR UPDATE`).Scan(&monitorID); err != nil {
		return nil, err
	}
	event := &service.CarpoolResetEvent{ID: eventID}
	var expected service.CarpoolResetSource
	var expires time.Time
	err = tx.QueryRowContext(ctx, `SELECT status,reason,group_id,account_id,identity_hash,expires_at,reset_count,skipped_count,source_revision
		FROM carpool_subscription_reset_events WHERE id=$1 FOR UPDATE`, eventID).
		Scan(&event.Status, &event.Reason, &expected.GroupID, &expected.AccountID, &expected.IdentityHash, &expires, &event.ResetCount, &event.SkippedCount, &expected.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if event.Status != "pending" {
		return event, nil
	}
	// Lock target rows before reading the clock: a renewal, revocation, or slow
	// writer must not let a pre-lock timestamp bypass eligibility or event expiry.
	var locked int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM (
		SELECT s.id FROM user_subscriptions s JOIN carpool_subscription_reset_targets t ON t.subscription_id=s.id
		WHERE t.event_id=$1 ORDER BY s.id FOR UPDATE OF s
	) locked`, eventID).Scan(&locked)
	if err != nil {
		return nil, err
	}
	var effectiveNow time.Time
	if err = tx.QueryRowContext(ctx, `SELECT GREATEST($1::timestamptz,clock_timestamp())`, now).Scan(&effectiveNow); err != nil {
		return nil, err
	}
	if effectiveNow.After(now) {
		now = effectiveNow
		dailyStart = timezone.StartOfDay(now)
	}
	current, err := carpoolResetSource(ctx, tx)
	if err != nil {
		return nil, err
	}
	if event.Reason != "vote" && (!sameCarpoolResetSource(current, &expected) || !expires.After(now)) {
		why := "source_changed"
		if !expires.After(now) {
			why = "expired"
		}
		if _, err = tx.ExecContext(ctx, `UPDATE carpool_subscription_reset_events SET status='cancelled',cancel_reason=$2,completed_at=$3 WHERE id=$1`, eventID, why, now); err != nil {
			return nil, err
		}
		event.Status = "cancelled"
	} else {
		// Same fields/window anchors as AdminResetQuota, but the entire immutable
		// event target set and completion marker commit together. No cap is written.
		result, err := tx.ExecContext(ctx, `WITH eligible AS MATERIALIZED (
			SELECT s.id FROM carpool_subscription_reset_targets t JOIN user_subscriptions s ON s.id=t.subscription_id
			WHERE t.event_id=$1 AND s.user_id=t.user_id AND s.group_id=t.group_id AND s.starts_at=t.starts_at
			AND s.deleted_at IS NULL AND s.status='active' AND s.expires_at>$2 ORDER BY s.id FOR UPDATE OF s
		), changed AS (
			UPDATE user_subscriptions s SET daily_usage_usd=0,weekly_usage_usd=0,monthly_usage_usd=0,
			daily_window_start=$3,weekly_window_start=$2,monthly_window_start=$2,updated_at=$2
			FROM eligible e WHERE s.id=e.id RETURNING s.id
		) UPDATE carpool_subscription_reset_targets SET reset_applied=TRUE
		WHERE event_id=$1 AND subscription_id IN (SELECT id FROM changed)`, eventID, now, dailyStart)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		event.ResetCount = int(n)
		var total int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM carpool_subscription_reset_targets WHERE event_id=$1`, eventID).Scan(&total); err != nil {
			return nil, err
		}
		event.SkippedCount = total - event.ResetCount
		if _, err = tx.ExecContext(ctx, `UPDATE carpool_subscription_reset_events SET status='completed',reset_count=$2,skipped_count=$3,completed_at=$4 WHERE id=$1`, eventID, event.ResetCount, event.SkippedCount, now); err != nil {
			return nil, err
		}
		event.Status = "completed"
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return event, nil
}

func (r *carpoolResetMonitorRepository) PendingInvalidations(ctx context.Context, limit int) ([]service.CarpoolResetInvalidation, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `SELECT t.event_id,t.subscription_id,t.user_id,t.group_id
		FROM carpool_subscription_reset_targets t JOIN carpool_subscription_reset_events e ON e.id=t.event_id
		WHERE e.status='completed' AND t.reset_applied AND t.cache_invalidated_at IS NULL ORDER BY t.event_id,t.subscription_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []service.CarpoolResetInvalidation
	for rows.Next() {
		var item service.CarpoolResetInvalidation
		if err := rows.Scan(&item.EventID, &item.SubscriptionID, &item.UserID, &item.GroupID); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *carpoolResetMonitorRepository) CompleteInvalidation(ctx context.Context, eventID, subscriptionID int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE carpool_subscription_reset_targets SET cache_invalidated_at=NOW() WHERE event_id=$1 AND subscription_id=$2 AND reset_applied`, eventID, subscriptionID)
	return err
}
