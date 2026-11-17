package repository

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func quotaTradePostgres(t *testing.T) (*sql.DB, *carpoolQuotaTradeRepository) {
	t.Helper()
	return quotaTradePostgresSchema(t, true)
}

func quotaTradePostgresSchema(t *testing.T, withPrice bool) (*sql.DB, *carpoolQuotaTradeRepository) {
	t.Helper()
	dsn := os.Getenv("CARPOOL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires isolated CARPOOL_TEST_DATABASE_URL")
	}
	root, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("quota_trade_test_%d", time.Now().UnixNano())
	_, err = root.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = db.Close(); _, _ = root.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); _ = root.Close() })
	_, err = db.Exec(`
 CREATE TABLE users(id BIGINT PRIMARY KEY,username TEXT NOT NULL,role TEXT NOT NULL DEFAULT 'user',status TEXT NOT NULL DEFAULT 'active',deleted_at TIMESTAMPTZ);
 CREATE TABLE groups(id BIGINT PRIMARY KEY,name TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'active',platform TEXT NOT NULL DEFAULT 'openai',subscription_type TEXT NOT NULL DEFAULT 'subscription',carpool_members INT NOT NULL DEFAULT 3,weekly_limit_usd NUMERIC NOT NULL DEFAULT 100,deleted_at TIMESTAMPTZ);
 CREATE TABLE accounts(id BIGINT PRIMARY KEY,platform TEXT DEFAULT 'openai',type TEXT DEFAULT 'oauth',status TEXT DEFAULT 'active',parent_account_id BIGINT,auto_pause_on_expired BOOLEAN DEFAULT FALSE,expires_at TIMESTAMPTZ,deleted_at TIMESTAMPTZ,credentials JSONB);
 CREATE TABLE account_groups(account_id BIGINT,group_id BIGINT,PRIMARY KEY(account_id,group_id));
 CREATE TABLE user_subscriptions(id BIGINT PRIMARY KEY,user_id BIGINT,group_id BIGINT,status TEXT NOT NULL DEFAULT 'active',starts_at TIMESTAMPTZ DEFAULT now()-interval '1 day',expires_at TIMESTAMPTZ DEFAULT now()+interval '20 days',deleted_at TIMESTAMPTZ,weekly_usage_usd NUMERIC NOT NULL DEFAULT 0);
 CREATE TABLE carpool_reset_monitor(id SMALLINT PRIMARY KEY);
 CREATE TABLE carpool_reset_account_changes(id BIGSERIAL,account_id BIGINT);
 CREATE TABLE carpool_reset_group_changes(id BIGSERIAL,group_id BIGINT);
 CREATE TABLE carpool_subscription_reset_events(id BIGSERIAL PRIMARY KEY,account_id BIGINT,identity_hash TEXT,status TEXT,observed_at TIMESTAMPTZ DEFAULT clock_timestamp());
 CREATE TABLE quota_reset_operations(account_id BIGINT,status TEXT);
 CREATE TABLE carpool_reset_votes(account_id BIGINT,status TEXT);
 CREATE TABLE carpool_account_reset_receipts(account_id BIGINT,identity_hash TEXT,occurred_at TIMESTAMPTZ);
 CREATE TABLE api_keys(key TEXT,group_id BIGINT,deleted_at TIMESTAMPTZ);
 CREATE TABLE auth_cache_invalidation_outbox(cache_key TEXT PRIMARY KEY);
 INSERT INTO users(id,username,role) VALUES(1,'seller','user'),(2,'buyer','user'),(3,'buyer2','user'),(4,'admin','admin'),(5,'outsider','user');
 INSERT INTO groups(id,name) VALUES(10,'Carpool'),(20,'Elsewhere');
 INSERT INTO accounts(id,credentials) VALUES(9,'{"chatgpt_account_id":"upstream"}'),(19,'{"chatgpt_account_id":"outside"}');
 INSERT INTO account_groups VALUES(9,10),(19,20);
 INSERT INTO user_subscriptions(id,user_id,group_id) VALUES(11,1,10),(12,2,10),(13,3,10),(15,5,20);
 INSERT INTO api_keys VALUES('test-trade-key',10,NULL);
 `)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("252_custom_carpool_quota_trades.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err, "migration is repeatable")
	if withPrice {
		quotaTradeApplyPriceMigration(t, db)
	}
	return db, &carpoolQuotaTradeRepository{db: db}
}

func quotaTradeApplyPriceMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	migration, err := migrations.FS.ReadFile("253_custom_carpool_quota_trade_price.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err, "price migration is repeatable")
}

func tradeActor(user, sub int64) service.CarpoolQuotaTradeActor {
	return service.CarpoolQuotaTradeActor{UserID: user, SubscriptionID: sub}
}
func quotaTradeAssertBalance(t *testing.T, db *sql.DB, sub int64, adj, frozen float64) {
	t.Helper()
	var a, f float64
	require.NoError(t, db.QueryRow(`SELECT adjustment_cents/100.0,frozen_cents/100.0 FROM carpool_quota_trade_entitlements WHERE subscription_id=$1`, sub).Scan(&a, &f))
	require.Equal(t, adj, a)
	require.Equal(t, frozen, f)
}
func TestCarpoolQuotaTradePostgresPriceMigration(t *testing.T) {
	db, r := quotaTradePostgresSchema(t, false) // A real 252-only schema with an old order.
	ctx := context.Background()
	key := uuid.NewString()
	var oldID int64
	require.NoError(t, db.QueryRow(`INSERT INTO carpool_quota_trades(group_id,group_name,account_id,identity_hash,source_revision,binding_revision,cycle_id,
 seller_user_id,seller_name,seller_subscription_id,seller_generation,amount_cents,request_id,terms_version)
 SELECT group_id,group_name,account_id,identity_hash,source_revision,binding_revision,cycle_id,user_id,user_name,subscription_id,generation,1000,$1,$2
 FROM carpool_quota_trade_subscription_state WHERE subscription_id=11 RETURNING id`, key, service.CarpoolQuotaTradeTermsVersion).Scan(&oldID))
	var before, after string
	require.NoError(t, db.QueryRow(`SELECT row_to_json(t)::text FROM carpool_quota_trades t WHERE id=$1`, oldID).Scan(&before))
	quotaTradeApplyPriceMigration(t, db)
	require.NoError(t, db.QueryRow(`SELECT (to_jsonb(t)-'sale_price_cents')::text FROM carpool_quota_trades t WHERE id=$1`, oldID).Scan(&after))
	require.JSONEq(t, before, after, "additive migration must preserve all existing order fields")
	old, err := quotaTradeGet(ctx, db, oldID)
	require.NoError(t, err)
	require.Nil(t, old.SalePriceCNY, "never invent a historical price from suggestions")
	_, err = r.Create(ctx, 1, 11, 1000, 1, key)
	require.Error(t, err)
	require.Equal(t, 409, infraerrors.Code(err), "legacy null price cannot be filled by reusing its request ID")
	quotaTradeAssertBalance(t, db, 11, 0, 10)
	for _, cents := range []int64{0, -1, service.CarpoolQuotaTradeMaxSalePriceCents + 1} {
		_, err = db.Exec(`UPDATE carpool_quota_trades SET sale_price_cents=$1 WHERE id=$2`, cents, oldID)
		require.Error(t, err, "DB CHECK must reject invalid price %d", cents)
		_, err = r.Create(ctx, 1, 11, 100, cents, uuid.NewString())
		require.Error(t, err, "repository must reject invalid price before freezing quota")
		require.Equal(t, 400, infraerrors.Code(err))
	}
	newOrder, err := r.Create(ctx, 1, 11, 100, service.CarpoolQuotaTradeMaxSalePriceCents, uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, newOrder.SalePriceCNY)
	require.Equal(t, 10000000000.0, *newOrder.SalePriceCNY)
	var stored int64
	require.NoError(t, db.QueryRow(`SELECT sale_price_cents FROM carpool_quota_trades WHERE id=$1`, newOrder.ID).Scan(&stored))
	require.Equal(t, service.CarpoolQuotaTradeMaxSalePriceCents, stored)
	penny, err := r.Create(ctx, 1, 11, 100, 1, uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, 0.01, *penny.SalePriceCNY)
	quotaTradeAssertBalance(t, db, 11, 0, 12)
	current, err := r.Market(ctx, 1, 11, 1, 20)
	require.NoError(t, err)
	require.Len(t, current.Items, 3)
	require.Nil(t, current.Items[2].SalePriceCNY)
	old, err = r.Act(ctx, tradeActor(1, 11), oldID, "withdraw")
	require.NoError(t, err)
	require.Nil(t, old.SalePriceCNY)
	penny, err = r.Act(ctx, tradeActor(1, 11), penny.ID, "withdraw")
	require.NoError(t, err)
	require.Equal(t, 0.01, *penny.SalePriceCNY)
	history, err := r.Market(ctx, 1, 11, 1, 20, "history")
	require.NoError(t, err)
	require.Len(t, history.Items, 2)
	require.Equal(t, 0.01, *history.Items[0].SalePriceCNY)
	require.Nil(t, history.Items[1].SalePriceCNY)
	admin, err := r.List(ctx, 4, service.CarpoolQuotaTradeFilter{Page: 1, PageSize: 20})
	require.NoError(t, err)
	require.Len(t, admin.Items, 3)
	require.Nil(t, admin.Items[2].SalePriceCNY)
	quotaTradeAssertBalance(t, db, 11, 0, 1)
}

