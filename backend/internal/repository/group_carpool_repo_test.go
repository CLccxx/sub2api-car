package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCarpoolRepositoryApply(t *testing.T) {
	scenarios := []struct {
		name    string
		account *service.Account
		locked  string // locked account row extra (JSON text)
		reach   bool   // proceeds to the group update
		ok      bool   // committed successfully
		fails   bool   // returns an error
	}{
		{name: "success", account: &service.Account{ID: 9, Credentials: map[string]any{}}, locked: "{}", reach: true, ok: true},
		{name: "sampled correction still matches", account: &service.Account{ID: 9, Credentials: map[string]any{}, Extra: map[string]any{service.CarpoolQuotaCorrectionExtraKey: 25.0}}, locked: `{"carpool_quota_correction_usd": 25}`, reach: true, ok: true},
		{name: "correction changed since sample", account: &service.Account{ID: 9, Credentials: map[string]any{}, Extra: map[string]any{service.CarpoolQuotaCorrectionExtraKey: 25.0}}, locked: `{"carpool_quota_correction_usd": 30}`},
		{name: "group changed", account: &service.Account{ID: 9, Credentials: map[string]any{}}, locked: "{}"},
		{name: "account changed", account: &service.Account{ID: 9, Credentials: map[string]any{}}, locked: "{}"},
		{name: "extra account", account: &service.Account{ID: 9, Credentials: map[string]any{}}, locked: "{}"},
		{name: "outbox failure", account: &service.Account{ID: 9, Credentials: map[string]any{}}, locked: "{}", reach: true, fails: true},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			repo := NewCarpoolQuotaRepository(db)
			slot := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
			target := service.CarpoolQuotaTarget{GroupID: 10, Members: 4, BonusUSD: 100, Revision: slot.Add(-time.Hour)}
			mock.ExpectBegin()
			rows := sqlmock.NewRows([]string{"id"})
			if scenario.name != "group changed" {
				rows.AddRow(10)
			}
			mock.ExpectQuery(`(?s)SELECT id FROM groups.*carpool_members.*updated_at.*carpool_quota_synced_at.*FOR UPDATE`).
				WithArgs(target.GroupID, target.Members, target.Revision, slot, target.BonusUSD).WillReturnRows(rows)
			if scenario.name != "group changed" {
				accounts := sqlmock.NewRows([]string{"id", "eligible", "extra"}).
					AddRow(9, scenario.name != "account changed", scenario.locked)
				if scenario.name == "extra account" {
					accounts.AddRow(11, true, "{}")
				}
				mock.ExpectQuery(`(?s)SELECT a.id,.*credentials.*account_groups.*FOR SHARE OF a, ag`).
					WithArgs(int64(10), "{}").WillReturnRows(accounts)
			}
			if scenario.reach {
				mock.ExpectExec(`UPDATE groups SET weekly_limit_usd`).WithArgs(325.0, slot, int64(10)).WillReturnResult(sqlmock.NewResult(0, 1))
				outbox := mock.ExpectExec(`INSERT INTO auth_cache_invalidation_outbox`).WithArgs(int64(10))
				if scenario.fails {
					outbox.WillReturnError(context.DeadlineExceeded)
					mock.ExpectRollback()
				} else {
					outbox.WillReturnResult(sqlmock.NewResult(0, 1))
					mock.ExpectExec(`INSERT INTO scheduler_outbox`).WillReturnResult(sqlmock.NewResult(1, 1))
					mock.ExpectCommit()
				}
			} else {
				mock.ExpectRollback()
			}
			ok, err := repo.Apply(context.Background(), target, scenario.account, 325, slot)
			if scenario.fails {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, scenario.ok, ok)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
