package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const (
	car022BonusMigration     = "251_custom_group_carpool_bonus.sql"
	car022TradesMigration    = "252_custom_carpool_quota_trades.sql"
	car022PriceMigration     = "253_custom_carpool_quota_trade_price.sql"
	car022UltrafastMigration = "251_group_force_openai_ultrafast.sql"
	car022RollupMigration    = "251_group_usage_rollup_nonblocking_build.sql"
)

// Pin both exact bytes (including whitespace) and the runner's TrimSpace SHA256.
// These manifest digests were computed from git objects, NOT the merged FS:
//
//	official v0.2.21 = 43f15294c477bec427d586ddfc3f918ce1284c98 (302 SQL files)
//	car.6 HEAD       = e47cfe349                            (311 SQL files)
//
// The manifest is sorted filename + "\t" + per-file SHA256 + "\n". No git binary,
// network, checked-out release tree or generated fixture is needed at test time.
func TestCar021ToOfficial022PublishedMigrationBytes(t *testing.T) {
	assertCar022PublishedMigrationBytes(t)
}

func assertCar022PublishedMigrationBytes(t *testing.T) {
	t.Helper()
	custom := map[string]bool{
		"237_custom_group_carpool.sql":          true,
		"238_custom_carpool_reset_monitor.sql":  true,
		"241_custom_quota_reset_operations.sql": true,
		"242_custom_carpool_reset_voting.sql":   true,
		"249_custom_carpool_account_scope.sql":  true,
		"250_custom_subscription_cost.sql":      true,
		car022BonusMigration:                    true,
		car022TradesMigration:                   true,
		car022PriceMigration:                    true,
	}
	files, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	sort.Strings(files)
	var officialRaw, officialChecksums, carRaw, carChecksums strings.Builder
	var officialCount, carCount int
	for _, name := range files {
		if name == car022UltrafastMigration || name == car022RollupMigration {
			continue
		}
		content, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		raw := fmt.Sprintf("%s\t%x\n", name, sha256.Sum256(content))
		checksum := fmt.Sprintf("%s\t%x\n", name, sha256.Sum256([]byte(strings.TrimSpace(string(content)))))
		_, _ = carRaw.WriteString(raw)
		_, _ = carChecksums.WriteString(checksum)
		carCount++
		if !custom[name] {
			_, _ = officialRaw.WriteString(raw)
			_, _ = officialChecksums.WriteString(checksum)
			officialCount++
		}
	}
	require.Equal(t, 302, officialCount, "official 0.2.21 SQL set must stay intact")
	require.Equal(t, 311, carCount, "car.6 SQL set must stay intact; only the two upstream 251 files are new")
	for _, manifest := range []struct {
		name, text, digest string
	}{
		{"official bytes", officialRaw.String(), "02ca8c89c930c09884362eb87cd002d72baffd1b9732c548a0fe4c0d75f5fcad"},
		{"official checksums", officialChecksums.String(), "84fe057f785f91dfe593a3da6d28d0bb7d38d96fb2e09329d849cb920e12f8fe"},
		{"car.6 bytes", carRaw.String(), "326212e4d5a2bafc6c82782d494ab386b4ca0672974e7fc94ae1f7bef214a160"},
		{"car.6 checksums", carChecksums.String(), "94bacccef8a860f8ed043952ff630392437a31923176d5c6dc14d0f6d3d1857e"},
	} {
		require.Equal(t, manifest.digest, fmt.Sprintf("%x", sha256.Sum256([]byte(manifest.text))), manifest.name)
	}
}

