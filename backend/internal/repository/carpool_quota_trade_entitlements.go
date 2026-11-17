package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type quotaTradeQueryContext interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Shared by Ent reads and raw transactions. Copy Group: callers may share cached
// group pointers. This intentionally bypasses auth-cache base limits.
func loadCarpoolQuotaTradeEntitlements(ctx context.Context, q quotaTradeQueryContext, subs []*service.UserSubscription) error {
	if len(subs) == 0 {
		return nil
	}
	byID := make(map[int64]*service.UserSubscription, len(subs))
	args := make([]any, 0, len(subs))
	marks := make([]string, 0, len(subs))
	for _, sub := range subs {
		if sub == nil {
			continue
		}
		sub.QuotaAdjustmentUSD = 0
		sub.QuotaFrozenUSD = 0
		if !sub.UsesCarpoolQuotaTrades() {
			continue
		}
		byID[sub.ID] = sub
		args = append(args, sub.ID)
		marks = append(marks, fmt.Sprintf("$%d", len(args)))
	}
	if len(args) == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT s.id,g.weekly_limit_usd,COALESCE(e.adjustment_cents,0),COALESCE(e.frozen_cents,0)
 FROM user_subscriptions s JOIN groups g ON g.id=s.group_id
 LEFT JOIN carpool_quota_trade_entitlements e ON e.subscription_id=s.id
 WHERE s.id IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var base sql.NullFloat64
		var adjustment, frozen float64
		if err = rows.Scan(&id, &base, &adjustment, &frozen); err != nil {
			return err
		}
		sub := byID[id]
		if sub == nil {
			continue
		}
		sub.QuotaAdjustmentUSD = adjustment / 100
		sub.QuotaFrozenUSD = frozen / 100
		if sub.Group != nil {
			group := *sub.Group
			group.WeeklyLimitUSD = nil
			if base.Valid {
				v := base.Float64
				group.WeeklyLimitUSD = &v
			}
			sub.Group = &group
		}
	}
	return rows.Err()
}
