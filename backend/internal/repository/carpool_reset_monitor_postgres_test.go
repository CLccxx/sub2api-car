package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func carpoolMonitorTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return carpoolMonitorSchemaDB(t, true)
}

func carpoolMonitorSchemaDB(t *testing.T, accountScope bool) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CARPOOL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires an isolated CARPOOL_TEST_DATABASE_URL")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	schema := fmt.Sprintf("carpool_monitor_%d", time.Now().UnixNano())
	_, err = db.Exec(`CREATE SCHEMA ` + schema + `; SET search_path TO ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); _ = db.Close() })
	_, err = db.Exec(`
		CREATE TABLE users(id bigint PRIMARY KEY,status text DEFAULT 'active',deleted_at timestamptz);
 INSERT INTO users(id) SELECT generate_series(1,1000);
		CREATE TABLE groups (id bigint PRIMARY KEY, status text, platform text, subscription_type text, carpool_members int, deleted_at timestamptz, weekly_limit_usd numeric);
		CREATE TABLE accounts (id bigint PRIMARY KEY, platform text, type text, status text, parent_account_id bigint, credentials jsonb, deleted_at timestamptz, auto_pause_on_expired boolean DEFAULT false, expires_at timestamptz);
		CREATE TABLE account_groups (account_id bigint, group_id bigint, PRIMARY KEY(account_id,group_id));
		CREATE TABLE user_subscriptions (id bigint PRIMARY KEY, user_id bigint, group_id bigint, status text, deleted_at timestamptz,
		 starts_at timestamptz DEFAULT now(), expires_at timestamptz DEFAULT now()+interval '1 day', updated_at timestamptz DEFAULT now(),
		 daily_usage_usd numeric DEFAULT 10, weekly_usage_usd numeric DEFAULT 20, monthly_usage_usd numeric DEFAULT 30,
		 daily_window_start timestamptz DEFAULT now()-interval '1 day', weekly_window_start timestamptz DEFAULT now()-interval '1 day', monthly_window_start timestamptz DEFAULT now()-interval '1 day');
		INSERT INTO groups VALUES (10,'active','openai','subscription',4,NULL,325), (20,'active','openai','subscription',0,NULL,99);
		INSERT INTO accounts (id,platform,type,status,credentials) VALUES (9,'openai','oauth','active','{"chatgpt_account_id":"shared"}');
		INSERT INTO account_groups VALUES(9,10),(9,20);
		INSERT INTO user_subscriptions(id,user_id,group_id,status) VALUES (1,1,10,'active'), (2,2,20,'active'), (3,3,10,'suspended');
		INSERT INTO user_subscriptions(id,user_id,group_id,status,expires_at) VALUES (4,4,10,'active',now()-interval '1 second');
	`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("238_custom_carpool_reset_monitor.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	files := []string{"241_custom_quota_reset_operations.sql", "242_custom_carpool_reset_voting.sql"}
	if accountScope {
		files = append(files, "249_custom_carpool_account_scope.sql")
	}
	for _, file := range files {
		migration, err := migrations.FS.ReadFile(file)
		require.NoError(t, err)
		_, err = db.Exec(string(migration))
		require.NoError(t, err)
	}
	return db
}

func carpoolMonitorPendingEvent(t *testing.T, db *sql.DB) (service.CarpoolResetMonitorRepository, *service.CarpoolResetEvent, time.Time) {
	t.Helper()
	ctx := context.Background()
	repo := NewCarpoolResetMonitorRepository(db)
	source, err := repo.Source(ctx)
	require.NoError(t, err)
	require.NotNil(t, source)
	now := time.Now().UTC().Truncate(time.Second)
	old := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(24 * time.Hour), Utilization: 20}
	event, err := repo.Observe(ctx, *source, old)
	require.NoError(t, err)
	require.Nil(t, event, "first read only establishes a baseline")
	now = now.Add(10 * time.Minute)
	current := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(7*24*time.Hour - time.Minute), Utilization: .5}
	event, err = repo.Observe(ctx, *source, current)
	require.NoError(t, err)
	require.NotNil(t, event)
	again, err := repo.Observe(ctx, *source, current)
	require.NoError(t, err)
	require.Nil(t, again, "same observation cannot create another event")
	return repo, event, now
}

func TestCarpoolResetMonitorPostgresExactlyOnce(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	repo, event, now := carpoolMonitorPendingEvent(t, db)
	ctx := context.Background()
	_, err := db.Exec(`INSERT INTO user_subscriptions(id,user_id,group_id,status) VALUES (5,5,10,'active')`)
	require.NoError(t, err)
	result, err := repo.ApplyEvent(ctx, event.ID, now, now.Truncate(24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, "completed", result.Status)
	require.Equal(t, 2, result.ResetCount, "account scope includes its ordinary subscription group")
	var daily, weekly, monthly float64
	var window time.Time
	for _, id := range []int64{1, 2} {
		require.NoError(t, db.QueryRow(`SELECT daily_usage_usd,weekly_usage_usd,monthly_usage_usd,weekly_window_start FROM user_subscriptions WHERE id=$1`, id).Scan(&daily, &weekly, &monthly, &window))
		require.Zero(t, daily+weekly+monthly)
		require.True(t, now.Equal(window))
	}
	for _, id := range []int64{3, 4, 5} {
		require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=$1`, id).Scan(&weekly))
		require.Equal(t, 20.0, weekly, "inactive/expired/new subscribers are not reset")
	}
	_, err = db.Exec(`UPDATE user_subscriptions SET weekly_usage_usd=7 WHERE id=1`)
	require.NoError(t, err)
	restarted := NewCarpoolResetMonitorRepository(db)
	_, err = restarted.ApplyEvent(ctx, event.ID, now.Add(time.Minute), now.Truncate(24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=1`).Scan(&weekly))
	require.Equal(t, 7.0, weekly, "replay after restart cannot clear new usage")
	invalidations, err := repo.PendingInvalidations(ctx, 100)
	require.NoError(t, err)
	require.Len(t, invalidations, 2)
	require.NoError(t, repo.CompleteInvalidation(ctx, event.ID, 1))
	invalidations, err = repo.PendingInvalidations(ctx, 100)
	require.NoError(t, err)
	require.Len(t, invalidations, 1)
	var cap float64
	require.NoError(t, db.QueryRow(`SELECT weekly_limit_usd FROM groups WHERE id=10`).Scan(&cap))
	require.Equal(t, 325.0, cap)
}

func TestCarpoolResetMonitorPostgresRollback(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	repo, event, now := carpoolMonitorPendingEvent(t, db)
	_, err := db.Exec(`CREATE FUNCTION fail_reset() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=2 THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_reset BEFORE UPDATE ON user_subscriptions FOR EACH ROW EXECUTE FUNCTION fail_reset()`)
	require.NoError(t, err)
	_, err = repo.ApplyEvent(context.Background(), event.ID, now, now)
	require.Error(t, err)
	var usage float64
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=1`).Scan(&usage))
	require.Equal(t, 20.0, usage, "all subscription changes roll back together")
	pending, err := repo.PendingEvents(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int64{event.ID}, pending)
	_, err = db.Exec(`DROP TRIGGER fail_reset ON user_subscriptions`)
	require.NoError(t, err)
	result, err := repo.ApplyEvent(context.Background(), event.ID, now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, 2, result.ResetCount)
}