func TestCar021ToOfficial022PostgresPreservesData(t *testing.T) {
	assertCar022PublishedMigrationBytes(t)
	for _, tc := range []struct {
		name                 string
		bonus, trades, price bool
		ultrafast, rollup    bool
		baselineCount        int
	}{
		{name: "car4_before_custom251", baselineCount: 308},
		{name: "car5_custom251", bonus: true, baselineCount: 309},
		// A durable checkpoint between 252 and 253, not a separate release.
		{name: "custom252_before_price", bonus: true, trades: true, baselineCount: 310},
		{name: "car6_all_custom251_252_253", bonus: true, trades: true, price: true, baselineCount: 311},
		// Same-prefix official migrations are independent ledger entries. Rehearse
		// recovery when either one has already been recorded, not just both pending.
		{name: "car6_ultrafast_already_applied", bonus: true, trades: true, price: true, ultrafast: true, baselineCount: 312},
		{name: "car6_rollup_already_applied", bonus: true, trades: true, price: true, rollup: true, baselineCount: 312},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := officialToCarTestDB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			pending := map[string]bool{
				car022BonusMigration: !tc.bonus, car022TradesMigration: !tc.trades, car022PriceMigration: !tc.price,
				car022UltrafastMigration: !tc.ultrafast, car022RollupMigration: !tc.rollup,
			}
			baseline := fstest.MapFS{}
			files, err := fs.Glob(migrations.FS, "*.sql")
			require.NoError(t, err)
			for _, name := range files {
				content, err := migrations.FS.ReadFile(name)
				require.NoError(t, err)
				if !pending[name] {
					baseline[name] = &fstest.MapFile{Data: content}
				}
			}
			require.Len(t, baseline, tc.baselineCount)
			require.NoError(t, applyMigrationsFS(ctx, db, baseline), "build source schema from unchanged released SQL")
			for name, isPending := range pending {
				var recorded bool
				require.NoError(t, db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename=$1)`, name).Scan(&recorded))
				require.Equal(t, !isPending, recorded, "source ledger: %s", name)
			}
			assertCar022ColumnPresence(t, ctx, db, "groups", "carpool_bonus_usd", tc.bonus)
			assertCar022ColumnPresence(t, ctx, db, "groups", "force_openai_ultrafast", tc.ultrafast)
			for _, column := range []string{"expired_before", "building_until", "build_dirty_from"} {
				assertCar022ColumnPresence(t, ctx, db, "usage_group_rollup_state", column, tc.rollup)
			}
			assertCar022ColumnPresence(t, ctx, db, "carpool_quota_trades", "sale_price_cents", tc.price)

			seedCar022MigrationData(t, ctx, db, tc.bonus, tc.trades, tc.price, tc.ultrafast, tc.rollup)
			// Select the actual source columns once: first apply may only ADD data,
			// never alter a source column. The second comparison includes ALL newly
			// added columns/tables as well, so defaults and NULL prices cannot drift.
			sourceColumns := car022MigrationColumns(t, ctx, db)
			sourceRows := car022MigrationSnapshot(t, ctx, db, sourceColumns)
			historical := car022MigrationLedger(t, ctx, db)
			require.Len(t, historical, tc.baselineCount)
			var originalViews map[string]string
			if tc.trades {
				originalViews = car022TradeViewSnapshot(t, ctx, db)
				assertCar022TradeEntitlements(t, ctx, db)
			}
			var firstRows map[string]string
			var firstColumns map[string][]string
			var firstViews map[string]string
			for pass := 1; pass <= 2; pass++ {
				require.NoError(t, ApplyMigrations(ctx, db), "upgrade pass %d", pass)
				require.Equal(t, sourceRows, car022MigrationSnapshot(t, ctx, db, sourceColumns), "all original rows/columns, pass %d", pass)
				current := car022MigrationLedger(t, ctx, db)
				require.Len(t, current, len(files))
				for name, entry := range historical {
					require.Equal(t, entry, current[name], "do not rewrite checksum/applied_at: %s, pass %d", name, pass)
				}
				for _, name := range files {
					content, err := migrations.FS.ReadFile(name)
					require.NoError(t, err)
					require.True(t, strings.HasPrefix(current[name], fmt.Sprintf("%x/", sha256.Sum256([]byte(strings.TrimSpace(string(content)))))), name)
				}
				if tc.trades {
					require.Equal(t, originalViews, car022TradeViewSnapshot(t, ctx, db), "cycle, revisions and entitlements survive, pass %d", pass)
					assertCar022TradeEntitlements(t, ctx, db)
					var priced int
					require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM carpool_quota_trades WHERE sale_price_cents IS NOT NULL`).Scan(&priced))
					if tc.price {
						require.Equal(t, 6, priced, "preserve non-NULL prices and legacy NULLs")
					} else {
						require.Zero(t, priced, "253 must never infer a price for 252 orders")
					}
				} else {
					var count int
					require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM carpool_quota_trades`).Scan(&count))
					require.Zero(t, count, "upgrade must not invent historical trades")
				}
				var bonus string
				var ultrafast bool
				require.NoError(t, db.QueryRowContext(ctx, `SELECT carpool_bonus_usd::text,force_openai_ultrafast FROM groups WHERE id=92001`).Scan(&bonus, &ultrafast))
				expectedBonus := "100.00000000"
				if tc.bonus {
					expectedBonus = "149.99000000"
				}
				require.Equal(t, expectedBonus, bonus, "preserve configured bonus; never recompute existing weekly cap")
				require.Equal(t, tc.ultrafast, ultrafast, "new ultrafast defaults off; an existing true survives")
				var expired string
				var building, dirty sql.NullString
				require.NoError(t, db.QueryRowContext(ctx, `SELECT to_char(expired_before AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI:SS'),building_until::text,build_dirty_from::text FROM usage_group_rollup_state WHERE id=1`).Scan(&expired, &building, &dirty))
				if tc.rollup {
					require.Equal(t, "2026-10-01 00:00:00", expired)
					require.Equal(t, sql.NullString{String: "2026-11-05", Valid: true}, building)
					require.Equal(t, sql.NullString{String: "2026-10-29", Valid: true}, dirty)
				} else {
					require.Equal(t, "1970-01-01 00:00:00", expired, "do not infer expiration from retained_from")
					require.False(t, building.Valid)
					require.False(t, dirty.Valid)
				}
				if pass == 1 {
					firstColumns = car022MigrationColumns(t, ctx, db)
					firstRows = car022MigrationSnapshot(t, ctx, db, firstColumns)
					firstViews = car022TradeViewSnapshot(t, ctx, db)
				} else {
					require.Equal(t, firstColumns, car022MigrationColumns(t, ctx, db), "second apply cannot change the schema")
					require.Equal(t, firstRows, car022MigrationSnapshot(t, ctx, db, firstColumns), "second apply cannot change any table row")
					require.Equal(t, firstViews, car022TradeViewSnapshot(t, ctx, db), "second apply cannot change cycle entitlements")
				}
				historical = current
			}
			// Run only after the immutable snapshots above. This smoke test uses
			// the real, fully migrated usage_logs table, not a reduced test schema.
			assertCar022RollupNonblocking(t, ctx, db)
		})
	}
}

