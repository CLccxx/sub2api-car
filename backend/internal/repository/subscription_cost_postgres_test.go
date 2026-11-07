package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
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

func TestSubscriptionCostPostgresFreezeRestartConcurrent(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	repo := NewSubscriptionCostRepository(db)
	now := time.Now().UTC().Truncate(time.Second)
	obs := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 90}
	_, err := db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,1800)`, now.Add(-time.Minute))
	require.NoError(t, err)
	estimate, err := repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 2000.0, *estimate.EstimatedTotalCost)
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,200)`, now)
	require.NoError(t, err)
	obs.ObservedAt = now.Add(time.Second)
	obs.Utilization = 100
	pool := carpoolMonitorConcurrentPool(t, db)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := NewSubscriptionCostRepository(pool).Observe(ctx, 9, "shared", obs)
			require.NoError(t, e)
		}()
	}
	wg.Wait()
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,200)`, now.Add(time.Second))
	require.NoError(t, err)
	obs.ObservedAt = now.Add(2 * time.Second)
	estimate, err = NewSubscriptionCostRepository(db).Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 2000.0, *estimate.EstimatedTotalCost)
	require.Equal(t, 200.0, *estimate.CreditsUsedCost)
	_, err = db.Exec(`UPDATE groups SET carpool_members=5 WHERE id=10;DELETE FROM account_groups WHERE group_id=20`)
	require.NoError(t, err)
	estimate, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 2000.0, *estimate.EstimatedTotalCost)
	obs.ObservedAt = now.Add(-time.Second)
	estimate, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Nil(t, estimate)
}

func TestSubscriptionCostPostgresResetAndIdentityExcludesHistory(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	repo := NewSubscriptionCostRepository(db)
	now := time.Now().UTC().Truncate(time.Second)
	_, err := db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,1800)`, now.Add(-time.Hour))
	require.NoError(t, err)
	obs := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 100}
	estimate, err := repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 1600.0, *estimate.EstimatedTotalCost)
	resetTime := now.Add(time.Second)
	_, err = db.Exec(`INSERT INTO carpool_account_reset_receipts(account_id,identity_hash,request_hash,occurred_at,windows_reset) VALUES(9,$1,'reset',$2,1)`, service.CarpoolResetIdentityHash("shared"), resetTime)
	require.NoError(t, err)
	obs.ObservedAt = now.Add(2 * time.Second)
	estimate, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Nil(t, estimate)
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,100)`, resetTime.Add(time.Millisecond))
	require.NoError(t, err)
	obs.ObservedAt = now.Add(3 * time.Second)
	obs.Utilization = 10
	estimate, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 1000.0, *estimate.EstimatedTotalCost)
	obs.ObservedAt = now.Add(time.Hour)
	obs.ResetAt = obs.ObservedAt.Add(7 * 24 * time.Hour)
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,50)`, obs.ObservedAt)
	require.NoError(t, err)
	estimate, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 500.0, *estimate.EstimatedTotalCost)
	_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"new"}' WHERE id=9`)
	require.NoError(t, err)
	estimate, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Nil(t, estimate)
}

