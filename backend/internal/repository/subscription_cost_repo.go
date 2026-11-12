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
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM accounts WHERE id>$1 AND deleted_at IS NULL AND platform='openai' AND type='oauth' AND parent_account_id IS NULL AND status='active' AND (NOT auto_pause_on_expired OR expires_at IS NULL OR expires_at>now()) AND btrim(COALESCE(credentials->>'chatgpt_account_id',''))<>'' ORDER BY id LIMIT $2`, after, limit)
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

// Sample serializes the small account snapshot update across replicas. The
// duplicate-slot check runs before aggregating any usage logs.
func (r *subscriptionCostRepository) Sample(ctx context.Context, accountID int64, identity string, observation service.CarpoolResetObservation, slot time.Time) (*service.SubscriptionCostSnapshot, error) {
	if accountID <= 0 || slot.IsZero() || observation.ObservedAt.Before(slot) || !observation.ObservedAt.Before(slot.Add(service.SubscriptionCostSampleInterval)) || strings.TrimSpace(identity) == "" || observation.ObservedAt.IsZero() || !observation.ResetAt.After(observation.ObservedAt) || math.IsNaN(observation.Utilization) || math.IsInf(observation.Utilization, 0) || observation.Utilization < 0 {
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
	err = tx.QueryRowContext(ctx, `SELECT btrim(COALESCE(credentials->>'chatgpt_account_id','')) FROM accounts WHERE id=$1 AND deleted_at IS NULL AND platform='openai' AND type='oauth' AND parent_account_id IS NULL AND status='active' FOR SHARE`, accountID).Scan(&current)
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

	if receiptPtr == nil || !receiptPtr.OccurredAt.After(state.Observation.ObservedAt) {
		if !state.SampleSlot.Before(slot) {
			if state.SampleSlot.Equal(slot) && state.Observation.ResetAt.After(observation.ObservedAt) &&
				state.Observation.ResetAt.Sub(observation.ResetAt).Abs() <= time.Minute {
				return subscriptionCostSnapshot(state), nil
			}
			return nil, nil
		}
	} else if !state.SampleSlot.Before(slot) {
		// A reset invalidates this slot. Do not reuse its old baseline or
		// calculate twice; the next scheduled sample starts the new cycle.
		return nil, nil
	}

	next, ok := service.AdvanceSubscriptionCostCycle(state, observation, receiptPtr, identityStart)
	if !ok {
		return nil, nil
	}
	next.SampleSlot = slot.UTC()
	next.Estimate = nil
	if !next.PendingReset {
		var cost float64
		var invalid int64
		// New rows have the official schema/insert path. Existing attribution
		// remains useful for excluding old-identity and pre-reset usage; the
		// obsolete per-request registration marker never gates this estimate.
		err = tx.QueryRowContext(ctx, `SELECT
 COALESCE(SUM(COALESCE(account_stats_cost,total_cost)*COALESCE(account_rate_multiplier,1)),0),
 COUNT(*) FILTER (WHERE COALESCE(account_stats_cost,total_cost) IS NULL
   OR COALESCE(account_stats_cost,total_cost)<0 OR COALESCE(account_rate_multiplier,1)<0)
 FROM usage_logs WHERE account_id=$1 AND created_at>=$2 AND created_at<=$3
   AND COALESCE(account_cost_started_at,created_at)>=$2
   AND (account_cost_identity_hash IS NULL OR account_cost_identity_hash='' OR account_cost_identity_hash=$4)`,
			accountID, next.CycleStart, observation.ObservedAt, hash).Scan(&cost, &invalid)
		if err != nil {
			return nil, err
		}
		if invalid == 0 {
			next.Estimate = service.UpdateSubscriptionCostEstimate(&next, observation.Utilization, cost)
		}
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
	return subscriptionCostSnapshot(next), nil
}

func subscriptionCostSnapshot(state service.SubscriptionCostState) *service.SubscriptionCostSnapshot {
	return &service.SubscriptionCostSnapshot{Observation: state.Observation, Slot: state.SampleSlot, Estimate: state.Estimate}
}

func (r *subscriptionCostRepository) Read(ctx context.Context, accountID int64, now time.Time) (*service.SubscriptionCostSnapshot, error) {
	var encoded []byte
	err := r.db.QueryRowContext(ctx, `SELECT s.state FROM subscription_cost_states s
 JOIN accounts a ON a.id=s.account_id AND a.platform='openai' AND a.type='oauth'
   AND a.parent_account_id IS NULL AND a.deleted_at IS NULL
 LEFT JOIN subscription_cost_account_epochs e ON e.account_id=s.account_id
 WHERE s.account_id=$1 AND s.identity_revision=COALESCE(e.revision,0)
   AND s.identity_hash=encode(sha256(convert_to(btrim(a.credentials->>'chatgpt_account_id'),'UTF8')),'hex')
   AND NOT EXISTS (SELECT 1 FROM carpool_account_reset_receipts r WHERE r.account_id=s.account_id
     AND r.identity_hash=s.identity_hash AND COALESCE(r.cost_reset_at,r.occurred_at)>(s.state#>>'{observation,observed_at}')::timestamptz)
   AND NOT EXISTS (SELECT 1 FROM quota_reset_operations o WHERE o.account_id=s.account_id
     AND o.identity_hash=s.identity_hash AND o.status='succeeded' AND o.result->>'code'='ok'
     AND (o.result->>'windows_reset')::int>0 AND o.completed_at>(s.state#>>'{observation,observed_at}')::timestamptz)`, accountID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state service.SubscriptionCostState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return nil, err
	}
	if state.SampleSlot.IsZero() || state.PendingReset || !state.Observation.ResetAt.After(now) {
		return nil, nil
	}
	return subscriptionCostSnapshot(state), nil
}
