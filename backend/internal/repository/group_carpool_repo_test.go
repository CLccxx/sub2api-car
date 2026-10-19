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
	for _, scenario := range []string{"success", "group changed", "account changed", "extra account", "outbox failure"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := NewCarpoolQuotaRepository(db)
			slot := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
			target := service.CarpoolQuotaTarget{GroupID: 10, Members: 4, Revision: slot.Add(-time.Hour)}
			mock.ExpectBegin()
			rows := sqlmock.NewRows([]string{"id"})
			if scenario != "group changed" {
				rows.AddRow(10)
			}
			mock.ExpectQuery(`(?s)SELECT id FROM groups.*carpool_members.*updated_at.*carpool_quota_synced_at.*FOR UPDATE`).
				WithArgs(target.GroupID, target.Members, target.Revision, slot).WillReturnRows(rows)
			if scenario != "group changed" {
				accounts := sqlmock.NewRows([]string{"id", "eligible"}).AddRow(9, scenario != "account changed")
				if scenario == "extra account" {
					accounts.AddRow(11, true)
				}
				mock.ExpectQuery(`(?s)SELECT a.id,.*credentials.*account_groups.*FOR SHARE OF a, ag`).
					WithArgs(int64(10), "{}").WillReturnRows(accounts)
			}
			if scenario == "success" || scenario == "outbox failure" {
				mock.ExpectExec(`UPDATE groups SET weekly_limit_usd`).WithArgs(325.0, slot, int64(10)).WillReturnResult(sqlmock.NewResult(0, 1))
				outbox := mock.ExpectExec(`INSERT INTO auth_cache_invalidation_outbox`).WithArgs(int64(10))
				if scenario == "outbox failure" {
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
			ok, err := repo.Apply(context.Background(), target, &service.Account{ID: 9, Credentials: map[string]any{}}, 325, slot)
			if scenario == "outbox failure" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, scenario == "success", ok)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
