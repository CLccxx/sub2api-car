package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCarpoolQuotaTradePostgresResetAdvisoryFence(t *testing.T) {
	db, r := quotaTradePostgres(t)
	ctx := context.Background()
	order, err := r.Create(ctx, 1, 11, 1000, 12345, uuid.NewString())
	require.NoError(t, err)
	order, err = r.Act(ctx, tradeActor(2, 12), order.ID, "reserve")
	require.NoError(t, err)
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	var locked bool
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended('9',824602))`).Scan(&locked))
	require.True(t, locked)
	defer func() {
		_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtextextended('9',824602))`)
	}()
	_, err = r.Create(ctx, 1, 11, 100, 12345, uuid.NewString())
	require.ErrorIs(t, err, service.ErrQuotaTradeUnavailable)
	_, err = r.Act(ctx, tradeActor(1, 11), order.ID, "confirm")
	require.ErrorIs(t, err, service.ErrQuotaTradeUnavailable)
	_, err = conn.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtextextended('9',824602))`)
	require.NoError(t, err)
	_, err = r.Act(ctx, tradeActor(1, 11), order.ID, "confirm")
	require.NoError(t, err)
}
func TestCarpoolQuotaTradePostgresExpiredAndInvalidMembership(t *testing.T) {
	db, r := quotaTradePostgres(t)
	ctx := context.Background()
	admin := service.CarpoolQuotaTradeActor{UserID: 4, IsAdmin: true}
	order, err := r.Create(ctx, 1, 11, 1000, 12345, uuid.NewString())
	require.NoError(t, err)
	_, err = r.Act(ctx, tradeActor(2, 12), order.ID, "reserve")
	require.NoError(t, err)
	// Renewal after an expiry is a new term, even if starts_at is retained.
	_, err = db.Exec(`UPDATE user_subscriptions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=11;
 UPDATE user_subscriptions SET expires_at=clock_timestamp()+interval '20 days' WHERE id=11`)
	require.NoError(t, err)
	_, err = r.Act(ctx, admin, order.ID, "confirm")
	require.Error(t, err)
	_, err = r.Act(ctx, admin, order.ID, "cancel-reservation")
	require.NoError(t, err)
	_, err = r.Act(ctx, admin, order.ID, "withdraw")
	require.NoError(t, err)
	order, err = r.Create(ctx, 1, 11, 1000, 12345, uuid.NewString())
	require.NoError(t, err)
	_, err = r.Act(ctx, tradeActor(2, 12), order.ID, "reserve")
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE user_subscriptions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=12`)
	require.NoError(t, err)
	_, err = r.Act(ctx, admin, order.ID, "confirm")
	require.Error(t, err, "admin cannot bypass subscription expiry")
	_, err = r.Act(ctx, admin, order.ID, "cancel-reservation")
	require.NoError(t, err)
	_, err = r.Act(ctx, admin, order.ID, "withdraw")
	require.NoError(t, err)
}

// Completed quota belongs to the account cycle, not either party's eligibility
// generation. Changing a term must invalidate pending intents, not refund sales.
func TestCarpoolQuotaTradePostgresCompletedSurvivesGenerationChanges(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"seller_shortened_active_term", `UPDATE user_subscriptions SET expires_at=expires_at-interval '1 day' WHERE id=11`},
		{"seller_user_disabled_restored", `UPDATE users SET status='inactive' WHERE id=1; UPDATE users SET status='active' WHERE id=1`},
		{"seller_subscription_revoked_restored", `UPDATE user_subscriptions SET status='revoked' WHERE id=11; UPDATE user_subscriptions SET status='active' WHERE id=11`},
		{"seller_term_start_changed", `UPDATE user_subscriptions SET starts_at=starts_at+interval '1 hour' WHERE id=11`},
		{"seller_expired_then_renewed_same_id", `UPDATE user_subscriptions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=11; UPDATE user_subscriptions SET starts_at=clock_timestamp()-interval '1 minute',expires_at=clock_timestamp()+interval '20 days' WHERE id=11`},
		{"buyer_term_start_changed", `UPDATE user_subscriptions SET starts_at=starts_at+interval '1 hour' WHERE id=12`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, r := quotaTradePostgres(t)
			ctx := context.Background()
			order, err := r.Create(ctx, 1, 11, 5000, 12345, uuid.NewString())
			require.NoError(t, err)
			_, err = r.Act(ctx, tradeActor(2, 12), order.ID, "reserve")
			require.NoError(t, err)
			_, err = r.Act(ctx, tradeActor(1, 11), order.ID, "confirm")
			require.NoError(t, err)
			quotaTradeAssertBalance(t, db, 11, -50, 0)
			quotaTradeAssertBalance(t, db, 12, 50, 0)
			_, err = db.Exec(tc.change)
			require.NoError(t, err)
			var changed int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM carpool_quota_trade_subscription_generations WHERE subscription_id IN(11,12) AND generation>0`).Scan(&changed))
			require.Equal(t, 1, changed, "change only one party's generation; both sides must retain settled quota")
			quotaTradeAssertBalance(t, db, 11, -50, 0)
			quotaTradeAssertBalance(t, db, 12, 50, 0)
			// Neither changing the term nor reusing its row ID can permit overselling.
			_, err = r.Create(ctx, 1, 11, 5000, 12345, uuid.NewString())
			require.ErrorIs(t, err, service.ErrQuotaTradeInsufficient)
			_, err = db.Exec(`INSERT INTO carpool_subscription_reset_events(account_id,identity_hash,status) VALUES(9,$1,'completed')`, service.CarpoolResetIdentityHash("upstream"))
			require.NoError(t, err)
			quotaTradeAssertBalance(t, db, 11, 0, 0)
			quotaTradeAssertBalance(t, db, 12, 0, 0)
		})
	}
}

func TestCarpoolQuotaTradePostgresNoTimerExpiryAndCycleIsolation(t *testing.T) {
	db, r := quotaTradePostgres(t)
	ctx := context.Background()
	order, err := r.Create(ctx, 1, 11, 1000, 12345, uuid.NewString())
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE carpool_quota_trades SET created_at=clock_timestamp()-interval '30 days' WHERE id=$1`, order.ID)
	require.NoError(t, err)
	market, err := r.Market(ctx, 2, 12, 1, 20)
	require.NoError(t, err)
	require.Len(t, market.Items, 1)
	require.Equal(t, "open", market.Items[0].Status)
	quotaTradeAssertBalance(t, db, 11, 0, 10)
	// Another upstream identity on the same local account must not end this cycle.
	_, err = db.Exec(`INSERT INTO carpool_subscription_reset_events(account_id,identity_hash,status,observed_at) VALUES(9,'old-identity','completed',$1)`, time.Now())
	require.NoError(t, err)
	quotaTradeAssertBalance(t, db, 11, 0, 10)
	market, err = r.Market(ctx, 2, 12, 1, 20)
	require.NoError(t, err)
	require.Len(t, market.Items, 1)
}