func TestCarpoolResetMonitorPostgresGuards(t *testing.T) {
	for _, scenario := range []string{"revoke", "renew", "expired subscription", "deleted subscription", "changed owner", "changed group", "replace account", "extra account", "expired event", "inactive account", "shadow account", "expired account", "delete group", "delete account", "unbound"} {
		t.Run(scenario, func(t *testing.T) {
			db := carpoolMonitorTestDB(t)
			repo, event, now := carpoolMonitorPendingEvent(t, db)
			var err error
			switch scenario {
			case "revoke":
				_, err = db.Exec(`UPDATE user_subscriptions SET status='suspended' WHERE id=1`)
			case "renew":
				_, err = db.Exec(`UPDATE user_subscriptions SET starts_at=starts_at+interval '1 second' WHERE id=1`)
			case "expired subscription":
				_, err = db.Exec(`UPDATE user_subscriptions SET expires_at=now()-interval '1 second' WHERE id=1`)
			case "deleted subscription":
				_, err = db.Exec(`UPDATE user_subscriptions SET deleted_at=now() WHERE id=1`)
			case "changed owner":
				_, err = db.Exec(`UPDATE user_subscriptions SET user_id=999 WHERE id=1`)
			case "changed group":
				_, err = db.Exec(`UPDATE user_subscriptions SET group_id=20 WHERE id=1`)
			case "replace account":
				_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"other"}' WHERE id=9`)
			case "extra account":
				_, err = db.Exec(`INSERT INTO accounts(id,platform,type,status,credentials) VALUES (11,'openai','oauth','inactive','{}'); INSERT INTO account_groups VALUES (11,10)`)
			case "disabled":
				_, err = db.Exec(`UPDATE groups SET status='disabled' WHERE id=10`)
			case "expired event":
				now = now.Add(time.Hour)
			case "inactive account":
				_, err = db.Exec(`UPDATE accounts SET status='inactive' WHERE id=9`)
			case "shadow account":
				_, err = db.Exec(`UPDATE accounts SET parent_account_id=8 WHERE id=9`)
			case "expired account":
				_, err = db.Exec(`UPDATE accounts SET auto_pause_on_expired=true,expires_at=now()-interval '1 second' WHERE id=9`)
			case "second carpool":
				_, err = db.Exec(`UPDATE groups SET carpool_members=3 WHERE id=20`)
			case "insert carpool":
				_, err = db.Exec(`INSERT INTO groups(id,status,platform,subscription_type,carpool_members) VALUES(30,'active','openai','subscription',4)`)
			case "delete group":
				_, err = db.Exec(`DELETE FROM groups WHERE id=10`)
			case "delete account":
				_, err = db.Exec(`DELETE FROM accounts WHERE id=9`)
			case "unbound":
				_, err = db.Exec(`DELETE FROM account_groups WHERE account_id=9`)
			}
			require.NoError(t, err)
			result, err := repo.ApplyEvent(context.Background(), event.ID, now, now)
			require.NoError(t, err)
			var usage float64
			require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=1`).Scan(&usage))
			require.Equal(t, 20.0, usage)
			if scenario == "revoke" || scenario == "renew" || scenario == "expired subscription" || scenario == "deleted subscription" || scenario == "changed owner" || scenario == "changed group" {
				require.Equal(t, 1, result.ResetCount)
			} else {
				require.Equal(t, "cancelled", result.Status)
			}
		})
	}
}

func TestCarpoolResetMonitorPostgresBeyondPagination(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	_, err := db.Exec(`INSERT INTO user_subscriptions(id,user_id,group_id,status) SELECT id,id,20,'active' FROM generate_series(10,259) AS id`)
	require.NoError(t, err)
	repo, event, now := carpoolMonitorPendingEvent(t, db)
	result, err := repo.ApplyEvent(context.Background(), event.ID, now, now)
	require.NoError(t, err)
	require.Equal(t, 252, result.ResetCount)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_subscriptions WHERE weekly_usage_usd=0`).Scan(&count))
	require.Equal(t, 252, count)
	for _, want := range []int{100, 100, 52} {
		items, err := repo.PendingInvalidations(context.Background(), 100)
		require.NoError(t, err)
		require.Len(t, items, want)
		for _, item := range items {
			require.NoError(t, repo.CompleteInvalidation(context.Background(), item.EventID, item.SubscriptionID))
		}
	}
	items, err := repo.PendingInvalidations(context.Background(), 100)
	require.NoError(t, err)
	require.Empty(t, items)
}

