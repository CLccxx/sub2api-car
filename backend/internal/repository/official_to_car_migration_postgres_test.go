package repository

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/alicebob/miniredis/v2"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Historical official SQL explicitly references public (including trigger
// functions), so search_path alone does not isolate a full migration rehearsal.
// Use a disposable DATABASE on the opt-in test server, preserving the SQL bytes
// and checksums exactly. Never point CARPOOL_TEST_DATABASE_URL at production.
func officialToCarTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CARPOOL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires an isolated CARPOOL_TEST_DATABASE_URL with CREATEDB permission")
	}
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, parsed.Scheme,
		"CARPOOL_TEST_DATABASE_URL must be a PostgreSQL URL")
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, admin.PingContext(ctx))
	var suffix [8]byte
	_, err = rand.Read(suffix[:])
	require.NoError(t, err)
	name := fmt.Sprintf("carpool_official_%x", suffix)
	_, err = admin.ExecContext(ctx, `CREATE DATABASE `+pq.QuoteIdentifier(name)+` TEMPLATE template0`)
	require.NoError(t, err, "isolated migration test requires CREATEDB permission on its test server")
	var db *sql.DB
	t.Cleanup(func() {
		if db != nil {
			require.NoError(t, db.Close())
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, err := admin.ExecContext(cleanupCtx, `DROP DATABASE `+pq.QuoteIdentifier(name))
		require.NoError(t, err, "remove only the disposable migration-test database")
	})
	parsed.Path = "/" + name
	parsed.RawPath = ""
	// Do not inherit a custom test schema: official SQL uses public explicitly.
	query := parsed.Query()
	query.Del("search_path")
	parsed.RawQuery = query.Encode()
	db, err = sql.Open("postgres", parsed.String())
	require.NoError(t, err)
	require.NoError(t, db.PingContext(ctx))
	return db
}