func TestSubscriptionCostPostgresPreciseResetAndOperationFallback(t *testing.T) {
	for _, evidence := range []string{"receipt", "operation"} {
		t.Run(evidence, func(t *testing.T) {
			db := subscriptionCostTestDB(t)
			ctx := context.Background()
			repo := NewSubscriptionCostRepository(db)
			now := time.Now().UTC().Truncate(time.Second)
			_, err := db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,1800)`, now.Add(-time.Hour))
			require.NoError(t, err)
			obs := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 100}
			estimate, err := repo.Observe(ctx, 9, "shared", obs)
			require.NoError(t, err)
			require.NotNil(t, estimate)
			cutoff := now.Add(500 * time.Millisecond)
			if evidence == "receipt" {
				require.NoError(t, NewCarpoolResetMonitorRepository(db).RecordResetReceipt(ctx, 9, service.CarpoolResetIdentityHash("shared"), service.CarpoolResetIdentityHash("card"), cutoff, 1))
			} else {
				_, err = db.Exec(`INSERT INTO quota_reset_operations(account_id,request_id,identity_hash,status,result,completed_at) VALUES(9,'card',$1,'succeeded','{"code":"ok","windows_reset":1}',$2)`, service.CarpoolResetIdentityHash("shared"), cutoff)
				require.NoError(t, err)
			}
			_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,1000),(9,$2,20)`, now.Add(100*time.Millisecond), now.Add(800*time.Millisecond))
			require.NoError(t, err)
			estimate, err = repo.Observe(ctx, 9, "shared", obs)
			require.NoError(t, err)
			require.Nil(t, estimate, "pre-reset samples cannot return the previous frozen allowance")
			obs.ObservedAt = now.Add(time.Second)
			obs.Utilization = 10
			estimate, err = repo.Observe(ctx, 9, "shared", obs)
			require.NoError(t, err)
			require.Equal(t, 200.0, *estimate.EstimatedTotalCost)
			obs.ObservedAt = now.Add(2 * time.Second)
			obs.Utilization = 100
			estimate, err = repo.Observe(ctx, 9, "shared", obs)
			require.NoError(t, err)
			require.Equal(t, 20.0, *estimate.EstimatedTotalCost)
			if evidence == "operation" {
				require.NoError(t, NewCarpoolResetMonitorRepository(db).RecordResetReceipt(ctx, 9, service.CarpoolResetIdentityHash("shared"), service.CarpoolResetIdentityHash("card"), cutoff.Add(time.Millisecond), 1))
				obs.ObservedAt = now.Add(3 * time.Second)
				estimate, err = repo.Observe(ctx, 9, "shared", obs)
				require.NoError(t, err)
				require.Equal(t, 20.0, *estimate.EstimatedTotalCost, "late receipt for the same consume must not clear the new freeze")
			}
		})
	}
}

