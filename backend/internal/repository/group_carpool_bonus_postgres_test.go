package repository

import (
	"context"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCarpoolBonusPostgres(t *testing.T) {
	db := officialToCarTestDB(t)
	ctx := context.Background()
	require.NoError(t, ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := newGroupRepositoryWithSQL(client, db)
	synced := time.Now().UTC().Truncate(30 * time.Minute)
	group := &service.Group{
		Name: "bonus-test", Platform: service.PlatformOpenAI, RateMultiplier: 1, Status: service.StatusActive,
		SubscriptionType: service.SubscriptionTypeSubscription, CarpoolMembers: 4, CarpoolBonusUSD: 100,
		WeeklyLimitUSD: new(445.0), DefaultValidityDays: 30,
	}
	require.NoError(t, repo.Create(ctx, group))
	_, err := db.ExecContext(ctx, `UPDATE groups SET carpool_quota_synced_at=$1 WHERE id=$2`, synced, group.ID)
	require.NoError(t, err)
	user, err := client.User.Create().SetEmail("carpool-bonus@example.test").SetPasswordHash("test-only").Save(ctx)
	require.NoError(t, err)
	subscription, err := client.UserSubscription.Create().SetUserID(user.ID).SetGroupID(group.ID).
		SetStartsAt(synced).SetExpiresAt(synced.Add(30 * 24 * time.Hour)).
		SetDailyWindowStart(synced).SetWeeklyWindowStart(synced).SetMonthlyWindowStart(synced).
		SetDailyUsageUsd(50).SetWeeklyUsageUsd(350).SetMonthlyUsageUsd(700).Save(ctx)
	require.NoError(t, err)

	read := func() *service.Group {
		t.Helper()
		got, err := repo.GetByID(ctx, group.ID)
		require.NoError(t, err)
		return got
	}
	captureConfig := func(g *service.Group) {
		g.PreviousCarpoolConfig = &service.CarpoolConfig{
			Members: g.CarpoolMembers, Platform: g.Platform,
			SubscriptionType: g.SubscriptionType, Status: g.Status,
		}
	}
	prepare := func(g *service.Group, bonus float64) {
		captureConfig(g)
		previous := g.CarpoolBonusUSD
		g.PreviousCarpoolBonusUSD = &previous
		g.CarpoolBonusUSD = bonus
		g.PreserveCarpoolWeeklyLimit = true
		g.AdjustCarpoolBonus = bonus != previous
		// Deliberately stale client preview; repository must use the current DB cap.
		g.WeeklyLimitUSD = new(9999.0)
	}
	assertSaved := func(g *service.Group, bonus, limit float64) {
		t.Helper()
		require.Equal(t, bonus, g.CarpoolBonusUSD)
		require.InDelta(t, limit, *g.WeeklyLimitUSD, 1e-8)
		require.NotNil(t, g.CarpoolQuotaSyncedAt)
		require.True(t, synced.Equal(*g.CarpoolQuotaSyncedAt), "manual edits must not move the half-hour watermark")
		stored, err := client.UserSubscription.Get(ctx, subscription.ID)
		require.NoError(t, err)
		require.Equal(t, 50.0, stored.DailyUsageUsd)
		require.Equal(t, 350.0, stored.WeeklyUsageUsd)
		require.Equal(t, 700.0, stored.MonthlyUsageUsd)
		require.True(t, synced.Equal(*stored.DailyWindowStart))
		require.True(t, synced.Equal(*stored.WeeklyWindowStart))
		require.True(t, synced.Equal(*stored.MonthlyWindowStart))
	}

	// A scheduled refresh between read and save must survive the bonus edit.
	editing := read()
	prepare(editing, 150)
	_, err = db.ExecContext(ctx, `UPDATE groups SET weekly_limit_usd=460, updated_at=NOW() WHERE id=$1`, group.ID)
	require.NoError(t, err)
	require.NoError(t, repo.Update(ctx, editing))
	assertSaved(editing, 150, 510)
	assertSaved(read(), 150, 510)

	// Repeating an identical edit must not add the delta again.
	editing = read()
	prepare(editing, 150)
	require.NoError(t, repo.Update(ctx, editing))
	assertSaved(editing, 150, 510)

	// Zero and cents are saved exactly, including when lowering the allowance.
	for _, bonus := range []float64{0, 0.29, 150.25, 100} {
		editing = read()
		prepare(editing, bonus)
		require.NoError(t, repo.Update(ctx, editing))
		assertSaved(editing, bonus, 360+bonus)
	}

	// A stale competing bonus edit is rejected instead of applying its delta twice.
	first, second, unrelated := read(), read(), read()
	prepare(first, 150)
	prepare(second, 200)
	require.NoError(t, repo.Update(ctx, first))
	err = repo.Update(ctx, second)
	require.Equal(t, "CARPOOL_QUOTA_CHANGED", infraerrors.Reason(err))
	assertSaved(read(), 150, 510)
	unrelated.PreserveCarpoolWeeklyLimit = true
	unrelated.Description = "unrelated update"
	require.NoError(t, repo.Update(ctx, unrelated))
	assertSaved(unrelated, 150, 510)

	// A refresh that has already locked the row commits before the atomic delta.
	editing = read()
	prepare(editing, 175)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE groups SET weekly_limit_usd=550, updated_at=NOW() WHERE id=$1`, group.ID)
	require.NoError(t, err)
	updateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- repo.Update(updateCtx, editing) }()
	select {
	case err := <-done:
		t.Fatalf("bonus update passed the locked group: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	require.NoError(t, <-done)
	assertSaved(editing, 175, 575)

	// Disabling after a scheduled refresh preserves its cap, and a stale bonus
	// edit cannot silently turn carpool mode back on.
	staleBonus, disabling := read(), read()
	prepare(staleBonus, 200)
	captureConfig(disabling)
	disabling.CarpoolMembers = 0
	disabling.PreserveCarpoolWeeklyLimit = true
	_, err = db.ExecContext(ctx, `UPDATE groups SET weekly_limit_usd=600, updated_at=NOW() WHERE id=$1`, group.ID)
	require.NoError(t, err)
	require.NoError(t, repo.Update(ctx, disabling))
	assertSaved(disabling, 175, 600)
	err = repo.Update(ctx, staleBonus)
	require.Equal(t, "CARPOOL_QUOTA_CHANGED", infraerrors.Reason(err))
	require.Zero(t, read().CarpoolMembers)
	assertSaved(read(), 175, 600)
	// A mode-only stale update also reports a conflict (without a bonus guard).
	staleMode := read()
	captureConfig(staleMode)
	staleMode.PreserveCarpoolWeeklyLimit = true
	enabling := read()
	captureConfig(enabling)
	enabling.CarpoolMembers = 4
	enabling.PreserveCarpoolWeeklyLimit = true
	require.NoError(t, repo.Update(ctx, enabling))
	err = repo.Update(ctx, staleMode)
	require.Equal(t, "CARPOOL_QUOTA_CHANGED", infraerrors.Reason(err))
	require.Equal(t, 4, read().CarpoolMembers)

	// Nonpositive caps are not allowed to become an accidental unlimited quota.
	editing = read()
	prepare(editing, 0)
	_, err = db.ExecContext(ctx, `UPDATE groups SET weekly_limit_usd=100 WHERE id=$1`, group.ID)
	require.NoError(t, err)
	err = repo.Update(ctx, editing)
	require.Equal(t, "CARPOOL_QUOTA_CHANGED", infraerrors.Reason(err))
	assertSaved(read(), 175, 100)
}