func TestOfficial018ToCar019PostgresPreservesData(t *testing.T) {
	db := officialToCarTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// The merged tree retains official 0.2.18 SQL byte-for-byte. Only these
	// four car migrations and two official 0.2.19 migrations were absent there.
	pending := map[string]bool{
		"237_custom_group_carpool.sql":          true,
		"238_custom_carpool_reset_monitor.sql":  true,
		"241_custom_quota_reset_operations.sql": true,
		"242_custom_carpool_reset_voting.sql":   true,
		"247_scoped_admin_api_keys.sql":         true,
		"248_group_image_count_billing.sql":     true,
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
	for name := range pending {
		_, err := migrations.FS.ReadFile(name)
		require.NoError(t, err, "the upgrade must include %s", name)
	}
	require.NoError(t, applyMigrationsFS(ctx, db, baseline), "create official 0.2.18 schema from unmodified SQL")

	// All credentials are inert fixtures. This test starts no application or
	// background workers and cannot refresh an upstream OAuth token.
	_, err = db.ExecContext(ctx, `
		INSERT INTO users (id,email,password_hash,username,balance,role,totp_enabled,totp_secret_encrypted,totp_enabled_at)
		VALUES (9001,'owner@example.invalid','$2a$10$fixture-password-hash','existing-owner',123.45678901,'admin',true,'fixture-encrypted-totp','2026-10-01T00:00:00Z');
		INSERT INTO groups (id,name,platform,subscription_type,weekly_limit_usd,rate_multiplier)
		VALUES (9002,'Existing subscription','openai','subscription',325.25,1.125);
		INSERT INTO proxies (id,name,protocol,host,port,username,password)
		VALUES (9003,'Existing proxy','http','proxy.example.invalid',8080,'fixture-proxy-user','fixture-proxy-password');
		INSERT INTO accounts (id,name,platform,type,proxy_id,credentials,extra)
		VALUES (9004,'Existing OAuth','openai','oauth',9003,
		 '{"access_token":"fixture-access-token","refresh_token":"fixture-refresh-token","id_token":"fixture-id-token","chatgpt_account_id":"fixture-chatgpt-account","expires_at":4102444800}',
		 '{"fixture_account_setting":"keep"}');
		INSERT INTO account_groups (account_id,group_id,priority) VALUES (9004,9002,7);
		INSERT INTO user_allowed_groups (user_id,group_id) VALUES (9001,9002);
		INSERT INTO api_keys (id,user_id,key,name,group_id)
		VALUES (9005,9001,'sk-fixture-existing-key','Existing key',9002);
		INSERT INTO user_subscriptions (id,user_id,group_id,starts_at,expires_at,daily_window_start,weekly_window_start,monthly_window_start,daily_usage_usd,weekly_usage_usd,monthly_usage_usd,assigned_by)
		VALUES (9006,9001,9002,'2026-10-01T00:00:00Z','2099-01-01T00:00:00Z',
		 '2026-10-30T00:00:00Z','2026-10-26T00:00:00Z','2026-10-01T00:00:00Z',12.3456789012,123.4567890123,234.5678901234,9001);
		INSERT INTO usage_logs (id,user_id,api_key_id,account_id,group_id,subscription_id,request_id,model,input_tokens,output_tokens,total_cost,actual_cost,billing_type,created_at)
		VALUES (9007,9001,9005,9004,9002,9006,'fixture-request','gpt-5.5',1234,567,0.1234567890,0.1111111111,1,'2026-10-30T12:00:00Z');
		INSERT INTO auth_identities (user_id,provider_type,provider_key,provider_subject,verified_at)
		VALUES (9001,'email','email','owner@example.invalid','2026-10-01T00:00:00Z');
		INSERT INTO settings (key,value) VALUES ('site_name','Existing official site'),('registration_enabled','false')
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
		INSERT INTO security_secrets (key,value) VALUES ('jwt_secret','fixture-persisted-jwt-secret-at-least-32-bytes'),('fixture_totp_secret','fixture-preserved-secret');
	`)
	require.NoError(t, err)

	// Auth and refresh-token code is unchanged from official 0.2.18. Issue a
	// session against that schema, keeping its signing key in PostgreSQL and
	// its refresh-token family in the same non-default Redis DB across restart.
	redisServer := miniredis.RunT(t)
	redisBefore := redis.NewClient(&redis.Options{Addr: redisServer.Addr(), DB: 3})
	t.Cleanup(func() { _ = redisBefore.Close() })
	clientBefore := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	cfgBefore := &config.Config{JWT: config.JWTConfig{ExpireHour: 1, RefreshTokenExpireDays: 7}}
	require.NoError(t, ensureBootstrapSecrets(ctx, clientBefore, cfgBefore))
	require.Equal(t, "fixture-persisted-jwt-secret-at-least-32-bytes", cfgBefore.JWT.Secret)
	userRepoBefore := NewUserRepository(clientBefore, db)
	userBefore, err := userRepoBefore.GetByID(ctx, 9001)
	require.NoError(t, err)
	authBefore := service.NewAuthService(clientBefore, userRepoBefore, nil, NewRefreshTokenCache(redisBefore), cfgBefore, nil, nil, nil, nil, nil, nil, nil, nil)
	tokensBefore, err := authBefore.GenerateTokenPair(ctx, userBefore, "")
	require.NoError(t, err)
	require.NoError(t, redisBefore.Close(), "application disconnect must not flush Redis session state")

	// Compare whole rows, including precision, JSON credentials, timestamps,
	// foreign keys, and settings already seeded by official migrations. Only
	// the new group columns are excluded from the before/after comparison.
	snapshot := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, table := range []string{"users", "groups", "proxies", "accounts", "account_groups", "user_allowed_groups", "api_keys", "user_subscriptions", "usage_logs", "auth_identities", "settings", "security_secrets"} {
			expression := "to_jsonb(t)"
			if table == "groups" {
				expression += " - 'carpool_members' - 'carpool_quota_synced_at' - 'image_count_billing'"
			}
			query := `SELECT COALESCE(jsonb_agg(row_data ORDER BY row_data::text), '[]'::jsonb)::text FROM (SELECT ` + expression + ` AS row_data FROM ` + pq.QuoteIdentifier(table) + ` t) rows`
			var value string
			require.NoError(t, db.QueryRowContext(ctx, query).Scan(&value), table)
			out[table] = value
		}
		return out
	}
	ledger := func() map[string]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT filename, checksum || '/' || applied_at::text FROM schema_migrations ORDER BY filename`)
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
	before := snapshot()
	historical := ledger()
	require.Len(t, historical, len(baseline))
	for name, entry := range baseline {
		require.True(t, strings.HasPrefix(historical[name], fmt.Sprintf("%x/", sha256.Sum256([]byte(strings.TrimSpace(string(entry.Data)))))), name)
	}

	for pass := 1; pass <= 2; pass++ {
		require.NoError(t, ApplyMigrations(ctx, db), "upgrade pass %d", pass)
		require.Equal(t, before, snapshot(), "upgrade/restart must preserve existing official rows")
		current := ledger()
		require.Len(t, current, len(files))
		for name, value := range historical {
			require.Equal(t, value, current[name], "historical checksum and applied_at must remain unchanged: %s", name)
		}
		for name := range pending {
			content, err := migrations.FS.ReadFile(name)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(current[name], fmt.Sprintf("%x/", sha256.Sum256([]byte(strings.TrimSpace(string(content)))))), name)
		}
		// The second startup must preserve the newly added ledger entries too.
		historical = current
	}

	// Reconstruct the application-side clients/services; retain only database
	// and Redis state, as an application-container replacement would do.
	clientAfter := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	cfgAfter := &config.Config{JWT: config.JWTConfig{ExpireHour: 1, RefreshTokenExpireDays: 7}}
	require.NoError(t, ensureBootstrapSecrets(ctx, clientAfter, cfgAfter))
	require.Equal(t, cfgBefore.JWT.Secret, cfgAfter.JWT.Secret, "restart must reuse the persisted signing key")
	redisAfter := redis.NewClient(&redis.Options{Addr: redisServer.Addr(), DB: 3})
	t.Cleanup(func() { _ = redisAfter.Close() })
	cacheAfter := NewRefreshTokenCache(redisAfter)
	userRepoAfter := NewUserRepository(clientAfter, db)
	authAfter := service.NewAuthService(clientAfter, userRepoAfter, nil, cacheAfter, cfgAfter, nil, nil, nil, nil, nil, nil, nil, nil)
	claims, err := authAfter.ValidateToken(tokensBefore.AccessToken)
	require.NoError(t, err, "existing browser access token must survive the migration")
	// UserService normalizes the email/password-derived token version used by
	// JWT middleware; the repository's raw User.TokenVersion is not normalized.
	userAfter, err := service.NewUserService(userRepoAfter, nil, nil, nil).GetByID(ctx, claims.UserID)
	require.NoError(t, err)
	require.Equal(t, int64(9001), userAfter.ID)
	require.True(t, userAfter.IsActive())
	require.True(t, userAfter.TokenVersionResolved)
	require.Equal(t, userAfter.TokenVersion, claims.TokenVersion)
	require.Equal(t, userAfter.Email, claims.Email)
	require.Equal(t, userAfter.Role, claims.Role)

	rotated, err := authAfter.RefreshTokenPair(ctx, tokensBefore.RefreshToken)
	require.NoError(t, err, "existing browser refresh token must survive the migration")
	require.NotEqual(t, tokensBefore.RefreshToken, rotated.RefreshToken, "normal refresh-token rotation remains enabled")
	require.Equal(t, userAfter.Role, rotated.UserRole)
	rotatedClaims, err := authAfter.ValidateToken(rotated.AccessToken)
	require.NoError(t, err)
	require.Equal(t, userAfter.ID, rotatedClaims.UserID)
	require.Equal(t, userAfter.TokenVersion, rotatedClaims.TokenVersion)
	require.NotEmpty(t, claims.SessionID)
	require.Equal(t, claims.SessionID, rotatedClaims.SessionID, "refresh retains the existing session family")
	_, err = cacheAfter.GetRefreshToken(ctx, fmt.Sprintf("%x", sha256.Sum256([]byte(tokensBefore.RefreshToken))))
	require.ErrorIs(t, err, service.ErrRefreshTokenNotFound, "consumed refresh token must no longer be reusable")
	require.Equal(t, before, snapshot(), "session validation and refresh must not rewrite preserved database rows")

	var members int
	var synced sql.NullTime
	var imageCountBilling bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT carpool_members,carpool_quota_synced_at,image_count_billing FROM groups WHERE id=9002`).Scan(&members, &synced, &imageCountBilling))
	require.Zero(t, members, "existing official groups do not opt into carpool automation")
	require.False(t, synced.Valid)
	require.False(t, imageCountBilling, "existing official billing mode is retained")
	for _, table := range []string{"carpool_reset_monitor", "carpool_account_reset_receipts", "carpool_subscription_reset_events", "carpool_subscription_reset_targets", "carpool_reset_source_changes", "quota_reset_operations", "carpool_reset_votes", "carpool_reset_vote_approvals", "carpool_reset_vote_requests", "carpool_reset_vote_targets", "carpool_vote_membership_changes", "scoped_admin_api_keys"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM `+pq.QuoteIdentifier(table)).Scan(&count), table)
		require.Zero(t, count, "migration must not invent an operation or admin key: %s", table)
	}
	for _, trigger := range []string{"carpool_reset_account_changed", "carpool_reset_group_changed", "carpool_reset_binding_changed", "carpool_vote_subscription_changed", "carpool_vote_user_changed"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname=$1 AND NOT tgisinternal`, trigger).Scan(&count))
		require.Equal(t, 1, count, trigger)
	}
}