func TestCarpoolQuotaTradePostgresLifecycle(t *testing.T) {
	db, r := quotaTradePostgres(t)
	ctx := context.Background()
	key := uuid.NewString()
	_, err := r.Create(ctx, 1, 11, 10000, 12345, uuid.NewString())
	require.ErrorIs(t, err, service.ErrQuotaTradeInsufficient, "strictly greater, not greater/equal")
	order, err := r.Create(ctx, 1, 11, 9500, 12345, key)
	require.NoError(t, err, "no extra ten dollar reserve")
	require.Equal(t, "open", order.Status)
	require.NotNil(t, order.SalePriceCNY)
	require.Equal(t, 123.45, *order.SalePriceCNY)
	current, err := r.Market(ctx, 2, 12, 1, 20, "current")
	require.NoError(t, err)
	require.Len(t, current.Items, 1)
	require.Equal(t, order.SalePriceCNY, current.Items[0].SalePriceCNY)
	quotaTradeAssertBalance(t, db, 11, 0, 95)
	replay, err := r.Create(ctx, 1, 11, 9500, 12345, key)
	require.NoError(t, err)
	require.Equal(t, order.ID, replay.ID)
	require.Equal(t, order.SalePriceCNY, replay.SalePriceCNY)
	_, err = r.Create(ctx, 1, 11, 9500, 12346, key)
	require.Error(t, err)
	require.Equal(t, 409, infraerrors.Code(err), "same request with changed sale price must conflict")
	quotaTradeAssertBalance(t, db, 11, 0, 95)
	var creates int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM carpool_quota_trade_events WHERE trade_id=$1 AND action='create'`, order.ID).Scan(&creates))
	require.Equal(t, 1, creates, "repricing a retry must not create another event or freeze")
	_, err = r.Create(ctx, 1, 11, 9400, 12345, key)
	require.Error(t, err)
	_, err = r.Create(ctx, 1, 11, 600, 12345, uuid.NewString())
	require.ErrorIs(t, err, service.ErrQuotaTradeInsufficient)
	_, err = r.Act(ctx, tradeActor(5, 15), order.ID, "reserve")
	require.Error(t, err, "cross-group forbidden")
	_, err = r.Act(ctx, tradeActor(1, 11), order.ID, "reserve")
	require.Error(t, err, "self buy forbidden")
	order, err = r.Act(ctx, tradeActor(2, 12), order.ID, "reserve")
	require.NoError(t, err)
	require.Equal(t, "reserved", order.Status)
	require.Equal(t, 123.45, *order.SalePriceCNY)
	_, err = r.Act(ctx, tradeActor(1, 11), order.ID, "withdraw")
	require.Error(t, err)
	_, err = r.Act(ctx, tradeActor(2, 12), order.ID, "confirm")
	require.Error(t, err, "buyer cannot confirm")
	_, err = r.Act(ctx, tradeActor(2, 12), order.ID, "cancel-reservation")
	require.Error(t, err)
	admin := service.CarpoolQuotaTradeActor{UserID: 4, IsAdmin: true}
	order, err = r.Act(ctx, admin, order.ID, "cancel-reservation")
	require.NoError(t, err)
	require.Nil(t, order.BuyerUserID)
	require.Nil(t, order.ReservedAt)
	require.Equal(t, 123.45, *order.SalePriceCNY)
	quotaTradeAssertBalance(t, db, 11, 0, 95)
	order, err = r.Act(ctx, tradeActor(3, 13), order.ID, "reserve")
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE groups SET weekly_limit_usd=10 WHERE id=10;UPDATE user_subscriptions SET weekly_usage_usd=50 WHERE id=11`)
	require.NoError(t, err)
	order, err = r.Act(ctx, admin, order.ID, "confirm")
	require.NoError(t, err, "frozen quota settles despite lower base and higher usage")
	require.Equal(t, "completed", order.Status)
	require.Equal(t, 123.45, *order.SalePriceCNY)
	_, err = r.Act(ctx, tradeActor(1, 11), order.ID, "confirm")
	require.NoError(t, err, "idempotent confirm")
	quotaTradeAssertBalance(t, db, 11, -95, 0)
	quotaTradeAssertBalance(t, db, 13, 95, 0)
	events, err := r.Events(ctx, 4, order.ID)
	require.NoError(t, err)
	require.Len(t, events, 5)
	require.True(t, events[4].IsAdmin)
	_, err = r.Events(ctx, 2, order.ID)
	require.Error(t, err)
	market, err := r.Market(ctx, 2, 12, 1, 20)
	require.NoError(t, err)
	require.Empty(t, market.Items, "other buyer cannot see completed details")
	market, err = r.Market(ctx, 3, 13, 1, 20, "history")
	require.NoError(t, err)
	require.Len(t, market.Items, 1)
	require.Equal(t, 105.0, market.Quota.AvailableUSD)
	require.Equal(t, order.SalePriceCNY, market.Items[0].SalePriceCNY)
	adminPage, err := r.List(ctx, 4, service.CarpoolQuotaTradeFilter{Page: 1, PageSize: 20})
	require.NoError(t, err)
	require.Len(t, adminPage.Items, 1)
	require.Equal(t, order.SalePriceCNY, adminPage.Items[0].SalePriceCNY)
	var cache int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM auth_cache_invalidation_outbox`).Scan(&cache))
	require.Equal(t, 1, cache)
	subs := []*service.UserSubscription{{ID: 11, Group: &service.Group{Platform: "openai", SubscriptionType: "subscription", CarpoolMembers: 3, WeeklyLimitUSD: new(float64)}}}
	oldGroup := subs[0].Group
	require.NoError(t, loadCarpoolQuotaTradeEntitlements(ctx, db, subs))
	require.NotSame(t, oldGroup, subs[0].Group)
	require.Equal(t, 10.0, *subs[0].Group.WeeklyLimitUSD)
	require.Equal(t, -95.0, subs[0].QuotaAdjustmentUSD)
}
func TestCarpoolQuotaTradePostgresMarketViews(t *testing.T) {
	db, r := quotaTradePostgres(t)
	ctx := context.Background()
	st, err := quotaTradeSubscription(ctx, db, 11)
	require.NoError(t, err)
	insert := func(seller, buyer int64, status string) int64 {
		t.Helper()
		var buyerUser, buyerSub, buyerGen any
		if buyer != 0 {
			buyerUser, buyerSub, buyerGen = buyer, buyer+10, st.Generation
		}
		group, account := st.GroupID, st.AccountID
		if seller == 5 {
			group, account = 20, 19
		}
		var id int64
		err := db.QueryRow(`INSERT INTO carpool_quota_trades(group_id,group_name,account_id,identity_hash,source_revision,binding_revision,cycle_id,
 seller_user_id,seller_name,seller_subscription_id,seller_generation,buyer_user_id,buyer_name,buyer_subscription_id,buyer_generation,
 amount_cents,request_id,status,terms_version)
 VALUES($1,'fixture',$2,$3,$4,$5,$6,$7,'seller',$8,$9,$10,'buyer',$11,$12,100,$13,$14,$15) RETURNING id`,
			group, account, st.IdentityHash, st.SourceRevision, st.BindingRevision, st.CycleID, seller, seller+10, st.Generation,
			buyerUser, buyerSub, buyerGen, uuid.NewString(), status, service.CarpoolQuotaTradeTermsVersion).Scan(&id)
		require.NoError(t, err)
		return id
	}
	ownOpen := insert(1, 0, "open")
	otherOpen := insert(2, 0, "open")
	ownReserved := insert(1, 2, "reserved")
	buyReserved := insert(2, 1, "reserved")
	privateReserved := insert(2, 3, "reserved")
	completed := insert(1, 2, "completed")
	withdrawn := insert(1, 0, "withdrawn")
	expired := insert(2, 1, "expired")
	privateCompleted := insert(2, 3, "completed")
	outside := insert(5, 0, "open")
	// All historical cycles remain visible, including a purchase in an old cycle.
	_, err = db.Exec(`UPDATE carpool_quota_trades SET cycle_id='old-cycle' WHERE id IN($1,$2,$3)`, completed, withdrawn, expired)
	require.NoError(t, err)
	var stale []int64
	for _, change := range []string{
		`seller_generation=seller_generation+1`, `account_id=999`, `identity_hash='old-identity'`,
		`cycle_id='old-cycle'`, `source_revision=source_revision+1`, `binding_revision=binding_revision+1`,
		`seller_user_id=3`, `seller_subscription_id=999`,
	} {
		id := insert(1, 0, "open")
		_, err = db.Exec(`UPDATE carpool_quota_trades SET `+change+` WHERE id=$1`, id)
		require.NoError(t, err)
		// Changing the seller user removes participation; don't expose this row.
		if change != `seller_user_id=3` {
			stale = append(stale, id)
		}
	}
	for _, change := range []string{
		`buyer_generation=buyer_generation+1`, `buyer_subscription_id=999`, `buyer_user_id=3`,
		`seller_generation=seller_generation+1`,
	} {
		id := insert(1, 2, "reserved")
		_, err = db.Exec(`UPDATE carpool_quota_trades SET `+change+` WHERE id=$1`, id)
		require.NoError(t, err)
		stale = append(stale, id)
	}
	assertPages := func(view string, want []int64) {
		t.Helper()
		// The newest rows are stale and terminal. Full pages and exact totals
		// therefore prove filtering occurs before SQL LIMIT/OFFSET, not after it.
		var got []int64
		for page := 1; page <= (len(want)+1)/2+1; page++ {
			market, err := r.Market(ctx, 1, 11, page, 2, view)
			require.NoError(t, err)
			require.Equal(t, int64(len(want)), market.Total)
			require.Equal(t, page, market.Page)
			require.Equal(t, 2, market.PageSize)
			require.True(t, market.Enabled)
			require.Equal(t, service.CarpoolQuotaTradeTermsVersion, market.TermsVersion)
			require.Equal(t, service.CarpoolQuotaTradeTermsNotice, market.TermsNotice)
			remaining := max(0, len(want)-(page-1)*2)
			require.Len(t, market.Items, min(2, remaining))
			for _, item := range market.Items {
				require.NotNil(t, item.CanAction)
				require.Equal(t, view == "current", *item.CanAction)
				got = append(got, item.ID)
			}
		}
		require.ElementsMatch(t, want, got)
		for i := 1; i < len(got); i++ {
			require.Greater(t, got[i-1], got[i], "stable descending ID ordering")
		}
	}
	current := []int64{ownOpen, otherOpen, ownReserved, buyReserved}
	history := append([]int64{completed, withdrawn, expired}, stale...)
	assertPages("current", current)
	assertPages("history", history)
	market, err := r.Market(ctx, 1, 11, 1, 100)
	require.NoError(t, err)
	require.Equal(t, int64(len(current)), market.Total, "old call signature defaults to current")
	// Histories also use the same quota snapshot, and listing is read-only.
	var before string
	require.NoError(t, db.QueryRow(`SELECT row_to_json(q)::text FROM carpool_quota_trade_entitlements q WHERE subscription_id=11`).Scan(&before))
	var eventsBefore int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM carpool_quota_trade_events`).Scan(&eventsBefore))
	market, err = r.Market(ctx, 1, 11, 1, 100, "history")
	require.NoError(t, err)
	for _, item := range market.Items {
		if item.ID == stale[0] {
			require.Equal(t, "open", item.Status, "retain the actual ledger status, with can_action=false")
		}
	}
	var after string
	require.NoError(t, db.QueryRow(`SELECT row_to_json(q)::text FROM carpool_quota_trade_entitlements q WHERE subscription_id=11`).Scan(&after))
	require.Equal(t, before, after)
	var eventsAfter int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM carpool_quota_trade_events`).Scan(&eventsAfter))
	require.Equal(t, eventsBefore, eventsAfter)
	// Administrator list is still unfiltered and has no user-only action field.
	admin, err := r.List(ctx, 4, service.CarpoolQuotaTradeFilter{Page: 1, PageSize: 100})
	require.NoError(t, err)
	var adminIDs []int64
	for _, item := range admin.Items {
		require.Nil(t, item.CanAction)
		adminIDs = append(adminIDs, item.ID)
	}
	require.Contains(t, adminIDs, privateReserved)
	require.Contains(t, adminIDs, privateCompleted)
	require.Contains(t, adminIDs, outside)
}

func TestCarpoolQuotaTradePostgresMarketEligibility(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"seller_generation", `UPDATE user_subscriptions SET starts_at=starts_at-interval '1 hour' WHERE id=11`},
		{"seller_disabled", `UPDATE users SET status='inactive' WHERE id=1`},
		{"seller_expired", `UPDATE user_subscriptions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=11`},
		{"buyer_generation", `UPDATE user_subscriptions SET starts_at=starts_at-interval '1 hour' WHERE id=12`},
		{"buyer_expired", `UPDATE user_subscriptions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=12`},
		{"buyer_revoked", `UPDATE user_subscriptions SET status='revoked' WHERE id=12`},
		{"buyer_disabled", `UPDATE users SET status='inactive' WHERE id=2`},
		{"buyer_reassigned", `UPDATE user_subscriptions SET group_id=20 WHERE id=12`},
		{"identity", `UPDATE accounts SET credentials='{"chatgpt_account_id":"replacement"}' WHERE id=9`},
		{"account", `DELETE FROM account_groups WHERE group_id=10; INSERT INTO account_groups VALUES(19,10)`},
		{"source", `INSERT INTO carpool_reset_account_changes(account_id) VALUES(9)`},
		{"binding", `INSERT INTO carpool_reset_group_changes(group_id) VALUES(10)`},
		{"ineligible_account", `UPDATE accounts SET status='inactive' WHERE id=9`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, r := quotaTradePostgres(t)
			ctx := context.Background()
			order, err := r.Create(ctx, 1, 11, 1000, 12345, uuid.NewString())
			require.NoError(t, err)
			_, err = r.Act(ctx, tradeActor(2, 12), order.ID, "reserve")
			require.NoError(t, err)
			var completed int64
			// A settled row stays history regardless of either party's later eligibility.
			require.NoError(t, db.QueryRow(`INSERT INTO carpool_quota_trades(group_id,group_name,account_id,identity_hash,source_revision,binding_revision,cycle_id,seller_user_id,seller_name,seller_subscription_id,seller_generation,buyer_user_id,buyer_name,buyer_subscription_id,buyer_generation,amount_cents,request_id,status,terms_version)
 SELECT group_id,group_name,account_id,identity_hash,source_revision,binding_revision,cycle_id,seller_user_id,seller_name,seller_subscription_id,seller_generation,buyer_user_id,buyer_name,buyer_subscription_id,buyer_generation,100,$2,'completed',terms_version FROM carpool_quota_trades WHERE id=$1 RETURNING id`, order.ID, uuid.NewString()).Scan(&completed))
			_, err = db.Exec(tc.change)
			require.NoError(t, err)
			// A third eligible member must never see another buyer's reservation.
			current, err := r.Market(ctx, 3, 13, 1, 20)
			require.NoError(t, err)
			require.Zero(t, current.Total)
			history, err := r.Market(ctx, 3, 13, 1, 20, "history")
			require.NoError(t, err)
			require.Zero(t, history.Total)
			// Seller-disabled cases use the still-active buyer; otherwise use seller.
			user, sub := int64(1), int64(11)
			if tc.name == "seller_disabled" {
				user, sub = 2, 12
			}
			current, err = r.Market(ctx, user, sub, 1, 20)
			require.NoError(t, err)
			require.Zero(t, current.Total)
			history, err = r.Market(ctx, user, sub, 1, 20, "history")
			require.NoError(t, err)
			require.Equal(t, int64(2), history.Total)
			require.Len(t, history.Items, 2)
			require.Equal(t, completed, history.Items[0].ID)
			require.Equal(t, "completed", history.Items[0].Status)
			require.Equal(t, order.ID, history.Items[1].ID)
			require.Equal(t, "reserved", history.Items[1].Status)
			for _, item := range history.Items {
				require.NotNil(t, item.CanAction)
				require.False(t, *item.CanAction)
			}
			if tc.name == "buyer_generation" || tc.name == "buyer_expired" || tc.name == "buyer_revoked" || tc.name == "buyer_disabled" {
				quotaTradeAssertBalance(t, db, 11, -1, 10)
			}
		})
	}
}

func TestCarpoolQuotaTradePostgresConcurrency(t *testing.T) {
	db, r := quotaTradePostgres(t)
	ctx := context.Background()
	order, err := r.Create(ctx, 1, 11, 6000, 12345, uuid.NewString())
	require.NoError(t, err)
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, a := range []service.CarpoolQuotaTradeActor{tradeActor(2, 12), tradeActor(3, 13)} {
		wg.Add(1)
		go func(a service.CarpoolQuotaTradeActor) {
			defer wg.Done()
			<-start
			if _, err := r.Act(ctx, a, order.ID, "reserve"); err == nil {
				wins.Add(1)
			}
		}(a)
	}
	close(start)
	wg.Wait()
	require.Equal(t, int32(1), wins.Load())
	quotaTradeAssertBalance(t, db, 11, 0, 60)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM carpool_quota_trade_events WHERE trade_id=$1 AND action='reserve'`, order.ID).Scan(&count))
	require.Equal(t, 1, count)
	// Two concurrent new listings cannot oversell the same subscription.
	start = make(chan struct{})
	wins.Store(0)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, e := r.Create(ctx, 1, 11, 3000, 12345, uuid.NewString()); e == nil {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.Equal(t, int32(1), wins.Load())
	quotaTradeAssertBalance(t, db, 11, 0, 90)
}
func TestCarpoolQuotaTradePostgresCycleAndGenerations(t *testing.T) {
	db, r := quotaTradePostgres(t)
	ctx := context.Background()
	admin := service.CarpoolQuotaTradeActor{UserID: 4, IsAdmin: true}
	newReserved := func() *service.CarpoolQuotaTrade {
		o, e := r.Create(ctx, 1, 11, 2000, 12345, uuid.NewString())
		require.NoError(t, e)
		o, e = r.Act(ctx, tradeActor(2, 12), o.ID, "reserve")
		require.NoError(t, e)
		return o
	}
	order := newReserved()
	_, err := db.Exec(`UPDATE user_subscriptions SET expires_at=expires_at+interval '1 day' WHERE id=11`)
	require.NoError(t, err)
	_, err = r.Act(ctx, admin, order.ID, "confirm")
	require.NoError(t, err, "ordinary active renewal retains generation")
	order = newReserved()
	_, err = db.Exec(`UPDATE user_subscriptions SET status='revoked' WHERE id=12;UPDATE user_subscriptions SET status='active' WHERE id=12`)
	require.NoError(t, err)
	_, err = r.Act(ctx, admin, order.ID, "confirm")
	require.Error(t, err, "revoked/restored buyer must not settle old intent")
	_, err = r.Act(ctx, admin, order.ID, "cancel-reservation")
	require.NoError(t, err)
	_, err = r.Act(ctx, admin, order.ID, "withdraw")
	require.NoError(t, err)
	order = newReserved()
	_, err = db.Exec(`INSERT INTO carpool_reset_group_changes(group_id) VALUES(10)`)
	require.NoError(t, err)
	_, err = r.Act(ctx, admin, order.ID, "confirm")
	require.Error(t, err, "binding generation prevents changed-back settlement")
	quotaTradeAssertBalance(t, db, 11, -20, 0) // completed sales do not silently refund after source revision
	_, err = r.Act(ctx, admin, order.ID, "cancel-reservation")
	require.NoError(t, err)
	_, err = r.Act(ctx, admin, order.ID, "withdraw")
	require.NoError(t, err)
	order = newReserved()
	identity := service.CarpoolResetIdentityHash("upstream")
	_, err = db.Exec(`INSERT INTO carpool_subscription_reset_events(account_id,identity_hash,status) VALUES(9,$1,'pending')`, identity)
	require.NoError(t, err)
	_, err = r.Act(ctx, admin, order.ID, "confirm")
	require.ErrorIs(t, err, service.ErrQuotaTradeUnavailable)
	_, err = db.Exec(`UPDATE carpool_subscription_reset_events SET status='completed' WHERE account_id=9`)
	require.NoError(t, err)
	stale, err := quotaTradeGet(ctx, db, order.ID)
	require.NoError(t, err)
	require.Equal(t, "expired", stale.Status)
	quotaTradeAssertBalance(t, db, 11, 0, 0)
	quotaTradeAssertBalance(t, db, 12, 0, 0)
	_, err = r.Act(ctx, admin, order.ID, "confirm")
	require.Error(t, err, "old cycle cannot settle")
	events, err := r.Events(ctx, 4, order.ID)
	require.NoError(t, err)
	require.Equal(t, "expire", events[len(events)-1].Action)
	_, err = r.Act(ctx, admin, order.ID, "cancel-reservation")
	require.NoError(t, err, "admin can clean stale buyer")
	_, err = r.Act(ctx, admin, order.ID, "withdraw")
	require.NoError(t, err, "admin can clean old order")
	order = newReserved()
	_, err = db.Exec(`INSERT INTO carpool_account_reset_receipts(account_id,identity_hash,occurred_at) VALUES(9,$1,clock_timestamp())`, identity)
	require.NoError(t, err)
	_, err = r.Act(ctx, admin, order.ID, "confirm")
	require.ErrorIs(t, err, service.ErrQuotaTradeUnavailable, "receipt ahead of ApplyEvent blocks stale settlement")
	_, err = r.Create(ctx, 1, 11, 100, 12345, uuid.NewString())
	require.ErrorIs(t, err, service.ErrQuotaTradeUnavailable)
}