func TestCarpoolResetMonitorPostgresReceiptReplay(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	repo, event, now := carpoolMonitorPendingEvent(t, db)
	ctx := context.Background()
	source, err := repo.Source(ctx)
	require.NoError(t, err)
	_, err = repo.ApplyEvent(ctx, event.ID, now, now)
	require.NoError(t, err)
	// Receipt insertion can lag the window observation, including the same second.
	request := service.CarpoolResetIdentityHash("request")
	require.NoError(t, repo.RecordResetReceipt(ctx, source.AccountID, source.IdentityHash, request, now.Add(500*time.Millisecond), 1))
	require.NoError(t, repo.RecordResetReceipt(ctx, source.AccountID, source.IdentityHash, request, now.Add(time.Minute), 1))
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM carpool_account_reset_receipts`).Scan(&count))
	require.Equal(t, 1, count)
	current := service.CarpoolResetObservation{ObservedAt: now.Add(10 * time.Minute), ResetAt: now.Add(7*24*time.Hour - time.Minute), Utilization: 0}
	next, err := repo.Observe(ctx, *source, current)
	require.NoError(t, err)
	require.Nil(t, next, "late receipt must not repeat a completed window reset")
	current.ObservedAt = current.ObservedAt.Add(10 * time.Minute)
	current.Utilization = 20
	_, err = repo.Observe(ctx, *source, current)
	require.NoError(t, err)
	require.NoError(t, repo.RecordResetReceipt(ctx, source.AccountID, source.IdentityHash, service.CarpoolResetIdentityHash("new request"), current.ObservedAt.Add(time.Minute), 1))
	current.ObservedAt = current.ObservedAt.Add(10 * time.Minute)
	current.Utilization = .5
	next, err = repo.Observe(ctx, *source, current)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, "receipt", next.Reason)
	_, err = repo.ApplyEvent(ctx, next.ID, current.ObservedAt, current.ObservedAt)
	require.NoError(t, err)
	current.ObservedAt = current.ObservedAt.Add(10 * time.Minute)
	current.Utilization = 0
	next, err = repo.Observe(ctx, *source, current)
	require.NoError(t, err)
	require.Nil(t, next)
}

func TestCarpoolResetMonitorPostgresSourceReplacementAndSuperseding(t *testing.T) {
	for _, scenario := range []string{"replacement", "clear", "new event"} {
		t.Run(scenario, func(t *testing.T) {
			db := carpoolMonitorTestDB(t)
			repo, event, now := carpoolMonitorPendingEvent(t, db)
			ctx := context.Background()
			if scenario == "replacement" {
				_, err := db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"replacement"}' WHERE id=9`)
				require.NoError(t, err)
			}
			if scenario == "clear" {
				_, err := db.Exec(`UPDATE groups SET status='disabled' WHERE id IN(10,20)`)
				require.NoError(t, err)
				require.NoError(t, repo.ClearBaseline(ctx))
				_, err = db.Exec(`UPDATE groups SET status='active' WHERE id IN(10,20)`)
				require.NoError(t, err)
			}
			source, err := repo.Source(ctx)
			require.NoError(t, err)
			current := service.CarpoolResetObservation{ObservedAt: now.Add(10 * time.Minute), ResetAt: now.Add(7*24*time.Hour + 9*time.Minute), Utilization: 0}
			next, err := repo.Observe(ctx, *source, current)
			require.NoError(t, err)
			if scenario == "new event" {
				require.NotNil(t, next)
			} else {
				require.Nil(t, next, "replacement/re-enabled sources establish a baseline only")
			}
			result, err := repo.ApplyEvent(ctx, event.ID, current.ObservedAt, current.ObservedAt)
			require.NoError(t, err)
			require.Equal(t, "cancelled", result.Status)
			var usage float64
			require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=1`).Scan(&usage))
			require.Equal(t, 20.0, usage)
		})
	}
}

func TestCarpoolResetMonitorPostgresSourceRoundTrip(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	repo, event, now := carpoolMonitorPendingEvent(t, db)
	ctx := context.Background()
	for _, identity := range []string{"replacement", "shared"} {
		_, err := db.Exec(`UPDATE accounts SET credentials=jsonb_build_object('chatgpt_account_id',$1::text) WHERE id=9`, identity)
		require.NoError(t, err)
		source, err := repo.Source(ctx)
		require.NoError(t, err)
		now = now.Add(time.Minute)
		next, err := repo.Observe(ctx, *source, service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(24 * time.Hour), Utilization: 0})
		require.NoError(t, err)
		require.Nil(t, next)
	}
	result, err := repo.ApplyEvent(ctx, event.ID, now, now)
	require.NoError(t, err)
	require.Equal(t, "cancelled", result.Status, "returning to a former source cannot revive its pending event")
}

func TestCarpoolResetMonitorPostgresSourceWrites(t *testing.T) {
	for _, scenario := range []string{"identity round trip", "binding round trip", "disabled round trip", "token refresh", "cap update"} {
		t.Run(scenario, func(t *testing.T) {
			db := carpoolMonitorTestDB(t)
			repo, event, now := carpoolMonitorPendingEvent(t, db)
			var err error
			switch scenario {
			case "identity round trip":
				_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"other"}' WHERE id=9; UPDATE accounts SET credentials='{"chatgpt_account_id":"shared"}' WHERE id=9`)
			case "binding round trip":
				_, err = db.Exec(`DELETE FROM account_groups WHERE account_id=9; INSERT INTO account_groups VALUES(9,10),(9,20)`)
			case "disabled round trip":
				_, err = db.Exec(`UPDATE groups SET carpool_members=0 WHERE id=10; UPDATE groups SET carpool_members=4 WHERE id=10`)
			case "token refresh":
				_, err = db.Exec(`UPDATE accounts SET credentials=credentials||'{"access_token":"refreshed"}'::jsonb WHERE id=9`)
			case "cap update":
				_, err = db.Exec(`UPDATE groups SET weekly_limit_usd=350 WHERE id=10`)
			}
			require.NoError(t, err)
			result, err := repo.ApplyEvent(context.Background(), event.ID, now, now)
			require.NoError(t, err)
			if scenario == "token refresh" || scenario == "cap update" {
				require.Equal(t, "completed", result.Status, "unrelated changes must not disable detection")
			} else {
				require.Equal(t, "cancelled", result.Status, "unobserved source changes must permanently cancel old work")
			}
		})
	}
}

