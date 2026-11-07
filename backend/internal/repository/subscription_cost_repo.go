package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type subscriptionCostRepository struct{ db *sql.DB }

func NewSubscriptionCostRepository(db *sql.DB) service.SubscriptionCostRepository {
	return &subscriptionCostRepository{db: db}
}

func (r *subscriptionCostRepository) Candidates(ctx context.Context, after int64, limit int) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM accounts WHERE id>$1 AND deleted_at IS NULL AND platform='openai' AND type='oauth' AND parent_account_id IS NULL AND status='active' AND btrim(COALESCE(credentials->>'chatgpt_account_id',''))<>'' ORDER BY id LIMIT $2`, after, limit)
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

func (r *subscriptionCostRepository) Observe(ctx context.Context, accountID int64, identity string, observation service.CarpoolResetObservation) (*service.SubscriptionCostEstimate, error) {
	return r.observe(ctx, accountID, identity, observation, true)
}
func (r *subscriptionCostRepository) Track(ctx context.Context, accountID int64, identity string, observation service.CarpoolResetObservation) error {
	_, err := r.observe(ctx, accountID, identity, observation, false)
	return err
}
func (r *subscriptionCostRepository) observe(ctx context.Context, accountID int64, identity string, observation service.CarpoolResetObservation, projection bool) (*service.SubscriptionCostEstimate, error) {
	if accountID <= 0 || strings.TrimSpace(identity) == "" || observation.ObservedAt.IsZero() || !observation.ResetAt.After(observation.ObservedAt) || math.IsNaN(observation.Utilization) || math.IsInf(observation.Utilization, 0) || observation.Utilization < 0 {
		return nil, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var current string
	// Lock credentials until the observation commits. A stale request cannot
	// revive a former identity, including an account switched away and back.
	err = tx.QueryRowContext(ctx, `SELECT btrim(COALESCE(credentials->>'chatgpt_account_id','')) FROM accounts WHERE id=$1 AND deleted_at IS NULL AND platform='openai' AND type='oauth' AND parent_account_id IS NULL FOR SHARE`, accountID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if current != strings.TrimSpace(identity) {
		return nil, nil
	}
	var revision int64
	var identityStart time.Time
	err = tx.QueryRowContext(ctx, `SELECT revision,identity_started_at FROM subscription_cost_account_epochs WHERE account_id=$1`, accountID).Scan(&revision, &identityStart)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if !identityStart.IsZero() && observation.ObservedAt.Before(identityStart) {
		return nil, nil
	}
	hash := service.CarpoolResetIdentityHash(identity)
	_, err = tx.ExecContext(ctx, `INSERT INTO subscription_cost_states(account_id,identity_hash,identity_revision) VALUES($1,$2,$3) ON CONFLICT(account_id) DO NOTHING`, accountID, hash, revision)
	if err != nil {
		return nil, err
	}
	var storedHash string
	var storedRevision int64
	var encoded []byte
	err = tx.QueryRowContext(ctx, `SELECT identity_hash,identity_revision,state FROM subscription_cost_states WHERE account_id=$1 FOR UPDATE`, accountID).Scan(&storedHash, &storedRevision, &encoded)
	if err != nil {
		return nil, err
	}
	state := service.SubscriptionCostState{}
	if storedHash == hash && storedRevision == revision {
		if err = json.Unmarshal(encoded, &state); err != nil {
			return nil, err
		}
	}
	var receipt service.SubscriptionCostReset
	var receiptPtr *service.SubscriptionCostReset
	err = tx.QueryRowContext(ctx, `SELECT reset_key,reset_at FROM (
 SELECT request_hash AS reset_key,COALESCE(cost_reset_at,occurred_at) AS reset_at FROM carpool_account_reset_receipts WHERE account_id=$1 AND identity_hash=$2
 UNION ALL
 SELECT encode(sha256(convert_to(o.request_id,'UTF8')),'hex'),o.completed_at FROM quota_reset_operations o
 WHERE o.account_id=$1 AND o.identity_hash=$2 AND o.status='succeeded' AND o.result->>'code'='ok' AND (o.result->>'windows_reset')::int>0
 AND NOT EXISTS(SELECT 1 FROM carpool_account_reset_receipts r WHERE r.account_id=o.account_id AND r.identity_hash=o.identity_hash AND r.request_hash=encode(sha256(convert_to(o.request_id,'UTF8')),'hex'))
 ) resets ORDER BY reset_at DESC LIMIT 1`, accountID, hash).Scan(&receipt.Key, &receipt.OccurredAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		receiptPtr = &receipt
	}
	// Never apply a snapshot captured before the latest confirmed consume.
	if receiptPtr != nil && receipt.OccurredAt.After(observation.ObservedAt) {
		return nil, nil
	}

	next, ok := service.AdvanceSubscriptionCostCycle(state, observation, receiptPtr, identityStart)
	if !ok {
		// An earlier completed request may settle after the first-full observer.
		// Its durable row can unblock the stored cutoff without moving the watermark.
		if state.FirstFullAt == nil || state.FrozenBase != nil || observation.ResetAt.Before(state.Observation.ResetAt.Add(-time.Minute)) || observation.ResetAt.After(state.Observation.ResetAt.Add(time.Minute)) || (receiptPtr != nil && receiptPtr.Key != state.ResetKey) {
			return nil, nil
		}
		next = state
	}
	evidence := observation.CostEvidenceAt
	if !evidence.IsZero() && evidence.Before(next.CycleStart) {
		return nil, nil
	}
	if !next.PendingReset && next.FrozenBase == nil && next.FirstFullAt == nil && observation.Utilization >= 100 {
		cutoff := observation.ObservedAt
		next.FirstFullAt = &cutoff
	}
	cutoff := observation.ObservedAt
	if next.FirstFullAt != nil {
		cutoff = *next.FirstFullAt
	}

	if !next.PendingReset && next.FrozenBase == nil && (projection || next.FirstFullAt != nil) {
		var earlierFull sql.NullTime
		err = tx.QueryRowContext(ctx, `SELECT min(observed_at) FROM subscription_cost_pending_usage WHERE account_id=$1 AND identity_hash=$2 AND started_at>=$3 AND observed_at<=$4 AND utilization>=100 AND reset_at BETWEEN $5::timestamptz-interval '1 minute' AND $5::timestamptz+interval '1 minute'`, accountID, hash, next.CycleStart, cutoff, next.Observation.ResetAt).Scan(&earlierFull)
		if err != nil {
			return nil, err
		}
		if earlierFull.Valid && (next.FirstFullAt == nil || earlierFull.Time.Before(*next.FirstFullAt)) {
			cutoff = earlierFull.Time
			next.FirstFullAt = &cutoff
		}
	}
	blocked := false
	if !next.PendingReset && next.FrozenBase == nil && (projection || next.FirstFullAt != nil) {
		var unresolved int64
		var earlierBelow bool
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE NOT EXISTS(
   SELECT 1 FROM usage_logs u WHERE u.account_id=p.account_id AND u.account_cost_identity_hash=p.identity_hash AND (u.account_cost_pending_id=p.id OR u.id=p.usage_log_id))),
   COALESCE(bool_or(p.utilization<100 AND p.reset_at BETWEEN $5::timestamptz-interval '1 minute' AND $5::timestamptz+interval '1 minute'),false)
   FROM subscription_cost_pending_usage p WHERE p.account_id=$1 AND p.identity_hash=$2 AND p.started_at>=$3 AND p.observed_at<=$4`, accountID, hash, next.CycleStart, cutoff, next.Observation.ResetAt).Scan(&unresolved, &earlierBelow)
		if err != nil {
			return nil, err
		}
		blocked = unresolved > 0
		if earlierBelow {
			next.SeenBelow = true
		}
	}
	var estimate *service.SubscriptionCostEstimate
	// Successful traffic only needs the aggregate at the first full observation.
	// Below exhaustion and after freeze, API/background projections own the sum.
	if !blocked && (projection || (!next.PendingReset && next.FrozenBase == nil && next.FirstFullAt != nil)) {
		var cost, observationCost float64
		var invalid int64

		err = tx.QueryRowContext(ctx, `WITH costs AS (
 SELECT COALESCE(account_stats_cost,total_cost)*COALESCE(account_rate_multiplier,1) AS cost,
 COALESCE(account_cost_observed_at,created_at) AS observed_at,
 (COALESCE(account_stats_cost,total_cost) IS NULL OR COALESCE(account_stats_cost,total_cost)<0 OR COALESCE(account_rate_multiplier,1)<0
 OR (COALESCE(account_stats_cost,total_cost)*COALESCE(account_rate_multiplier,1)<>0 AND created_at >= (SELECT started_at FROM subscription_cost_migration_boundary WHERE id=1) AND (account_cost_started_at IS NULL OR account_cost_observed_at IS NULL OR account_cost_identity_hash IS NULL OR account_cost_identity_hash='' OR account_cost_pending_id<0))
 OR account_cost_observed_at<account_cost_started_at) AS invalid
 FROM usage_logs WHERE account_id=$1 AND created_at>=$2 AND COALESCE(account_cost_started_at,created_at)>=$2
 AND (account_cost_identity_hash IS NULL OR account_cost_identity_hash='' OR account_cost_identity_hash=$4)
 ) SELECT COALESCE(SUM(cost),0),COALESCE(SUM(cost) FILTER (WHERE observed_at<=$3),0),COUNT(*) FILTER (WHERE invalid) FROM costs`, accountID, next.CycleStart, cutoff, hash).Scan(&cost, &observationCost, &invalid)
		if err != nil {
			return nil, err
		}
		if invalid > 0 || math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
			return nil, nil
		}
		utilization := observation.Utilization
		if next.FirstFullAt != nil {
			utilization = 100
		}
		estimate = service.UpdateSubscriptionCostEstimate(&next, utilization, observationCost)
		if estimate != nil && estimate.EstimateFrozen {
			credits := math.Max(0, cost-*estimate.EstimatedTotalCost)
			estimate.CreditsUsedCost = &credits
		}
	} else if !next.PendingReset && observation.Utilization < 100 && (next.FirstFullAt == nil || !observation.ObservedAt.After(*next.FirstFullAt)) {
		next.SeenBelow = true
	}

	encoded, err = json.Marshal(next)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE subscription_cost_states SET identity_hash=$2,identity_revision=$3,state=$4::jsonb,updated_at=clock_timestamp() WHERE account_id=$1`, accountID, hash, revision, string(encoded))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return estimate, nil
}
