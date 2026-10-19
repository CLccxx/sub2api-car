package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestCarpoolPostgres(t *testing.T) {
	dsn := os.Getenv("CARPOOL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires an isolated CARPOOL_TEST_DATABASE_URL")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	schema := fmt.Sprintf("carpool_test_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, `CREATE SCHEMA `+schema+`; SET search_path TO `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`); _ = db.Close() })
	_, err = db.ExecContext(ctx, `
		CREATE TABLE groups (id bigint PRIMARY KEY, status text, platform text, subscription_type text,
		 updated_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz, weekly_limit_usd numeric(20,8));
		CREATE TABLE accounts (id bigint PRIMARY KEY, platform text, type text, status text, parent_account_id bigint,
		 credentials jsonb, deleted_at timestamptz, auto_pause_on_expired boolean DEFAULT false, expires_at timestamptz);
		CREATE TABLE account_groups (account_id bigint, group_id bigint, PRIMARY KEY(account_id, group_id));
		CREATE TABLE api_keys (id bigint PRIMARY KEY, key text, group_id bigint, deleted_at timestamptz);
		CREATE TABLE auth_cache_invalidation_outbox (cache_key text);
		CREATE TABLE scheduler_outbox (id bigserial PRIMARY KEY, event_type text, account_id bigint, group_id bigint, payload jsonb, dedup_key text);
		CREATE UNIQUE INDEX scheduler_dedup ON scheduler_outbox(dedup_key) WHERE dedup_key IS NOT NULL;
		CREATE TABLE user_subscriptions (id bigint PRIMARY KEY, weekly_usage_usd numeric, weekly_window_start timestamptz);
	`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("237_custom_group_carpool.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err, "migration is idempotent")
	_, err = db.ExecContext(ctx, `
		INSERT INTO groups (id,status,platform,subscription_type,carpool_members,weekly_limit_usd) VALUES (10,'active','openai','subscription',4,400);
		INSERT INTO accounts (id,platform,type,status,credentials) VALUES (9,'openai','oauth','active','{}');
		INSERT INTO account_groups VALUES (9,10);
		INSERT INTO api_keys VALUES (1,'test-key',10,NULL);
		INSERT INTO user_subscriptions VALUES (1,350,'2026-10-01T00:00:00Z');
	`)
	require.NoError(t, err)
	repo := NewCarpoolQuotaRepository(db)
	slot := time.Now().UTC().Truncate(time.Second)
	targets, err := repo.ListPending(ctx, slot)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	account := &service.Account{ID: 9, Credentials: map[string]any{}}
	ok, err := repo.Apply(ctx, targets[0], account, 325, slot)
	require.NoError(t, err)
	require.True(t, ok)
	var limit, usage float64
	var window time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT weekly_limit_usd FROM groups WHERE id=10`).Scan(&limit))
	require.Equal(t, 325.0, limit)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT weekly_usage_usd, weekly_window_start FROM user_subscriptions WHERE id=1`).Scan(&usage, &window))
	require.Equal(t, 350.0, usage)
	require.Equal(t, "2026-10-01T00:00:00Z", window.UTC().Format(time.RFC3339))
	for _, table := range []string{"auth_cache_invalidation_outbox", "scheduler_outbox"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count))
		require.Equal(t, 1, count)
	}
	ok, err = repo.Apply(ctx, targets[0], account, 400, slot)
	require.NoError(t, err)
	require.False(t, ok, "same slot/revision cannot be applied twice")
	targets, err = repo.ListPending(ctx, slot)
	require.NoError(t, err)
	require.Empty(t, targets)
	slot = slot.Add(6 * time.Hour)
	targets, err = repo.ListPending(ctx, slot)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	_, err = db.ExecContext(ctx, `INSERT INTO accounts (id,platform,type,status,credentials) VALUES (11,'openai','apikey','inactive','{}'); INSERT INTO account_groups VALUES (11,10)`)
	require.NoError(t, err)
	ok, err = repo.Apply(ctx, targets[0], account, 400, slot)
	require.NoError(t, err)
	require.False(t, ok, "disabled extra bindings must prevent ambiguous updates")
	_, err = db.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=11; UPDATE groups SET carpool_members=3,updated_at=now() WHERE id=10`)
	require.NoError(t, err)
	ok, err = repo.Apply(ctx, targets[0], account, 400, slot)
	require.NoError(t, err)
	require.False(t, ok, "editing the size invalidates an in-flight estimate")
}
