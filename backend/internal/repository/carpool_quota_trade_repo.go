package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type carpoolQuotaTradeRepository struct{ db *sql.DB }

func NewCarpoolQuotaTradeRepository(db *sql.DB) service.CarpoolQuotaTradeRepository {
	return &carpoolQuotaTradeRepository{db: db}
}

// Same table-before-monitor lock order as reset voting and ApplyEvent. All
// mutations, quota checks and ledger entries commit together; no network calls.
func (r *carpoolQuotaTradeRepository) begin(ctx context.Context) (*sql.Tx, error) {
	return (&carpoolResetVoteRepository{db: r.db}).begin(ctx)
}
func quotaTradeAdmin(ctx context.Context, q carpoolResetQuerier, user int64) error {
	var allowed bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND role='admin' AND status='active' AND deleted_at IS NULL)`, user).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return infraerrors.Forbidden("QUOTA_TRADE_ADMIN_REQUIRED", "Administrator access required")
	}
	return nil
}

type quotaTradeState struct {
	SubscriptionID, UserID, GroupID, AccountID, Generation, SourceRevision, BindingRevision int64
	GroupName, UserName, IdentityHash, CycleID                                              string
	Eligible                                                                                bool
	Base, Used                                                                              float64
}

func quotaTradeSubscription(ctx context.Context, q carpoolResetQuerier, sub int64) (*quotaTradeState, error) {
	st := &quotaTradeState{}
	err := q.QueryRowContext(ctx, `SELECT subscription_id,user_id,group_id,group_name,user_name,account_id,identity_hash,cycle_id,generation,source_revision,binding_revision,
 eligible,COALESCE(base_limit_usd,0),COALESCE(used_usd,0) FROM carpool_quota_trade_subscription_state WHERE subscription_id=$1 AND eligible ORDER BY account_id LIMIT 1`, sub).
		Scan(&st.SubscriptionID, &st.UserID, &st.GroupID, &st.GroupName, &st.UserName, &st.AccountID, &st.IdentityHash, &st.CycleID, &st.Generation, &st.SourceRevision, &st.BindingRevision, &st.Eligible, &st.Base, &st.Used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrQuotaTradeUnavailable
	}
	return st, err
}
func quotaTradeOwner(ctx context.Context, q carpoolResetQuerier, user, sub int64) (int64, error) {
	var group int64
	err := q.QueryRowContext(ctx, `SELECT s.group_id FROM user_subscriptions s JOIN users u ON u.id=s.user_id WHERE s.id=$1 AND s.user_id=$2 AND s.deleted_at IS NULL AND u.deleted_at IS NULL AND u.status='active'`, sub, user).Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, service.ErrSubscriptionNotFound
	}
	return group, err
}
func quotaTradeResetReady(ctx context.Context, q carpoolResetQuerier, st *quotaTradeState) error {
	// Fence the coordinator's session advisory lock. try-only avoids a lock-order
	// cycle with a reset worker that owns its account lock before touching source.
	var locked bool
	if err := q.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1::text,824602))`, fmt.Sprint(st.AccountID)).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return service.ErrQuotaTradeUnavailable
	}
	var blocked bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM carpool_subscription_reset_events WHERE account_id=$1 AND identity_hash=$2 AND status='pending')
 OR EXISTS(SELECT 1 FROM quota_reset_operations WHERE account_id=$1 AND status='pending')
 OR EXISTS(SELECT 1 FROM carpool_reset_votes WHERE account_id=$1 AND status IN('executing','awaiting_confirmation','applying'))
 OR EXISTS(SELECT 1 FROM carpool_account_reset_receipts r WHERE r.account_id=$1 AND r.identity_hash=$2 AND r.occurred_at>
 COALESCE((SELECT observed_at FROM carpool_subscription_reset_events WHERE account_id=$1 AND identity_hash=$2 AND status='completed' ORDER BY id DESC LIMIT 1),'-infinity'::timestamptz))`, st.AccountID, st.IdentityHash).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return service.ErrQuotaTradeUnavailable
	}
	return nil
}

const quotaTradeColumns = `t.id,t.group_id,t.group_name,t.account_id,t.cycle_id,t.seller_user_id,t.seller_name,t.seller_subscription_id,t.buyer_user_id,t.buyer_name,t.amount_cents,t.sale_price_cents,t.status,t.created_at,t.reserved_at,t.completed_at,t.terms_version,t.terms_accepted_at`

type quotaTradeScanner interface{ Scan(...any) error }

func scanQuotaTrade(row quotaTradeScanner) (*service.CarpoolQuotaTrade, error) {
	t := &service.CarpoolQuotaTrade{}
	var cents int64
	var salePriceCents sql.NullInt64
	err := row.Scan(&t.ID, &t.GroupID, &t.GroupName, &t.AccountID, &t.CycleID, &t.SellerUserID, &t.SellerName, &t.SellerSubscriptionID, &t.BuyerUserID, &t.BuyerName, &cents, &salePriceCents, &t.Status, &t.CreatedAt, &t.ReservedAt, &t.CompletedAt, &t.TermsVersion, &t.TermsAcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrQuotaTradeNotFound
	}
	t.AmountUSD = float64(cents) / 100
	if salePriceCents.Valid {
		price := float64(salePriceCents.Int64) / 100
		t.SalePriceCNY = &price
	}
	return t, err
}
func quotaTradeGet(ctx context.Context, q carpoolResetQuerier, id int64) (*service.CarpoolQuotaTrade, error) {
	return scanQuotaTrade(q.QueryRowContext(ctx, `SELECT `+quotaTradeColumns+` FROM carpool_quota_trades t WHERE id=$1`, id))
}
func quotaTradeEvent(ctx context.Context, tx *sql.Tx, id int64, action string, actor service.CarpoolQuotaTradeActor, buyer *int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO carpool_quota_trade_events(trade_id,action,actor_user_id,actor_name,is_admin,buyer_user_id)
 SELECT $1,$2,id,username,$4,$5 FROM users WHERE id=$3`, id, action, actor.UserID, actor.IsAdmin, buyer)
	return err
}
func (r *carpoolQuotaTradeRepository) Create(parent context.Context, user, sub, cents, salePriceCents int64, key string) (*service.CarpoolQuotaTrade, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if cents < 1 || cents > 1000000000000 {
		return nil, infraerrors.BadRequest("QUOTA_TRADE_AMOUNT", "Invalid amount")
	}
	if salePriceCents < 1 || salePriceCents > service.CarpoolQuotaTradeMaxSalePriceCents {
		return nil, infraerrors.BadRequest("QUOTA_TRADE_SALE_PRICE", "Invalid CNY sale price")
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = quotaTradeOwner(ctx, tx, user, sub); err != nil {
		return nil, err
	}
	// Resolve the durable idempotency key BEFORE eligibility/balance checks.
	var oldID, oldSub, oldCents int64
	var oldSalePriceCents sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT id,seller_subscription_id,amount_cents,sale_price_cents FROM carpool_quota_trades WHERE seller_user_id=$1 AND request_id=$2::uuid`, user, key).Scan(&oldID, &oldSub, &oldCents, &oldSalePriceCents)
	if err == nil {
		if oldSub != sub || oldCents != cents || !oldSalePriceCents.Valid || oldSalePriceCents.Int64 != salePriceCents {
			return nil, infraerrors.Conflict("QUOTA_TRADE_REQUEST_REUSED", "request_id was already used for a different order")
		}
		t, e := quotaTradeGet(ctx, tx, oldID)
		if e != nil {
			return nil, e
		}
		return t, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	st, err := quotaTradeSubscription(ctx, tx, sub)
	if err != nil {
		return nil, err
	}
	if st.UserID != user {
		return nil, service.ErrSubscriptionNotFound
	}
	if err = quotaTradeResetReady(ctx, tx, st); err != nil {
		return nil, err
	}
	var available bool
	// Compare NUMERIC at full precision. No extra $10 reserve and no rounding up.
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(base_limit_usd,0)+(adjustment_cents-frozen_cents)/100.0-COALESCE(used_usd,0)>$2::numeric/100
 FROM carpool_quota_trade_entitlements WHERE subscription_id=$1`, sub, cents).Scan(&available)
	if err != nil {
		return nil, err
	}
	if !available {
		return nil, service.ErrQuotaTradeInsufficient
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO carpool_quota_trades(group_id,group_name,account_id,identity_hash,source_revision,binding_revision,cycle_id,seller_user_id,seller_name,seller_subscription_id,seller_generation,amount_cents,sale_price_cents,request_id,terms_version)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::uuid,$15) RETURNING id`, st.GroupID, st.GroupName, st.AccountID, st.IdentityHash, st.SourceRevision, st.BindingRevision, st.CycleID, user, st.UserName, sub, st.Generation, cents, salePriceCents, key, service.CarpoolQuotaTradeTermsVersion).Scan(&id)
	if err != nil {
		return nil, err
	}
	if err = quotaTradeEvent(ctx, tx, id, "create", service.CarpoolQuotaTradeActor{UserID: user, SubscriptionID: sub}, nil); err != nil {
		return nil, err
	}
	t, err := quotaTradeGet(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return t, tx.Commit()
}

type quotaTradeSnapshot struct {
	Identity, Cycle                     string
	Account, Source, Binding, SellerGen int64
	BuyerSub, BuyerGen                  sql.NullInt64
}

func quotaTradeMatches(st *quotaTradeState, snap quotaTradeSnapshot, group, user, generation int64) bool {
	return st != nil && st.Eligible && st.GroupID == group && st.UserID == user && st.AccountID == snap.Account && st.IdentityHash == snap.Identity && st.CycleID == snap.Cycle && st.SourceRevision == snap.Source && st.BindingRevision == snap.Binding && st.Generation == generation
}
func (r *carpoolQuotaTradeRepository) Act(parent context.Context, actor service.CarpoolQuotaTradeActor, id int64, action string) (*service.CarpoolQuotaTrade, error) {
	if action == "cancel-reservation" && !actor.IsAdmin {
		return nil, infraerrors.Forbidden("QUOTA_TRADE_ADMIN_REQUIRED", "Only administrators can cancel reservations")
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if actor.IsAdmin {
		if err = quotaTradeAdmin(ctx, tx, actor.UserID); err != nil {
			return nil, err
		}
	} else if _, err = quotaTradeOwner(ctx, tx, actor.UserID, actor.SubscriptionID); err != nil {
		return nil, err
	}
	snap := quotaTradeSnapshot{}
	err = tx.QueryRowContext(ctx, `SELECT identity_hash,cycle_id,account_id,source_revision,binding_revision,seller_generation,buyer_subscription_id,buyer_generation FROM carpool_quota_trades WHERE id=$1 FOR UPDATE`, id).
		Scan(&snap.Identity, &snap.Cycle, &snap.Account, &snap.Source, &snap.Binding, &snap.SellerGen, &snap.BuyerSub, &snap.BuyerGen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrQuotaTradeNotFound
	}
	if err != nil {
		return nil, err
	}
	t, err := quotaTradeGet(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	seller := actor.UserID == t.SellerUserID && actor.SubscriptionID == t.SellerSubscriptionID
	if !actor.IsAdmin {
		if action == "reserve" {
			st, e := quotaTradeSubscription(ctx, tx, actor.SubscriptionID)
			if e != nil {
				return nil, e
			}
			if st.GroupID != t.GroupID || actor.UserID == t.SellerUserID {
				return nil, service.ErrQuotaTradeNotFound
			}
		} else if !seller {
			return nil, service.ErrQuotaTradeNotFound
		}
	}
	// Repeated state transitions are safe and never duplicate quota or events.
	done := action == "confirm" && t.Status == "completed" || action == "withdraw" && t.Status == "withdrawn" || action == "cancel-reservation" && t.Status == "open" || action == "reserve" && t.Status == "reserved" && t.BuyerUserID != nil && *t.BuyerUserID == actor.UserID && snap.BuyerSub.Int64 == actor.SubscriptionID
	if done {
		return t, tx.Commit()
	}
	oldBuyer := t.BuyerUserID
	switch action {
	case "withdraw":
		if t.Status != "open" && (!actor.IsAdmin || t.Status != "expired") {
			return nil, service.ErrQuotaTradeConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE carpool_quota_trades SET status='withdrawn',updated_at=clock_timestamp() WHERE id=$1`, id)
	case "cancel-reservation":
		if !actor.IsAdmin {
			return nil, infraerrors.Forbidden("QUOTA_TRADE_ADMIN_REQUIRED", "Only administrators can cancel reservations")
		}
		if t.Status != "reserved" && (t.Status != "expired" || t.BuyerUserID == nil) {
			return nil, service.ErrQuotaTradeConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE carpool_quota_trades SET status=CASE WHEN status='expired' THEN 'expired' ELSE 'open' END,buyer_user_id=NULL,buyer_name=NULL,buyer_subscription_id=NULL,buyer_generation=NULL,reserved_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, id)
	case "reserve", "confirm":
		if action == "reserve" && (actor.IsAdmin || t.Status != "open") || action == "confirm" && t.Status != "reserved" {
			return nil, service.ErrQuotaTradeConflict
		}
		st, e := quotaTradeSubscription(ctx, tx, t.SellerSubscriptionID)
		if e != nil {
			return nil, e
		}
		if !quotaTradeMatches(st, snap, t.GroupID, t.SellerUserID, snap.SellerGen) {
			return nil, service.ErrQuotaTradeConflict
		}
		if err = quotaTradeResetReady(ctx, tx, st); err != nil {
			return nil, err
		}
		buyerSub := actor.SubscriptionID
		buyerUser := actor.UserID
		buyerGen := int64(0)
		if action == "confirm" {
			buyerSub = snap.BuyerSub.Int64
			buyerUser = *t.BuyerUserID
			buyerGen = snap.BuyerGen.Int64
		}
		buyer, e := quotaTradeSubscription(ctx, tx, buyerSub)
		if e != nil {
			return nil, e
		}
		if action == "reserve" {
			buyerGen = buyer.Generation
		}
		if !quotaTradeMatches(buyer, snap, t.GroupID, buyerUser, buyerGen) || buyerUser == t.SellerUserID {
			return nil, service.ErrQuotaTradeConflict
		}
		// Never recheck seller balance here: the exact amount was already frozen.
		if action == "reserve" {
			_, err = tx.ExecContext(ctx, `UPDATE carpool_quota_trades SET status='reserved',buyer_user_id=$2,buyer_name=$3,buyer_subscription_id=$4,buyer_generation=$5,reserved_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, id, buyerUser, buyer.UserName, buyerSub, buyer.Generation)
			oldBuyer = &buyerUser
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE carpool_quota_trades SET status='completed',completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, id)
		}
	default:
		return nil, infraerrors.BadRequest("QUOTA_TRADE_ACTION", "Invalid trade action")
	}
	if err != nil {
		return nil, err
	}
	if err = quotaTradeEvent(ctx, tx, id, action, actor, oldBuyer); err != nil {
		return nil, err
	}
	t, err = quotaTradeGet(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return t, tx.Commit()
}

func quotaTradeReadPage(ctx context.Context, q carpoolResetQuerier, where string, args []any, page, size int) (*service.CarpoolQuotaTradePage, error) {
	result := &service.CarpoolQuotaTradePage{Items: []service.CarpoolQuotaTrade{}, Page: page, PageSize: size}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM carpool_quota_trades t WHERE `+where, args...).Scan(&result.Total); err != nil {
		return nil, err
	}
	args = append(args, size, (page-1)*size)
	rows, err := q.QueryContext(ctx, `SELECT `+quotaTradeColumns+` FROM carpool_quota_trades t WHERE `+where+fmt.Sprintf(` ORDER BY t.id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		t, err := scanQuotaTrade(rows)
		if err != nil {
			return nil, err
		}
		result.Items = append(result.Items, *t)
	}
	return result, rows.Err()
}

// Match the full immutable intent against BOTH parties' live state. Keep this
// predicate shared by current/history so invalid pending intents cannot leak
// into current (or disappear from their participants' history). Read-only: no
// status, ledger, frozen quota or audit events are changed by listing.
const quotaTradePendingValid = `t.status IN('open','reserved')
 AND EXISTS(SELECT 1 FROM carpool_quota_trade_subscription_state seller
   WHERE seller.subscription_id=t.seller_subscription_id AND seller.user_id=t.seller_user_id
     AND seller.group_id=t.group_id AND seller.account_id=t.account_id AND seller.identity_hash=t.identity_hash
     AND seller.cycle_id=t.cycle_id AND seller.source_revision=t.source_revision AND seller.binding_revision=t.binding_revision
     AND seller.generation=t.seller_generation AND seller.eligible)
 AND (t.status='open' OR EXISTS(SELECT 1 FROM carpool_quota_trade_subscription_state buyer
   WHERE buyer.subscription_id=t.buyer_subscription_id AND buyer.user_id=t.buyer_user_id
     AND buyer.group_id=t.group_id AND buyer.account_id=t.account_id AND buyer.identity_hash=t.identity_hash
     AND buyer.cycle_id=t.cycle_id AND buyer.source_revision=t.source_revision AND buyer.binding_revision=t.binding_revision
     AND buyer.generation=t.buyer_generation AND buyer.eligible))`

func (r *carpoolQuotaTradeRepository) Market(parent context.Context, user, sub int64, page, size int, view ...string) (*service.CarpoolQuotaTradeMarket, error) {
	selected, err := service.CarpoolQuotaTradeMarketView(view...)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	group, err := quotaTradeOwner(ctx, tx, user, sub)
	if err != nil {
		return nil, err
	}
	v := &service.CarpoolQuotaTradeMarket{TermsVersion: service.CarpoolQuotaTradeTermsVersion, TermsNotice: service.CarpoolQuotaTradeTermsNotice}
	st, err := quotaTradeSubscription(ctx, tx, sub)
	if err != nil && !errors.Is(err, service.ErrQuotaTradeUnavailable) {
		return nil, err
	}
	participant := `(t.seller_user_id=$2 OR t.buyer_user_id=$2)`
	where := `t.group_id=$1 AND ` + participant + ` AND
 (t.status IN('completed','withdrawn','expired') OR (t.status IN('open','reserved') AND NOT (` + quotaTradePendingValid + `)))`
	args := []any{group, user}
	if selected == "current" {
		where = `FALSE`
		args = nil
	}
	if st != nil {
		v.Enabled = true
		if e := quotaTradeResetReady(ctx, tx, st); e != nil {
			if !errors.Is(e, service.ErrQuotaTradeUnavailable) {
				return nil, e
			}
			v.Enabled = false
		}
		var adj, frozen float64
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(base_limit_usd,0),COALESCE(used_usd,0),adjustment_cents,frozen_cents FROM carpool_quota_trade_entitlements WHERE subscription_id=$1`, sub).Scan(&v.Quota.BaseLimitUSD, &v.Quota.UsedUSD, &adj, &frozen)
		if err != nil {
			return nil, err
		}
		v.Quota.AdjustmentUSD = adj / 100
		v.Quota.FrozenUSD = frozen / 100
		v.Quota.AvailableUSD = max(0, v.Quota.BaseLimitUSD+adj/100-frozen/100-v.Quota.UsedUSD)
		if selected == "current" {
			where = `t.group_id=$1 AND t.account_id=$3 AND t.identity_hash=$4 AND t.cycle_id=$5
 AND t.source_revision=$6 AND t.binding_revision=$7
 AND (t.status='open' OR (t.status='reserved' AND ` + participant + `))
 AND (` + quotaTradePendingValid + `)`
			args = []any{group, user, st.AccountID, st.IdentityHash, st.CycleID, st.SourceRevision, st.BindingRevision}
		}
	}
	p, err := quotaTradeReadPage(ctx, tx, where, args, page, size)
	if err != nil {
		return nil, err
	}
	for i := range p.Items {
		canAction := selected == "current" && v.Enabled
		p.Items[i].CanAction = &canAction
	}
	v.CarpoolQuotaTradePage = *p
	return v, tx.Commit()
}
func (r *carpoolQuotaTradeRepository) List(parent context.Context, admin int64, f service.CarpoolQuotaTradeFilter) (*service.CarpoolQuotaTradePage, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if err := quotaTradeAdmin(ctx, r.db, admin); err != nil {
		return nil, err
	}
	parts := []string{"TRUE"}
	args := []any{}
	add := func(expr string, v any) { args = append(args, v); parts = append(parts, fmt.Sprintf(expr, len(args))) }
	if f.Status != "" {
		add("t.status=$%d", f.Status)
	}
	if f.GroupID > 0 {
		add("t.group_id=$%d", f.GroupID)
	}
	if f.AccountID > 0 {
		add("t.account_id=$%d", f.AccountID)
	}
	if f.UserID > 0 {
		args = append(args, f.UserID)
		parts = append(parts, fmt.Sprintf("(t.seller_user_id=$%d OR t.buyer_user_id=$%d)", len(args), len(args)))
	}
	return quotaTradeReadPage(ctx, r.db, strings.Join(parts, " AND "), args, f.Page, f.PageSize)
}
func (r *carpoolQuotaTradeRepository) Events(parent context.Context, admin, id int64) ([]service.CarpoolQuotaTradeEvent, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if err := quotaTradeAdmin(ctx, r.db, admin); err != nil {
		return nil, err
	}
	if _, err := quotaTradeGet(ctx, r.db, id); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id,action,actor_user_id,actor_name,is_admin,buyer_user_id,created_at FROM carpool_quota_trade_events WHERE trade_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []service.CarpoolQuotaTradeEvent{}
	for rows.Next() {
		var e service.CarpoolQuotaTradeEvent
		if err = rows.Scan(&e.ID, &e.Action, &e.ActorUserID, &e.ActorName, &e.IsAdmin, &e.BuyerUserID, &e.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}
