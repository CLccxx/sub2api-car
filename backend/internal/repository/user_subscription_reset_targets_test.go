package repository

import (
	"context"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUserSubscriptionListActiveIDsUsesSingleGlobalSnapshot(t *testing.T) {
	var capturedSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &capturedSQL}))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	rows := sqlmock.NewRows([]string{"id"})
	for id := int64(1); id <= 205; id++ {
		rows.AddRow(id)
	}
	mock.ExpectQuery("global active IDs").
		WithArgs(service.SubscriptionStatusActive, sqlmock.AnyArg()).WillReturnRows(rows)
	ids, err := NewUserSubscriptionRepository(client).ListActiveIDs(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 205)
	require.Equal(t, int64(205), ids[204])
	require.NoError(t, mock.ExpectationsWereMet())
	query := normalizeSQLWhitespace(capturedSQL)
	require.Contains(t, query, `"user_subscriptions"."status" = $1`)
	require.Contains(t, query, `"user_subscriptions"."expires_at" > $2`)
	require.Contains(t, query, `"user_subscriptions"."deleted_at" IS NULL`)
	require.Contains(t, query, `ORDER BY "user_subscriptions"."id" ASC`)
	for _, excluded := range []string{"LIMIT", "OFFSET", "COUNT(", "JOIN"} {
		require.NotContains(t, strings.ToUpper(query), excluded)
	}
}