func TestSubscriptionCostPostgresTwoAccountsAndIdentityRoundTrip(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	repo := NewSubscriptionCostRepository(db)
	now := time.Now().UTC().Truncate(time.Second)
	_, err := db.Exec(`INSERT INTO accounts(id,platform,type,status,credentials) VALUES(8,'openai','oauth','active','{"chatgpt_account_id":"second"}')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost,account_stats_cost,account_rate_multiplier) VALUES(9,$1,9999,1800,1),(8,$1,9999,250,2)`, now.Add(-time.Minute))
	require.NoError(t, err)
	obs := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 50}
	first, err := repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	second, err := repo.Observe(ctx, 8, "second", obs)
	require.NoError(t, err)
	require.Equal(t, 3600.0, *first.EstimatedTotalCost)
	require.Equal(t, 1000.0, *second.EstimatedTotalCost)
	_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"other"}' WHERE id=9;UPDATE accounts SET credentials='{"chatgpt_account_id":"shared"}' WHERE id=9`)
	require.NoError(t, err)
	first, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Nil(t, first, "round-trip credentials invalidate old samples")
	var cutoff time.Time
	require.NoError(t, db.QueryRow(`SELECT identity_started_at FROM subscription_cost_account_epochs WHERE account_id=9`).Scan(&cutoff))
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,50)`, cutoff.Add(time.Millisecond))
	require.NoError(t, err)
	obs.ObservedAt = cutoff.Add(time.Second)
	first, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 100.0, *first.EstimatedTotalCost, "old-identity costs must be excluded")
	second, err = repo.Observe(ctx, 8, "second", obs)
	require.NoError(t, err)
	require.Equal(t, 1000.0, *second.EstimatedTotalCost)
}

func TestSubscriptionCostPostgresFreezeSeparatesObservationAndLiveCost(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	repo := NewSubscriptionCostRepository(db)
	now := time.Now().UTC().Truncate(time.Second)
	obs := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 90}
	_, err := db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,1800)`, now.Add(-time.Minute))
	require.NoError(t, err)
	_, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	// Both rows land before the delayed observer runs. Nanosecond ordering keeps
	// the first full observation's own request while excluding later credit spend.
	fullAt := now.Add(200 * time.Millisecond)
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,200),(9,$2,300)`, fullAt, fullAt.Add(200*time.Millisecond))
	require.NoError(t, err)
	obs.ObservedAt = fullAt
	obs.Utilization = 100
	estimate, err := repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 2000.0, *estimate.EstimatedTotalCost)
	require.Equal(t, 300.0, *estimate.CreditsUsedCost)
}

func TestSubscriptionCostPostgresLateOldIdentityAndCycleCostsStayExcluded(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	repo := NewSubscriptionCostRepository(db)
	now := time.Now().UTC().Truncate(time.Second)
	_, err := db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost,account_cost_started_at,account_cost_observed_at,account_cost_identity_hash) VALUES
 (9,$1,1800,$2,$2,$3)`, now, now.Add(-time.Hour), service.CarpoolResetIdentityHash("shared"))
	require.NoError(t, err)
	obs := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 100}
	_, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	cutoff := now.Add(time.Second)
	require.NoError(t, NewCarpoolResetMonitorRepository(db).RecordResetReceipt(ctx, 9, service.CarpoolResetIdentityHash("shared"), "new-cycle", cutoff, 1))
	// Recorded late; actual request/response belong to the preceding cycle.
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost,account_cost_started_at,account_cost_observed_at,account_cost_identity_hash) VALUES
 (9,$1,900,$2,$3,$4),(9,$1,50,$5,$5,$4)`, now.Add(3*time.Second), now.Add(-time.Minute), now.Add(-time.Second), service.CarpoolResetIdentityHash("shared"), now.Add(2*time.Second))
	require.NoError(t, err)
	obs.ObservedAt = now.Add(4 * time.Second)
	obs.Utilization = 10
	estimate, err := repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 500.0, *estimate.EstimatedTotalCost)
	// Switching away and back cannot admit a late request started in old A.
	_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"B"}' WHERE id=9;UPDATE accounts SET credentials='{"chatgpt_account_id":"shared"}' WHERE id=9`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE subscription_cost_account_epochs SET identity_started_at=$1 WHERE account_id=9`, now.Add(5*time.Second))
	require.NoError(t, err)
	var identityStart time.Time
	require.NoError(t, db.QueryRow(`SELECT identity_started_at FROM subscription_cost_account_epochs WHERE account_id=9`).Scan(&identityStart))
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost,account_cost_started_at,account_cost_observed_at,account_cost_identity_hash) VALUES
 (9,$1,900,$2,$1,$3),(9,$1,20,$1,$1,$3)`, identityStart.Add(time.Minute), identityStart.Add(-time.Minute), service.CarpoolResetIdentityHash("shared"))
	require.NoError(t, err)
	// Ignore the earlier synthetic future receipt to isolate the identity cutoff.
	_, err = db.Exec(`DELETE FROM carpool_account_reset_receipts`)
	require.NoError(t, err)
	obs.ObservedAt = identityStart.Add(2 * time.Minute)
	estimate, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 200.0, *estimate.EstimatedTotalCost)
}

func TestSubscriptionCostPostgresTrafficAvoidsAggregateExceptFreeze(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	repo := NewSubscriptionCostRepository(db)
	now := time.Now().UTC().Truncate(time.Second)
	obs := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 90}
	// Removing the cost table proves lower and already-frozen traffic never sum it.
	_, err := db.Exec(`ALTER TABLE usage_logs RENAME TO saved_usage_logs`)
	require.NoError(t, err)
	require.NoError(t, repo.Track(ctx, 9, "shared", obs))
	_, err = db.Exec(`ALTER TABLE saved_usage_logs RENAME TO usage_logs;INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,now()-interval '1 minute',2000)`)
	require.NoError(t, err)
	obs.ObservedAt = now.Add(time.Second)
	obs.Utilization = 100
	require.NoError(t, repo.Track(ctx, 9, "shared", obs))
	_, err = db.Exec(`ALTER TABLE usage_logs RENAME TO saved_usage_logs`)
	require.NoError(t, err)
	obs.ObservedAt = now.Add(2 * time.Second)
	require.NoError(t, repo.Track(ctx, 9, "shared", obs))
}