func assertCar022ColumnPresence(t *testing.T, ctx context.Context, db *sql.DB, table, column string, expected bool) {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name=$2)`, table, column).Scan(&exists))
	require.Equal(t, expected, exists, "source column %s.%s", table, column)
}

func car022MigrationColumns(t *testing.T, ctx context.Context, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT c.table_name,array_agg(c.column_name::text ORDER BY c.ordinal_position)
		FROM information_schema.columns c JOIN information_schema.tables t
		 USING(table_catalog,table_schema,table_name)
		WHERE c.table_schema='public' AND t.table_type='BASE TABLE' AND c.table_name<>'schema_migrations'
		GROUP BY c.table_name ORDER BY c.table_name`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	out := map[string][]string{}
	for rows.Next() {
		var table string
		var columns pq.StringArray
		require.NoError(t, rows.Scan(&table, &columns))
		out[table] = []string(columns)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, out)
	return out
}

func car022MigrationSnapshot(t *testing.T, ctx context.Context, db *sql.DB, columns map[string][]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for table, names := range columns {
		quoted := make([]string, len(names))
		for i, name := range names {
			quoted[i] = pq.QuoteIdentifier(name)
		}
		out[table] = car022JSONRows(t, ctx, db, `SELECT `+strings.Join(quoted, ",")+` FROM public.`+pq.QuoteIdentifier(table))
	}
	return out
}

