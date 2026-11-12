package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func subscriptionCostTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := carpoolMonitorTestDB(t)
	_, err := db.Exec(`CREATE TABLE usage_logs(id bigserial PRIMARY KEY,account_id bigint,created_at timestamptz,total_cost numeric,account_stats_cost numeric,account_rate_multiplier numeric,account_cost_started_at timestamptz,account_cost_observed_at timestamptz,account_cost_identity_hash text,account_cost_pending_id bigint)`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("250_custom_subscription_cost.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE subscription_cost_migration_boundary SET started_at=now()+interval '1 day'`)
	require.NoError(t, err)
	return db
}

func TestSubscriptionCostPostgresTenMinuteLifecycle(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	slot := time.Now().UTC().Truncate(10 * time.Minute)
	reset := slot.Add(2 * time.Hour)
	_, err := db.Exec(`UPDATE subscription_cost_migration_boundary SET started_at=$1`, slot.Add(-time.Minute))
	require.NoError(t, err)
	insert := func(at time.Time, cost float64, oldMark bool) {
		t.Helper()
		var mark any
		if oldMark {
			mark = -1
		}
		_, err := db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost,account_cost_pending_id) VALUES(9,$1,$2,$3)`, at, cost, mark)
		require.NoError(t, err)
	}
	sample := func(slot, reset time.Time, pct float64) *service.SubscriptionCostSnapshot {
		t.Helper()
		// Use a new repository object to prove that slot and baseline survive
		// restart rather than relying on process memory.
		repo := NewSubscriptionCostRepository(db)
		got, err := repo.Sample(ctx, 9, "shared", service.CarpoolResetObservation{
			ObservedAt: slot.Add(time.Minute), ResetAt: reset, Utilization: pct,
		}, slot)
		require.NoError(t, err)
		require.NotNil(t, got)
		return got
	}

	insert(slot, 1400, true)
	first := sample(slot, reset, 90)
	require.InDelta(t, 1400.0/0.9, *first.Estimate.EstimatedTotalCost, 0.0001)
	require.False(t, first.Estimate.EstimateFrozen)
	insert(slot.Add(5*time.Minute), 130, false)
	sameSlot := sample(slot, reset, 100)
	require.Equal(t, first, sameSlot, "later requests must not trigger another calculation in the same slot")
	fullSlot := slot.Add(10 * time.Minute)
	late, err := NewSubscriptionCostRepository(db).Sample(ctx, 9, "shared", service.CarpoolResetObservation{
		ObservedAt: fullSlot, ResetAt: reset, Utilization: 103,
	}, slot)
	require.NoError(t, err)
	require.Nil(t, late, "an observation at the next ten-minute boundary cannot be stored in the previous slot")
	full := sample(fullSlot, reset, 103)
	require.Equal(t, 1530.0, *full.Estimate.EstimatedTotalCost)
	require.True(t, full.Estimate.EstimateFrozen)
	require.Zero(t, *full.Estimate.CreditsUsedCost)

	insert(fullSlot.Add(5*time.Minute), 70, false)
	later := sample(fullSlot.Add(10*time.Minute), reset, 105)
	require.Equal(t, 1530.0, *later.Estimate.EstimatedTotalCost)
	require.Equal(t, 70.0, *later.Estimate.CreditsUsedCost)
	persisted, err := NewSubscriptionCostRepository(db).Read(ctx, 9, slot.Add(21*time.Minute))
	require.NoError(t, err)
	require.Equal(t, later, persisted)

	insert(reset, 100, false)
	next := sample(reset, reset.Add(7*24*time.Hour), 10)
	require.Equal(t, 1000.0, *next.Estimate.EstimatedTotalCost)
	require.False(t, next.Estimate.EstimateFrozen)
	require.Zero(t, *next.Estimate.CreditsUsedCost)
}

func TestSubscriptionCostPostgresFailedWriteKeepsSnapshotAndNextSlotRecovers(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	repo := NewSubscriptionCostRepository(db)
	slot := time.Now().UTC().Truncate(10 * time.Minute)
	reset := slot.Add(2 * time.Hour)
	_, err := db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,80),(9,$2,20),(9,$3,5)`,
		slot, slot.Add(5*time.Minute), slot.Add(15*time.Minute))
	require.NoError(t, err)
	sample := func(slot time.Time, utilization float64) (*service.SubscriptionCostSnapshot, error) {
		return repo.Sample(ctx, 9, "shared", service.CarpoolResetObservation{
			ObservedAt: slot.Add(time.Minute), ResetAt: reset, Utilization: utilization,
		}, slot)
	}
	first, err := sample(slot, 80)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.Estimate)
	require.Equal(t, 100.0, *first.Estimate.EstimatedTotalCost)

	_, err = db.Exec(`CREATE FUNCTION reject_subscription_cost_update() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'simulated subscription cost write failure'; END; $$;
		CREATE TRIGGER reject_subscription_cost_update BEFORE UPDATE ON subscription_cost_states
		FOR EACH ROW EXECUTE FUNCTION reject_subscription_cost_update();`)
	require.NoError(t, err)
	failed, err := sample(slot.Add(10*time.Minute), 100)
	require.ErrorContains(t, err, "simulated subscription cost write failure")
	require.Nil(t, failed)
	persisted, err := repo.Read(ctx, 9, slot.Add(11*time.Minute))
	require.NoError(t, err)
	require.Equal(t, first, persisted)

	_, err = db.Exec(`DROP TRIGGER reject_subscription_cost_update ON subscription_cost_states`)
	require.NoError(t, err)
	recovered, err := sample(slot.Add(20*time.Minute), 100)
	require.NoError(t, err)
	require.NotNil(t, recovered)
	require.NotNil(t, recovered.Estimate)
	require.True(t, recovered.Estimate.EstimateFrozen)
	require.Equal(t, 105.0, *recovered.Estimate.EstimatedTotalCost)
	require.Zero(t, *recovered.Estimate.CreditsUsedCost)
}

func TestSubscriptionCostPostgresReadRejectsReboundIdentityAndReset(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	slot := time.Now().UTC().Truncate(10 * time.Minute)
	repo := NewSubscriptionCostRepository(db)
	_, err := db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,160)`, slot)
	require.NoError(t, err)
	obs := service.CarpoolResetObservation{ObservedAt: slot.Add(time.Minute), ResetAt: slot.Add(4 * time.Hour), Utilization: 20}
	got, err := repo.Sample(ctx, 9, "shared", obs, slot)
	require.NoError(t, err)
	require.NotNil(t, got)
	err = NewCarpoolResetMonitorRepository(db).RecordResetReceipt(ctx, 9, service.CarpoolResetIdentityHash("shared"), "manual", slot.Add(2*time.Minute), 1)
	require.NoError(t, err)
	got, err = repo.Read(ctx, 9, slot.Add(3*time.Minute))
	require.NoError(t, err)
	require.Nil(t, got)

	next := slot.Add(10 * time.Minute)
	obs.ObservedAt, obs.Utilization = next.Add(time.Minute), 5
	got, err = repo.Sample(ctx, 9, "shared", obs, next)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Nil(t, got.Estimate, "manual reset excludes the previous cycle even if reset_at did not change")
	_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"other"}' WHERE id=9`)
	require.NoError(t, err)
	got, err = repo.Read(ctx, 9, next.Add(2*time.Minute))
	require.NoError(t, err)
	require.Nil(t, got)
}