func TestSubscriptionCostPostgresMissingAttributionFailsClosedExceptZeroLiveUsage(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	repo := NewSubscriptionCostRepository(db)
	now := time.Now().UTC().Truncate(time.Second)
	_, err := db.Exec(`UPDATE subscription_cost_migration_boundary SET started_at=$1`, now.Add(-time.Minute))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,now(),0)`)
	require.NoError(t, err)
	obs := service.CarpoolResetObservation{ObservedAt: now.Add(time.Second), ResetAt: now.Add(time.Hour), Utilization: 100}
	estimate, err := repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 1600.0, *estimate.EstimatedTotalCost, "zero-cost Live rows need no attribution")
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,now(),200)`)
	require.NoError(t, err)
	estimate, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Nil(t, estimate, "positive cost without attribution must not affect quotas")
}

func TestSubscriptionCostPostgresPendingAccountingCutoffSurvivesRestart(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	repo := NewSubscriptionCostRepository(db)
	t0 := time.Now().UTC().Truncate(time.Second)
	reset := t0.Add(time.Hour)
	identity := service.CarpoolResetIdentityHash("shared")
	register := func(observed time.Time, percent float64) int64 {
		obs := &service.CarpoolResetObservation{ObservedAt: observed, ResetAt: reset, Utilization: percent}
		id, err := repo.RegisterPending(ctx, service.SubscriptionCostPendingUsage{AccountID: 9, Identity: "shared", StartedAt: observed.Add(-time.Millisecond), ObservedAt: observed, Observation: obs})
		require.NoError(t, err)
		return id
	}
	persist := func(id int64, observed time.Time, cost float64) {
		_, err := db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost,account_cost_started_at,account_cost_observed_at,account_cost_identity_hash,account_cost_pending_id) VALUES(9,$1,$2,$3,$4,$5,$6)`, t0.Add(time.Minute), cost, observed.Add(-time.Millisecond), observed, identity, id)
		require.NoError(t, err)
	}
	aTime, bTime, cTime := t0.Add(time.Second), t0.Add(2*time.Second), t0.Add(3*time.Second)
	a := register(aTime, 90)
	b := register(bTime, 100)
	persist(b, bTime, 1800)
	// A later sampler wins before B's own callback: registration's first100 is authoritative.
	observe := service.CarpoolResetObservation{ObservedAt: cTime, ResetAt: reset, Utilization: 100}
	estimate, err := repo.Observe(ctx, 9, "shared", observe)
	require.NoError(t, err)
	require.Nil(t, estimate, "A must settle before any baseline is frozen")
	c := register(cTime, 100)
	persist(c, cTime, 300)
	restarted := NewSubscriptionCostRepository(db)
	observe.ObservedAt = cTime.Add(time.Second)
	estimate, err = restarted.Observe(ctx, 9, "shared", observe)
	require.NoError(t, err)
	require.Nil(t, estimate)
	var state service.SubscriptionCostState
	var encoded []byte
	require.NoError(t, db.QueryRow(`SELECT state FROM subscription_cost_states WHERE account_id=9`).Scan(&encoded))
	require.NoError(t, json.Unmarshal(encoded, &state))
	require.Equal(t, bTime, *state.FirstFullAt)
	persist(a, aTime, 200)
	// The earlier lower callback settles late; restart must preserve its evidence.
	require.NoError(t, restarted.Track(ctx, 9, "shared", service.CarpoolResetObservation{ObservedAt: aTime, ResetAt: reset, Utilization: 90}))
	estimate, err = restarted.Observe(ctx, 9, "shared", observe)
	require.NoError(t, err)
	require.Equal(t, 2000.0, *estimate.EstimatedTotalCost)
	require.Equal(t, 300.0, *estimate.CreditsUsedCost)
	require.Equal(t, "observed", estimate.EstimateSource)
}

func TestSubscriptionCostPostgresUnresolvedPendingDoesNotCrossReset(t *testing.T) {
	db := subscriptionCostTestDB(t)
	ctx := context.Background()
	repo := NewSubscriptionCostRepository(db)
	now := time.Now().UTC().Truncate(time.Second)
	_, err := repo.RegisterPending(ctx, service.SubscriptionCostPendingUsage{AccountID: 9, Identity: "shared", StartedAt: now.Add(-time.Minute), ObservedAt: now, Observation: &service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 100}})
	require.NoError(t, err)
	obs := service.CarpoolResetObservation{ObservedAt: now.Add(time.Second), ResetAt: now.Add(time.Hour), Utilization: 100}
	estimate, err := repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Nil(t, estimate)
	cutoff := now.Add(2 * time.Second)
	require.NoError(t, NewCarpoolResetMonitorRepository(db).RecordResetReceipt(ctx, 9, service.CarpoolResetIdentityHash("shared"), "reset-after-crash", cutoff, 1))
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,$1,20)`, now.Add(3*time.Second))
	require.NoError(t, err)
	obs.ObservedAt = now.Add(4 * time.Second)
	obs.Utilization = 10
	estimate, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.Equal(t, 200.0, *estimate.EstimatedTotalCost)
	// Late pre-card full headers cannot freeze the fresh cycle.
	require.NoError(t, repo.Track(ctx, 9, "shared", service.CarpoolResetObservation{ObservedAt: now.Add(5 * time.Second), CostEvidenceAt: now.Add(-time.Minute), ResetAt: obs.ResetAt, Utilization: 100}))
	obs.ObservedAt = now.Add(6 * time.Second)
	estimate, err = repo.Observe(ctx, 9, "shared", obs)
	require.NoError(t, err)
	require.False(t, estimate.EstimateFrozen)
}