func TestCarpoolResetMonitorPostgresInFlightObservationAcrossSourceChange(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	repo := NewCarpoolResetMonitorRepository(db)
	ctx := context.Background()
	source, err := repo.Source(ctx)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	old := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(24 * time.Hour), Utilization: 20}
	_, err = repo.Observe(ctx, *source, old)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"other"}' WHERE id=9; UPDATE accounts SET credentials='{"chatgpt_account_id":"shared"}' WHERE id=9`)
	require.NoError(t, err)
	old.ObservedAt = old.ObservedAt.Add(10 * time.Minute)
	_, err = repo.Observe(ctx, *source, old)
	require.NoError(t, err)
	fresh, err := repo.Source(ctx)
	require.NoError(t, err)
	next := service.CarpoolResetObservation{ObservedAt: now.Add(20 * time.Minute), ResetAt: now.Add(7*24*time.Hour + 19*time.Minute), Utilization: 0}
	event, err := repo.Observe(ctx, *fresh, next)
	require.NoError(t, err)
	require.Nil(t, event, "an in-flight response from before a source change cannot seed the new lifecycle")
}

func TestCarpoolResetMonitorPostgresIndependentSourceWriters(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	pool := carpoolMonitorConcurrentPool(t, db)
	_, _, _ = carpoolMonitorPendingEvent(t, pool)
	first, err := pool.Begin()
	require.NoError(t, err)
	defer func() { _ = first.Rollback() }()
	_, err = first.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"other"}' WHERE id=9`)
	require.NoError(t, err)
	second, err := pool.Begin()
	require.NoError(t, err)
	defer func() { _ = second.Rollback() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = second.ExecContext(ctx, `UPDATE groups SET carpool_members=3 WHERE id=10`)
	require.NoError(t, err, "independent source writers must not contend on the monitor singleton")
	require.NoError(t, second.Commit())
	repo := NewCarpoolResetMonitorRepository(pool)
	before, err := repo.Source(context.Background())
	require.NoError(t, err)
	require.NotNil(t, before)
	_, err = first.Exec(`UPDATE groups SET carpool_members=3 WHERE id=10`)
	require.NoError(t, err)
	require.NoError(t, first.Commit())
	after, err := repo.Source(context.Background())
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Greater(t, after.Revision, before.Revision, "a lower sequence ID committed later must still advance the revision")
}