func car022JSONRows(t *testing.T, ctx context.Context, db *sql.DB, query string) string {
	t.Helper()
	var value string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM (`+query+`) t`).Scan(&value))
	return value
}

func car022TradeViewSnapshot(t *testing.T, ctx context.Context, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, view := range []string{"carpool_quota_trade_subscription_state", "carpool_quota_trade_entitlements"} {
		out[view] = car022JSONRows(t, ctx, db, `SELECT * FROM `+pq.QuoteIdentifier(view))
	}
	return out
}

func car022MigrationLedger(t *testing.T, ctx context.Context, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT filename,checksum || '/' || applied_at::text FROM schema_migrations ORDER BY filename`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	out := map[string]string{}
	for rows.Next() {
		var name, value string
		require.NoError(t, rows.Scan(&name, &value))
		out[name] = value
	}
	require.NoError(t, rows.Err())
	return out
}

func seedCar022MigrationData(t *testing.T, ctx context.Context, db *sql.DB, bonus, trades, price, ultrafast, rollup bool) {
	t.Helper()
	// No service, worker, OAuth refresh or upstream request is started. Fixed
	// microsecond timestamps, decimal amounts and JSON are compared as whole rows.
	_, err := db.ExecContext(ctx, `
		INSERT INTO users(id,email,password_hash,username,balance,role) VALUES
		 (91001,'seller@example.invalid','fixture-password','existing seller',123.45678901,'user'),
		 (91002,'buyer@example.invalid','fixture-password','existing buyer',98.76543210,'user');
		INSERT INTO groups(id,name,platform,subscription_type,weekly_limit_usd,rate_multiplier,carpool_members,carpool_quota_synced_at,force_openai_fast) VALUES
		 (92001,'Existing carpool','openai','subscription',325.25,1.125,3,'2026-10-30T12:34:56.123456Z',true),
		 (92002,'Existing ordinary group','openai','subscription',123.45,0.875,0,NULL,false);
		INSERT INTO accounts(id,name,platform,type,credentials,extra) VALUES
		 (93001,'Existing inert OAuth','openai','oauth',
		  '{"access_token":"fixture-access","refresh_token":"fixture-refresh","chatgpt_account_id":"fixture-car022-account","expires_at":4102444800}',
		  '{"fixture_setting":"preserved"}');
		INSERT INTO account_groups(account_id,group_id,priority) VALUES(93001,92001,7);
		INSERT INTO api_keys(id,user_id,key,name,group_id) VALUES(94001,91001,'sk-fixture-car022','Existing trade key',92001);
		INSERT INTO user_subscriptions(id,user_id,group_id,starts_at,expires_at,daily_window_start,weekly_window_start,monthly_window_start,daily_usage_usd,weekly_usage_usd,monthly_usage_usd,assigned_by) VALUES
		 (95001,91001,92001,'2026-10-01T00:00:00Z','2099-01-01T00:00:00Z','2026-10-30T00:00:00Z','2026-10-26T00:00:00Z','2026-10-01T00:00:00Z',1.1234567890,12.3456789012,123.4567890123,91001),
		 (95002,91002,92001,'2026-10-02T00:00:00Z','2099-01-02T00:00:00Z','2026-10-30T00:00:00Z','2026-10-26T00:00:00Z','2026-10-01T00:00:00Z',2.2345678901,23.4567890123,234.5678901234,91001);
		INSERT INTO usage_logs(id,user_id,api_key_id,account_id,group_id,subscription_id,request_id,model,input_tokens,output_tokens,total_cost,actual_cost,billing_type,cache_creation_30m_tokens,cache_creation_12h_tokens,created_at)
		 VALUES(96001,91001,94001,93001,92001,95001,'fixture-car022-request','gpt-5.5',1234,567,0.1234567890,0.1111111111,1,12,34,'2026-10-30T12:00:00.123456Z');
		INSERT INTO subscription_cost_account_epochs(account_id,revision,identity_started_at) VALUES(93001,7,'2026-10-01T00:00:00.123456Z');
		INSERT INTO subscription_cost_states(account_id,identity_hash,identity_revision,state,updated_at)
		 VALUES(93001,encode(sha256(convert_to('fixture-car022-account','UTF8')),'hex'),7,'{"frozen_base":2000,"cycle_start":"2026-10-26T00:00:00Z","seen_below":true}','2026-10-30T12:00:00.123456Z');
		INSERT INTO subscription_cost_pending_usage(id,account_id,identity_hash,started_at,observed_at,utilization,reset_at,usage_log_id,created_at)
		 VALUES(96002,93001,encode(sha256(convert_to('fixture-car022-account','UTF8')),'hex'),'2026-10-30T11:59:58Z','2026-10-30T12:00:00Z',90,'2026-11-02T00:00:00Z',96001,'2026-10-30T12:00:01Z'),
		       (96003,93001,encode(sha256(convert_to('fixture-car022-account','UTF8')),'hex'),'2026-10-30T12:00:02Z','2026-10-30T12:00:04Z',91,'2026-11-02T00:00:00Z',NULL,'2026-10-30T12:00:05Z');
		UPDATE usage_logs SET account_cost_started_at='2026-10-30T11:59:58Z',account_cost_observed_at='2026-10-30T12:00:00Z',
		 account_cost_identity_hash=encode(sha256(convert_to('fixture-car022-account','UTF8')),'hex'),account_cost_pending_id=96002 WHERE id=96001;
		INSERT INTO carpool_subscription_reset_events(id,event_key,group_id,account_id,identity_hash,reason,source_revision,observed_at,expires_at,status,reset_count,skipped_count,created_at,completed_at,scope_version)
		 SELECT v.id,v.key,92001,93001,encode(sha256(convert_to('fixture-car022-account','UTF8')),'hex'),'receipt',
		 (SELECT count(*) FROM carpool_reset_account_changes WHERE account_id=93001),v.observed::timestamptz,'2099-01-01T00:00:00Z','completed',2,1,v.observed::timestamptz,v.observed::timestamptz+interval '1 second',1
		 FROM (VALUES(97001,'fixture-previous-cycle','2026-10-19T00:00:00.123456Z'),(97002,'fixture-current-cycle','2026-10-26T00:00:00.234567Z')) v(id,key,observed);
		INSERT INTO carpool_subscription_reset_targets(event_id,subscription_id,user_id,group_id,starts_at,reset_applied,cache_invalidated_at,binding_revision)
		 SELECT 97002,id,user_id,group_id,starts_at,true,'2026-10-26T00:00:02.345678Z',
		 (SELECT count(*) FROM carpool_reset_group_changes WHERE group_id=92001) FROM user_subscriptions WHERE group_id=92001;
		INSERT INTO carpool_account_reset_receipts(id,account_id,identity_hash,request_hash,occurred_at,windows_reset,cost_reset_at)
		 VALUES(97010,93001,encode(sha256(convert_to('fixture-car022-account','UTF8')),'hex'),'fixture-receipt-hash','2026-10-26T00:00:00Z',2,'2026-10-26T00:00:00.234567Z');
		INSERT INTO quota_reset_operations(account_id,request_id,credit_id,identity_hash,dispatch_count,status,result,created_at,completed_at)
		 VALUES(93001,'fixture-succeeded','fixture-credit',encode(sha256(convert_to('fixture-car022-account','UTF8')),'hex'),2,'succeeded','{"windows_reset":2,"code":"ok"}','2026-10-25T23:59:59Z','2026-10-26T00:00:00.234567Z'),
		       (93001,'fixture-unresolved','fixture-credit-unknown',encode(sha256(convert_to('fixture-car022-account','UTF8')),'hex'),1,'pending',NULL,'2026-10-30T12:00:00.123456Z',NULL);
		INSERT INTO carpool_reset_account_monitors(account_id,group_id,identity_hash,source_revision,scope_version,baseline,updated_at)
		 SELECT 93001,92001,encode(sha256(convert_to('fixture-car022-account','UTF8')),'hex'),count(*),1,
		 '{"ObservedAt":"2026-10-30T12:00:00Z","ResetAt":"2026-11-02T00:00:00Z","Utilization":87.5}','2026-10-30T12:00:00.123456Z'
		 FROM carpool_reset_account_changes WHERE account_id=93001;
		INSERT INTO carpool_reset_votes(id,group_id,account_id,created_by,creation_key,eligibility,observation,required_count,status,reason,expires_at,credit_id,request_id,request_hash,dispatched_at,succeeded_at,event_id,created_at,updated_at)
		 VALUES(98001,92001,93001,91001,'00000000-0000-4000-8000-000000098001','{"ScopeVersion":1,"fixture":"electorate"}','{"fixture":"observation"}',2,'completed','receipt','2026-10-27T00:00:00Z','fixture-credit','fixture-succeeded','fixture-receipt-hash','2026-10-25T23:59:59Z','2026-10-26T00:00:00.234567Z',97002,'2026-10-25T12:00:00.123456Z','2026-10-26T00:00:02.345678Z');
		INSERT INTO carpool_reset_vote_approvals(proposal_id,user_id,created_at) VALUES(98001,91001,'2026-10-25T12:00:01.123456Z'),(98001,91002,'2026-10-25T12:00:02.234567Z');
		INSERT INTO carpool_reset_vote_requests(created_by,creation_key,proposal_id) VALUES(91001,'00000000-0000-4000-8000-000000098001',98001);
		INSERT INTO carpool_reset_vote_targets(proposal_id,subscription_id,user_id,group_id,starts_at,binding_revision)
		 SELECT 98001,id,user_id,group_id,starts_at,(SELECT count(*) FROM carpool_reset_group_changes WHERE group_id=92001)
		 FROM user_subscriptions WHERE group_id=92001;
		INSERT INTO usage_group_daily_rollups(bucket_date,group_id,actual_cost,computed_at)
		 VALUES('2026-10-30',92001,0.1111111111,'2026-10-31T00:00:01.123456Z'),('2026-10-29',92002,123.4567890123,'2026-10-31T00:00:02.234567Z');
		UPDATE usage_group_rollup_state SET closed_before='2026-11-01',retained_from='2026-10-01T00:00:00.123456Z',timezone_name='Asia/Shanghai',updated_at='2026-10-31T00:00:03.345678Z' WHERE id=1;
	`)
	require.NoError(t, err)
	if bonus {
		_, err = db.ExecContext(ctx, `UPDATE groups SET carpool_bonus_usd=149.99 WHERE id=92001`)
		require.NoError(t, err)
	}
	if ultrafast {
		_, err = db.ExecContext(ctx, `UPDATE groups SET force_openai_ultrafast=true WHERE id=92001`)
		require.NoError(t, err)
	}
	if rollup {
		_, err = db.ExecContext(ctx, `UPDATE usage_group_rollup_state SET expired_before='2026-10-01T00:00:00Z',building_until='2026-11-05',build_dirty_from='2026-10-29' WHERE id=1`)
		require.NoError(t, err)
	}
	if !trades {
		return
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO carpool_quota_trade_subscription_generations(subscription_id,generation) VALUES(95001,7),(95002,3);
		INSERT INTO carpool_quota_trades(id,group_id,group_name,account_id,identity_hash,source_revision,binding_revision,cycle_id,
		 seller_user_id,seller_name,seller_subscription_id,seller_generation,buyer_user_id,buyer_name,buyer_subscription_id,buyer_generation,
		 amount_cents,request_id,status,terms_version,terms_accepted_at,created_at,reserved_at,completed_at,updated_at)
		 SELECT v.id,s.group_id,s.group_name,s.account_id,CASE WHEN v.id=99008 THEN 'previous-identity' ELSE s.identity_hash END,
		 s.source_revision-CASE WHEN v.status='completed' THEN 1 ELSE 0 END,s.binding_revision-CASE WHEN v.status='completed' THEN 1 ELSE 0 END,
		 CASE WHEN v.id=99006 THEN '93001:97001' ELSE s.cycle_id END,
		 s.user_id,s.user_name,s.subscription_id,s.generation-CASE WHEN v.status='completed' THEN 1 ELSE 0 END,
		 CASE WHEN v.status IN('reserved','completed') THEN b.user_id END,CASE WHEN v.status IN('reserved','completed') THEN b.user_name END,
		 CASE WHEN v.status IN('reserved','completed') THEN b.subscription_id END,CASE WHEN v.status IN('reserved','completed') THEN b.generation END,
		 v.amount,('00000000-0000-4000-8000-'||lpad(v.id::text,12,'0'))::uuid,v.status,'fixture-terms-v1',
		 '2026-10-27T01:02:03.123456Z','2026-10-27T01:02:04.234567Z',
		 CASE WHEN v.status IN('reserved','completed') THEN '2026-10-27T01:02:05.345678Z'::timestamptz END,
		 CASE WHEN v.status='completed' THEN '2026-10-27T01:02:06.456789Z'::timestamptz END,'2026-10-27T01:02:07.567890Z'
		 FROM carpool_quota_trade_subscription_state s CROSS JOIN carpool_quota_trade_subscription_state b
		 CROSS JOIN (VALUES(99001,'completed',1250),(99002,'open',3456),(99003,'reserved',789),(99004,'withdrawn',500),
		                   (99005,'expired',600),(99006,'completed',2345),(99007,'open',111),(99008,'completed',10000)) v(id,status,amount)
		 WHERE s.subscription_id=95001 AND b.subscription_id=95002;
		INSERT INTO carpool_quota_trade_events(trade_id,action,actor_user_id,actor_name,is_admin,buyer_user_id,created_at)
		 SELECT v.trade_id,v.action,v.actor_id,v.actor_name,v.is_admin,v.buyer_id,'2026-10-27T01:02:08.678901Z'::timestamptz+v.seq*interval '1 microsecond'
		 FROM (VALUES
		  (1,99001,'create',91001,'existing seller',false,NULL),(2,99001,'reserve',91002,'existing buyer',false,91002),(3,99001,'confirm',91001,'existing seller',false,91002),
		  (4,99002,'create',91001,'existing seller',false,NULL),(5,99003,'create',91001,'existing seller',false,NULL),(6,99003,'reserve',91002,'existing buyer',false,91002),
		  (7,99004,'create',91001,'existing seller',false,NULL),(8,99004,'reserve',91002,'existing buyer',false,91002),(9,99004,'cancel-reservation',91002,'existing buyer',false,91002),
		  (10,99004,'withdraw',91001,'existing admin',true,NULL),(11,99005,'expire',NULL,'system',false,NULL),
		  (12,99006,'confirm',91001,'existing seller',false,91002),(13,99007,'create',91001,'existing seller',false,NULL),(14,99008,'confirm',91001,'existing seller',false,91002)
		 ) v(seq,trade_id,action,actor_id,actor_name,is_admin,buyer_id);
	`)
	require.NoError(t, err)
	if price {
		_, err = db.ExecContext(ctx, `UPDATE carpool_quota_trades SET sale_price_cents=CASE id
			WHEN 99001 THEN 12345 WHEN 99002 THEN 1 WHEN 99003 THEN 1000000000000 WHEN 99004 THEN 555
			WHEN 99006 THEN 9999 WHEN 99008 THEN 54321 ELSE NULL END`)
		require.NoError(t, err)
	}
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM carpool_quota_trades`).Scan(&count))
	require.Equal(t, 8, count, "trade seeding must not silently select zero state rows")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM carpool_quota_trade_events`).Scan(&count))
	require.Equal(t, 14, count)
}

func assertCar022TradeEntitlements(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var states int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM carpool_quota_trade_subscription_state WHERE eligible AND cycle_id='93001:97002'`).Scan(&states))
	require.Equal(t, 2, states, "both subscriptions retain the latest completed account cycle")
	for _, expected := range []struct {
		subscription, adjustment, frozen int64
	}{
		{95001, -1250, 4356}, // Open + reserved freeze; old cycle/identity settlements do not count.
		{95002, 1250, 0},
	} {
		var adjustment, frozen int64
		var base, used string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT adjustment_cents::bigint,frozen_cents::bigint,base_limit_usd::text,used_usd::text FROM carpool_quota_trade_entitlements WHERE subscription_id=$1`, expected.subscription).Scan(&adjustment, &frozen, &base, &used))
		require.Equal(t, expected.adjustment, adjustment)
		require.Equal(t, expected.frozen, frozen)
		require.Equal(t, "325.25000000", base, "bonus and price must not rewrite the historical weekly cap")
		if expected.subscription == 95001 {
			require.Equal(t, "12.3456789012", used)
		} else {
			require.Equal(t, "23.4567890123", used)
		}
	}
}

func assertCar022RollupNonblocking(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(ctx, `UPDATE usage_group_rollup_state SET closed_before='2026-11-01',expired_before='2026-10-01T00:00:00Z',building_until='2026-11-05',build_dirty_from=NULL WHERE id=1`)
	require.NoError(t, err)
	reader, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = reader.Rollback() }()
	var id int
	require.NoError(t, reader.QueryRowContext(ctx, `SELECT id FROM usage_group_rollup_state WHERE id=1 FOR KEY SHARE`).Scan(&id))
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	writer, err := db.BeginTx(writeCtx, nil)
	require.NoError(t, err)
	defer func() { _ = writer.Rollback() }()
	_, err = writer.ExecContext(writeCtx, `SET LOCAL lock_timeout='1500ms'; SET LOCAL statement_timeout='5s'; SET LOCAL TIME ZONE 'UTC'`)
	require.NoError(t, err)
	// The previous 223 UPDATE trigger requested FOR UPDATE and would time out
	// behind the reader. 251 uses compatible KEY SHARE, and marks the build dirty.
	_, err = writer.ExecContext(writeCtx, `UPDATE usage_logs SET actual_cost=actual_cost+0.0000000001 WHERE id=96001`)
	require.NoError(t, err, "upgraded UPDATE trigger must not wait for a build's KEY SHARE lock")
	var closed, dirty string
	require.NoError(t, writer.QueryRowContext(writeCtx, `SELECT closed_before::text,build_dirty_from::text FROM usage_group_rollup_state WHERE id=1`).Scan(&closed, &dirty))
	require.Equal(t, "2026-10-30", closed)
	require.Equal(t, "2026-10-30", dirty)
	// Expired inserts are ignored, whereas retained late inserts dirty the build.
	_, err = writer.ExecContext(writeCtx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,group_id,request_id,model,created_at)
	 VALUES(91001,94001,93001,92001,'fixture-car022-expired','gpt-5.5','2026-09-30T00:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, writer.QueryRowContext(writeCtx, `SELECT closed_before::text,build_dirty_from::text FROM usage_group_rollup_state WHERE id=1`).Scan(&closed, &dirty))
	require.Equal(t, "2026-10-30", closed)
	require.Equal(t, "2026-10-30", dirty)
	_, err = writer.ExecContext(writeCtx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,group_id,request_id,model,created_at)
	 VALUES(91001,94001,93001,92001,'fixture-car022-late','gpt-5.5','2026-10-29T00:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, writer.QueryRowContext(writeCtx, `SELECT closed_before::text,build_dirty_from::text FROM usage_group_rollup_state WHERE id=1`).Scan(&closed, &dirty))
	require.Equal(t, "2026-10-29", closed)
	require.Equal(t, "2026-10-29", dirty)
	require.NoError(t, writer.Rollback(), "smoke writes must not alter preserved trade/usage fixtures")
}