func TestSubscriptionCostPostgresPendingRetentionPreservesCurrentGaps(t *testing.T) {
	db := subscriptionCostTestDB(t)
	repo := NewSubscriptionCostRepository(db)
	now := time.Now().UTC()
	_, err := db.Exec(`INSERT INTO subscription_cost_pending_usage(account_id,identity_hash,started_at,observed_at) VALUES(9,'old',$1,$1),(9,'current',$2,$2)`, now.Add(-15*24*time.Hour), now.Add(-time.Hour))
	require.NoError(t, err)
	removed, err := repo.CleanupPending(context.Background(), now.Add(-14*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), removed)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM subscription_cost_pending_usage WHERE identity_hash='current' AND usage_log_id IS NULL`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestSubscriptionCostPostgresAtomicUsageAttributionAndDedupSettlement(t *testing.T) {
	db := officialToCarTestDB(t)
	ctx := context.Background()
	require.NoError(t, ApplyMigrations(ctx, db))
	_, err := db.Exec(`INSERT INTO users(id,email,password_hash) VALUES(99001,'cost@example.invalid','inert');
 INSERT INTO accounts(id,name,platform,type,credentials) VALUES(99002,'cost','openai','oauth','{"chatgpt_account_id":"cost-identity"}');
 INSERT INTO api_keys(id,user_id,key,name) VALUES(99003,99001,'sk-inert-cost-test','cost');`)
	require.NoError(t, err)
	now := time.Now().UTC()
	hash := service.CarpoolResetIdentityHash("cost-identity")
	costRepo := NewSubscriptionCostRepository(db)
	pending := service.SubscriptionCostPendingUsage{AccountID: 99002, Identity: "cost-identity", StartedAt: now.Add(-time.Second), ObservedAt: now}
	id, err := costRepo.RegisterPending(ctx, pending)
	require.NoError(t, err)
	log := &service.UsageLog{UserID: 99001, AccountID: 99002, APIKeyID: 99003, RequestID: "atomic-cost", Model: "gpt-5.1", TotalCost: 20, AccountCostStartedAt: &pending.StartedAt, AccountCostObservedAt: &pending.ObservedAt, AccountCostIdentityHash: &hash, AccountCostPendingID: &id, CreatedAt: now.Add(time.Second)}
	logs := &usageLogRepository{sql: db}
	inserted, err := logs.Create(ctx, log)
	require.NoError(t, err)
	require.True(t, inserted)
	var started, observed time.Time
	var storedHash string
	var storedID int64
	require.NoError(t, db.QueryRow(`SELECT account_cost_started_at,account_cost_observed_at,account_cost_identity_hash,account_cost_pending_id FROM usage_logs WHERE id=$1`, log.ID).Scan(&started, &observed, &storedHash, &storedID))
	require.WithinDuration(t, pending.StartedAt, started, time.Microsecond)
	require.WithinDuration(t, pending.ObservedAt, observed, time.Microsecond)
	require.Equal(t, hash, storedHash)
	require.Equal(t, id, storedID)
	// Retrying the same billing request gets the existing row and settles its new marker.
	retryID, err := costRepo.RegisterPending(ctx, pending)
	require.NoError(t, err)
	log.AccountCostPendingID = &retryID
	inserted, err = logs.Create(ctx, log)
	require.NoError(t, err)
	require.False(t, inserted)
	require.NoError(t, costRepo.CompletePending(ctx, retryID, log.ID))
	var linked int64
	require.NoError(t, db.QueryRow(`SELECT usage_log_id FROM subscription_cost_pending_usage WHERE id=$1`, retryID).Scan(&linked))
	require.Equal(t, log.ID, linked)
}

func TestSubscriptionCostPostgresRegistrationCommitFencesFreeze(t *testing.T) {
	db := subscriptionCostTestDB(t)
	pool := carpoolMonitorConcurrentPool(t, db)
	repo := NewSubscriptionCostRepository(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(time.Hour)
	key := time.Now().UnixNano() & 0x7fffffff
	_, err := db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost) VALUES(9,now()-interval '1 minute',1800)`)
	require.NoError(t, err)
	_, err = db.Exec(fmt.Sprintf(`CREATE FUNCTION block_subscription_cost_registration() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(%d);RETURN NEW;END;$$;
 CREATE TRIGGER block_subscription_cost_registration BEFORE INSERT ON subscription_cost_pending_usage FOR EACH ROW EXECUTE FUNCTION block_subscription_cost_registration()`, key))
	require.NoError(t, err)
	blocker, err := pool.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.Exec(`SELECT pg_advisory_xact_lock($1)`, key)
	require.NoError(t, err)
	type registration struct {
		id  int64
		err error
	}
	registered := make(chan registration, 1)
	go func() {
		id, err := repo.RegisterPending(ctx, service.SubscriptionCostPendingUsage{AccountID: 9, Identity: "shared", StartedAt: now.Add(-time.Second), ObservedAt: now, Observation: &service.CarpoolResetObservation{ObservedAt: now, ResetAt: reset, Utilization: 90}})
		registered <- registration{id, err}
	}()
	require.Eventually(t, func() bool {
		var count int
		err := pool.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND NOT granted`, key).Scan(&count)
		return err == nil && count > 0
	}, time.Second, 10*time.Millisecond)
	type observed struct {
		estimate *service.SubscriptionCostEstimate
		err      error
	}
	result := make(chan observed, 1)
	go func() {
		estimate, err := repo.Observe(ctx, 9, "shared", service.CarpoolResetObservation{ObservedAt: now.Add(time.Second), ResetAt: reset, Utilization: 100})
		result <- observed{estimate, err}
	}()
	select {
	case outcome := <-result:
		t.Fatalf("freeze crossed an uncommitted registration: %+v", outcome)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, blocker.Commit())
	pending := <-registered
	require.NoError(t, pending.err)
	outcome := <-result
	require.NoError(t, outcome.err)
	require.Nil(t, outcome.estimate)
	_, err = db.Exec(`INSERT INTO usage_logs(account_id,created_at,total_cost,account_cost_started_at,account_cost_observed_at,account_cost_identity_hash,account_cost_pending_id) VALUES(9,$1,200,$2,$3,$4,$5)`, now.Add(time.Second), now.Add(-time.Second), now, service.CarpoolResetIdentityHash("shared"), pending.id)
	require.NoError(t, err)
	estimate, err := repo.Observe(ctx, 9, "shared", service.CarpoolResetObservation{ObservedAt: now.Add(2 * time.Second), ResetAt: reset, Utilization: 100})
	require.NoError(t, err)
	require.Equal(t, 2000.0, *estimate.EstimatedTotalCost)
}