func TestCarpoolResetMonitorPostgresStaleClearDecision(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	repo, event, _ := carpoolMonitorPendingEvent(t, db)
	require.NoError(t, repo.ClearBaseline(context.Background()))
	pending, err := repo.PendingEvents(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int64{event.ID}, pending, "a now-valid source must not be cleared based on an earlier unavailable read")
}

func TestCarpoolResetMonitorPostgresActualExpiry(t *testing.T) {
	for _, scenario := range []string{"event", "subscription"} {
		t.Run(scenario, func(t *testing.T) {
			db := carpoolMonitorTestDB(t)
			repo, event, _ := carpoolMonitorPendingEvent(t, db)
			var err error
			if scenario == "event" {
				_, err = db.Exec(`UPDATE carpool_subscription_reset_events SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, event.ID)
			} else {
				_, err = db.Exec(`UPDATE user_subscriptions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=1`)
			}
			require.NoError(t, err)
			staleNow := time.Now().Add(-10 * time.Second)
			result, err := repo.ApplyEvent(context.Background(), event.ID, staleNow, staleNow)
			require.NoError(t, err)
			if scenario == "event" {
				require.Equal(t, "cancelled", result.Status)
			} else {
				require.Equal(t, 1, result.ResetCount)
			}
		})
	}
}

func carpoolMonitorConcurrentPool(t *testing.T, db *sql.DB) *sql.DB {
	t.Helper()
	var schema string
	require.NoError(t, db.QueryRow(`SELECT current_schema()`).Scan(&schema))
	dsn, err := url.Parse(os.Getenv("CARPOOL_TEST_DATABASE_URL"))
	require.NoError(t, err)
	query := dsn.Query()
	query.Set("search_path", schema)
	dsn.RawQuery = query.Encode()
	pool, err := sql.Open("postgres", dsn.String())
	require.NoError(t, err)
	pool.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func TestCarpoolResetMonitorPostgresConcurrentSourceWriter(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	pool := carpoolMonitorConcurrentPool(t, db)
	_, err := pool.Exec(`INSERT INTO accounts(id,platform,type,status,credentials,deleted_at) VALUES(11,'openai','oauth','active','{"chatgpt_account_id":"second"}',now()); INSERT INTO account_groups VALUES(11,10)`)
	require.NoError(t, err)
	repo, event, now := carpoolMonitorPendingEvent(t, pool)
	writer, err := pool.Begin()
	require.NoError(t, err)
	defer func() { _ = writer.Rollback() }()
	_, err = writer.Exec(`UPDATE accounts SET deleted_at=NULL WHERE id=11`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = repo.ApplyEvent(ctx, event.ID, now, now)
	require.Error(t, err, "do not reset while an excluded account is concurrently being restored")
	require.NoError(t, writer.Rollback())
	result, err := repo.ApplyEvent(context.Background(), event.ID, now, now)
	require.NoError(t, err)
	require.Equal(t, 2, result.ResetCount)
}

func TestCarpoolResetMonitorPostgresConcurrentInstances(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	pool := carpoolMonitorConcurrentPool(t, db)
	repo, event, now := carpoolMonitorPendingEvent(t, pool)
	_, err := pool.Exec(`CREATE TABLE reset_audit(subscription_id bigint);
		CREATE FUNCTION audit_reset() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO reset_audit VALUES(NEW.id); RETURN NEW; END $$;
		CREATE TRIGGER audit_reset AFTER UPDATE ON user_subscriptions FOR EACH ROW EXECUTE FUNCTION audit_reset()`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source, err := repo.Source(ctx)
	require.NoError(t, err)
	results := make(chan error, 12)
	for i := 0; i < 6; i++ {
		go func() { _, err := repo.ApplyEvent(ctx, event.ID, now, now); results <- err }()
		go func() {
			_, err := repo.Observe(ctx, *source, service.CarpoolResetObservation{ObservedAt: now.Add(time.Minute), ResetAt: now.Add(7*24*time.Hour - time.Minute), Utilization: 1})
			results <- err
		}()
	}
	for i := 0; i < 12; i++ {
		require.NoError(t, <-results)
	}
	var count int
	require.NoError(t, pool.QueryRow(`SELECT count(*) FROM reset_audit`).Scan(&count))
	require.Equal(t, 2, count, "each subscription is updated exactly once across concurrent instances")
	require.NoError(t, pool.QueryRow(`SELECT count(*) FROM carpool_subscription_reset_events`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestCarpoolResetMonitorPostgresAccountScope(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	_, err := db.Exec(`INSERT INTO groups VALUES(30,'active','openai','subscription',5,NULL,400),(40,'active','openai','standard',0,NULL,0),(50,'active','openai','subscription',4,NULL,500),(60,'active','openai','subscription',5,NULL,500);
 INSERT INTO accounts(id,platform,type,status,credentials) VALUES(8,'openai','oauth','active','{"chatgpt_account_id":"other"}');
 INSERT INTO account_groups VALUES(9,30),(9,40),(8,50),(8,60);
 INSERT INTO user_subscriptions(id,user_id,group_id,status) VALUES(5,5,30,'active'),(6,6,40,'active'),(7,7,50,'active'),(8,8,60,'active')`)
	require.NoError(t, err)
	r := NewCarpoolResetMonitorRepository(db)
	// A source is resolved per account even when several tiers and accounts exist.
	e, err := NewCarpoolResetVoteRepository(db).Eligibility(context.Background(), 1, 1)
	require.NoError(t, err)
	require.NotNil(t, e.Source)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = r.Observe(context.Background(), *e.Source, service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 20})
	require.NoError(t, err)
	event, err := r.Observe(context.Background(), *e.Source, service.CarpoolResetObservation{ObservedAt: now.Add(time.Minute), ResetAt: now.Add(7 * 24 * time.Hour), Utilization: 0})
	require.NoError(t, err)
	require.NotNil(t, event)
	result, err := r.ApplyEvent(context.Background(), event.ID, now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, 3, result.ResetCount)
	var resetIDs string
	require.NoError(t, db.QueryRow(`SELECT string_agg(id::text,',' ORDER BY id) FROM user_subscriptions WHERE weekly_usage_usd=0`).Scan(&resetIDs))
	require.Equal(t, "1,2,5", resetIDs, "ordinary subscriptions included, balance and other accounts excluded")
}

func TestCarpoolResetMonitorPostgresOrdinaryOAuthSources(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	_, err := db.Exec(`UPDATE groups SET carpool_members=0;
 INSERT INTO groups VALUES(30,'active','openai','subscription',0,NULL,100),(40,'active','openai','standard',0,NULL,100);
 INSERT INTO accounts(id,platform,type,status,credentials) VALUES(8,'openai','apikey','active','{"chatgpt_account_id":"key-account"}'),(7,'openai','oauth','active','{"chatgpt_account_id":"balance-only"}');
 INSERT INTO account_groups VALUES(8,30),(7,40)`)
	require.NoError(t, err)
	r := NewCarpoolResetMonitorRepository(db)
	sources, err := r.Sources(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.EqualValues(t, 9, sources[0].AccountID)
	_, event, now := carpoolMonitorPendingEvent(t, db)
	result, err := r.ApplyEvent(context.Background(), event.ID, now, now)
	require.NoError(t, err)
	require.Equal(t, 2, result.ResetCount)
	e, err := NewCarpoolResetVoteRepository(db).Eligibility(context.Background(), 1, 1)
	require.NoError(t, err)
	require.Empty(t, e.BlockedReason)
	require.Equal(t, 2, e.RequiredCount)
}

func TestCarpoolResetMonitorPostgresIndependentAccounts(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	_, err := db.Exec(`INSERT INTO groups VALUES(30,'active','openai','subscription',4,NULL,100),(40,'active','openai','subscription',5,NULL,100);
 INSERT INTO accounts(id,platform,type,status,credentials) VALUES(8,'openai','oauth','active','{"chatgpt_account_id":"other"}');
 INSERT INTO account_groups VALUES(8,30),(8,40);
 INSERT INTO user_subscriptions(id,user_id,group_id,status) VALUES(5,5,30,'active'),(6,6,40,'active')`)
	require.NoError(t, err)
	r := NewCarpoolResetMonitorRepository(db)
	ctx := context.Background()
	sources, err := r.Sources(ctx)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	now := time.Now().UTC().Truncate(time.Second)
	events := map[int64]int64{}
	for _, source := range sources {
		_, err = r.Observe(ctx, source, service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 20})
		require.NoError(t, err)
	}
	for _, source := range sources {
		event, err := r.Observe(ctx, source, service.CarpoolResetObservation{ObservedAt: now.Add(time.Minute), ResetAt: now.Add(7 * 24 * time.Hour), Utilization: 0})
		require.NoError(t, err)
		require.NotNil(t, event)
		events[source.AccountID] = event.ID
	}
	var baselines int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM carpool_reset_account_monitors WHERE baseline IS NOT NULL`).Scan(&baselines))
	require.Equal(t, 2, baselines)
	// Only B changes. A retains both its baseline and its pending event.
	_, err = db.Exec(`UPDATE accounts SET credentials='{"chatgpt_account_id":"other-new"}' WHERE id=8`)
	require.NoError(t, err)
	result, err := r.ApplyEvent(ctx, events[9], now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, "completed", result.Status)
	require.Equal(t, 2, result.ResetCount)
	result, err = r.ApplyEvent(ctx, events[8], now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, "cancelled", result.Status)
	var untouched int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_subscriptions WHERE id IN(5,6) AND weekly_usage_usd=20`).Scan(&untouched))
	require.Equal(t, 2, untouched)
	// Pruning unavailable B must leave A intact even with another active source.
	_, err = db.Exec(`UPDATE accounts SET status='disabled' WHERE id=8`)
	require.NoError(t, err)
	require.NoError(t, r.ClearBaseline(ctx))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM carpool_reset_account_monitors WHERE account_id=9 AND baseline IS NOT NULL`).Scan(&baselines))
	require.Equal(t, 1, baselines)
}

func TestCarpoolResetMonitorPostgresLegacyGlobalTargets(t *testing.T) {
	db := carpoolMonitorTestDB(t)
	_, err := db.Exec(`INSERT INTO groups VALUES(30,'active','openai','subscription',0,NULL,100),(40,'active','openai','standard',0,NULL,100);
 INSERT INTO accounts(id,platform,type,status,credentials) VALUES(8,'openai','oauth','active','{"chatgpt_account_id":"other"}');
 INSERT INTO account_groups VALUES(8,30),(9,40);
 INSERT INTO user_subscriptions(id,user_id,group_id,status) VALUES(5,5,30,'active'),(6,6,40,'active')`)
	require.NoError(t, err)
	// Seed a historical event carrying the old global snapshot. Ordinary groups'
	// original binding cannot be proven; only its unchanged source group is safe.
	var event int64
	require.NoError(t, db.QueryRow(`INSERT INTO carpool_subscription_reset_events(event_key,group_id,account_id,identity_hash,reason,observed_at,expires_at,source_revision)
 VALUES('legacy',10,9,$1,'vote',now(),now()+interval '1 day',(SELECT count(*) FROM carpool_reset_source_changes)) RETURNING id`, service.CarpoolResetIdentityHash("shared")).Scan(&event))
	_, err = db.Exec(`INSERT INTO carpool_reset_monitor(id) VALUES(1) ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO carpool_subscription_reset_targets(event_id,subscription_id,user_id,group_id,starts_at) SELECT $1,id,user_id,group_id,starts_at FROM user_subscriptions WHERE status='active'`, event)
	require.NoError(t, err)
	r := NewCarpoolResetMonitorRepository(db)
	now := time.Now()
	result, err := r.ApplyEvent(context.Background(), event, now, now)
	require.NoError(t, err)
	require.Equal(t, 1, result.ResetCount)
	require.Equal(t, 4, result.SkippedCount)
	var ids string
	require.NoError(t, db.QueryRow(`SELECT string_agg(id::text,',' ORDER BY id) FROM user_subscriptions WHERE weekly_usage_usd=0`).Scan(&ids))
	require.Equal(t, "1", ids)
}

func TestCarpoolResetMonitorPostgresLegacyMigration(t *testing.T) {
	db := carpoolMonitorSchemaDB(t, false)
	now := time.Now().UTC().Truncate(time.Second)
	observation := service.CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(time.Hour), Utilization: 20}
	baseline, err := json.Marshal(service.CarpoolResetBaseline{Observation: observation, Since: now})
	require.NoError(t, err)
	identity := service.CarpoolResetIdentityHash("shared")
	_, err = db.Exec(`INSERT INTO carpool_reset_monitor(id,group_id,account_id,identity_hash,baseline,source_revision) VALUES(1,10,9,$1,$2::jsonb,0)`, identity, string(baseline))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO carpool_reset_votes(group_id,account_id,created_by,creation_key,eligibility,observation,required_count,status,request_id)
 VALUES(10,9,1,'00000000-0000-4000-8000-000000000001','{}','{}',4,'executing','never-dispatched');
 INSERT INTO carpool_reset_votes(group_id,account_id,created_by,creation_key,eligibility,observation,required_count,status,request_id,dispatched_at)
 VALUES(20,8,2,'00000000-0000-4000-8000-000000000002','{}','{}',4,'awaiting_confirmation','uncertain',now());
 INSERT INTO quota_reset_operations(account_id,request_id,credit_id) VALUES(9,'never-dispatched','card1'),(8,'uncertain','card2');
 INSERT INTO carpool_reset_vote_requests(created_by,creation_key,proposal_id) SELECT created_by,creation_key,id FROM carpool_reset_votes;
 INSERT INTO carpool_reset_vote_approvals(proposal_id,user_id) SELECT id,created_by FROM carpool_reset_votes;
 INSERT INTO carpool_reset_vote_targets(proposal_id,subscription_id,user_id,group_id,starts_at) SELECT 2,id,user_id,group_id,starts_at FROM user_subscriptions`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("249_custom_carpool_account_scope.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	var proposal, operation string
	require.NoError(t, db.QueryRow(`SELECT p.status,o.status FROM carpool_reset_votes p JOIN quota_reset_operations o ON o.account_id=p.account_id WHERE p.id=1`).Scan(&proposal, &operation))
	require.Equal(t, "invalidated", proposal)
	require.Equal(t, "failed", operation)
	require.NoError(t, db.QueryRow(`SELECT p.status,o.status FROM carpool_reset_votes p JOIN quota_reset_operations o ON o.account_id=p.account_id WHERE p.id=2`).Scan(&proposal, &operation))
	require.Equal(t, "awaiting_confirmation", proposal)
	require.Equal(t, "pending", operation)
	var count int
	for _, table := range []string{"carpool_reset_vote_requests", "carpool_reset_vote_approvals"} {
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		require.Equal(t, 2, count)
	}
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM carpool_reset_vote_targets WHERE binding_revision IS NOT NULL`).Scan(&count))
	require.Zero(t, count)
	var inherited []byte
	var version int
	require.NoError(t, db.QueryRow(`SELECT baseline,scope_version FROM carpool_reset_account_monitors WHERE account_id=9`).Scan(&inherited, &version))
	require.JSONEq(t, string(baseline), string(inherited))
	require.Equal(t, 1, version)
	// Churn on B after migration cannot erase A's inherited observation.
	_, err = db.Exec(`INSERT INTO accounts(id,platform,type,status,credentials) VALUES(8,'openai','oauth','active','{"chatgpt_account_id":"other"}'); UPDATE accounts SET status='disabled' WHERE id=8`)
	require.NoError(t, err)
	r := NewCarpoolResetMonitorRepository(db)
	source, err := r.Source(context.Background())
	require.NoError(t, err)
	require.NotNil(t, source)
	event, err := r.Observe(context.Background(), *source, service.CarpoolResetObservation{ObservedAt: now.Add(time.Minute), ResetAt: now.Add(7 * 24 * time.Hour), Utilization: 0})
	require.NoError(t, err)
	require.NotNil(t, event, "unchanged legacy baseline must carry over into per-account monitoring")
	require.NoError(t, db.QueryRow(`SELECT scope_version FROM carpool_reset_account_monitors WHERE account_id=9`).Scan(&version))
	require.Equal(t, 1, version)
}
