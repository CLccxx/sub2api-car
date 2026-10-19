package repository

import (
	"context"
	"errors"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCarpoolInitializationChecksCurrentDatabaseCap(t *testing.T) {
	var captured string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &captured}))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer client.Close()
	mock.ExpectBegin()
	mock.ExpectExec("update").WillReturnError(errors.New("stop after captured update"))
	mock.ExpectRollback()
	limit := 400.0
	g := &service.Group{ID: 10, Name: "4 人车分组", Platform: service.PlatformOpenAI,
		SubscriptionType: service.SubscriptionTypeSubscription, CarpoolMembers: 4, WeeklyLimitUSD: &limit}
	err = newGroupRepositoryWithSQL(client, nil).Update(context.Background(), g)
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	parts := strings.Split(captured, "WHERE")
	require.Len(t, parts, 2)
	require.Contains(t, parts[1], `"weekly_limit_usd" IS NULL`)
	require.Contains(t, parts[1], `"weekly_limit_usd" =`)
}

func TestCarpoolInitializationConflictRequiresRefresh(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer client.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "groups"`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(int64(10), float64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectRollback()
	limit := 400.0
	err = newGroupRepositoryWithSQL(client, nil).Update(context.Background(), &service.Group{
		ID: 10, Name: "4 人车分组", Platform: service.PlatformOpenAI, SubscriptionType: service.SubscriptionTypeSubscription,
		CarpoolMembers: 4, WeeklyLimitUSD: &limit,
	})
	require.Error(t, err)
	require.Equal(t, 409, infraerrors.Code(err), "%v", err)
	require.Equal(t, "CARPOOL_QUOTA_CHANGED", infraerrors.Reason(err))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCarpoolExistingCapIsOmittedFromAdminUpdate(t *testing.T) {
	var captured string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &captured}))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer client.Close()
	mock.ExpectBegin()
	mock.ExpectExec("update").WillReturnError(errors.New("stop after captured update"))
	mock.ExpectRollback()
	limit := 325.0
	err = newGroupRepositoryWithSQL(client, nil).Update(context.Background(), &service.Group{
		ID: 10, Name: "4 人车分组", Platform: service.PlatformOpenAI, SubscriptionType: service.SubscriptionTypeSubscription,
		CarpoolMembers: 4, WeeklyLimitUSD: &limit, PreserveCarpoolWeeklyLimit: true,
	})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.NotContains(t, captured, "weekly_limit_usd")
}
