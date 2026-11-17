package service

import (
	"context"
	"math"
	"math/big"
	"strconv"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

var (
	ErrQuotaTradeNotFound     = infraerrors.NotFound("QUOTA_TRADE_NOT_FOUND", "Quota trade not found")
	ErrQuotaTradeConflict     = infraerrors.Conflict("QUOTA_TRADE_CONFLICT", "Quota trade state changed or is no longer eligible")
	ErrQuotaTradeUnavailable  = infraerrors.Conflict("QUOTA_TRADE_UNAVAILABLE", "Quota trading is unavailable for this subscription or account reset is pending")
	ErrQuotaTradeInsufficient = infraerrors.BadRequest("QUOTA_TRADE_INSUFFICIENT", "Current available quota must be greater than the sale amount")
)

type CarpoolQuotaTradeService struct {
	repo  CarpoolQuotaTradeRepository
	cache APIKeyAuthCacheInvalidator
}

func NewCarpoolQuotaTradeService(repo CarpoolQuotaTradeRepository, keys *APIKeyService) *CarpoolQuotaTradeService {
	return &CarpoolQuotaTradeService{repo: repo, cache: keys}
}

// The total CNY sale price is independent of the USD quota amount. Only integer
// cents cross the repository boundary; historical orders can have no sale price.
const CarpoolQuotaTradeMaxSalePriceCents int64 = 1000000000000 // CNY 10,000,000,000.00

func CarpoolQuotaTradeAmountCents(amount float64) (int64, error) {
	return quotaTradeMoneyCents(amount, 1000000000000, "QUOTA_TRADE_AMOUNT", "Amount must be positive USD with at most two decimal places")
}

func CarpoolQuotaTradeSalePriceCents(price float64) (int64, error) {
	return quotaTradeMoneyCents(price, CarpoolQuotaTradeMaxSalePriceCents, "QUOTA_TRADE_SALE_PRICE", "sale_price_cny is required and must be positive CNY, at most 10000000000, with at most two decimal places")
}

func quotaTradeMoneyCents(amount float64, maxCents int64, code, message string) (int64, error) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 || amount > float64(maxCents)/100 {
		return 0, infraerrors.BadRequest(code, message)
	}
	// Validate the shortest decimal representation, not binary multiplication:
	// e.g. 9999999999.99 * 100 is not an exact float64 integer.
	decimal, ok := new(big.Rat).SetString(strconv.FormatFloat(amount, 'f', -1, 64))
	if !ok {
		return 0, infraerrors.BadRequest(code, message)
	}
	decimal.Mul(decimal, big.NewRat(100, 1))
	if !decimal.IsInt() || !decimal.Num().IsInt64() || decimal.Num().Int64() > maxCents {
		return 0, infraerrors.BadRequest(code, message)
	}
	return decimal.Num().Int64(), nil
}
func quotaTradePagination(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	if page > 1000000 {
		page = 1000000
	}
	return page, size
}

// CarpoolQuotaTradeMarketView validates the optional view shared by HTTP and
// direct service/repository callers. Only an omitted view defaults to current.
func CarpoolQuotaTradeMarketView(view ...string) (string, error) {
	if len(view) == 0 {
		return "current", nil
	}
	if len(view) == 1 && (view[0] == "current" || view[0] == "history") {
		return view[0], nil
	}
	return "", infraerrors.BadRequest("QUOTA_TRADE_VIEW", "view must be current or history")
}
func (s *CarpoolQuotaTradeService) Market(ctx context.Context, user, sub int64, page, size int, view ...string) (*CarpoolQuotaTradeMarket, error) {
	selected, err := CarpoolQuotaTradeMarketView(view...)
	if err != nil {
		return nil, err
	}
	page, size = quotaTradePagination(page, size)
	return s.repo.Market(ctx, user, sub, page, size, selected)
}
func (s *CarpoolQuotaTradeService) Create(ctx context.Context, user, sub int64, amount, salePriceCNY float64, key string, accepted bool) (*CarpoolQuotaTrade, error) {
	if !accepted {
		return nil, infraerrors.BadRequest("QUOTA_TRADE_TERMS_REQUIRED", "You must accept the quota trading terms")
	}
	id, err := uuid.Parse(key)
	if err != nil {
		return nil, infraerrors.BadRequest("QUOTA_TRADE_REQUEST_ID", "request_id must be a UUID")
	}
	cents, err := CarpoolQuotaTradeAmountCents(amount)
	if err != nil {
		return nil, err
	}
	salePriceCents, err := CarpoolQuotaTradeSalePriceCents(salePriceCNY)
	if err != nil {
		return nil, err
	}
	trade, err := s.repo.Create(ctx, user, sub, cents, salePriceCents, id.String())
	s.invalidate(ctx, trade, err)
	return trade, err
}
func (s *CarpoolQuotaTradeService) Act(ctx context.Context, actor CarpoolQuotaTradeActor, id int64, action string) (*CarpoolQuotaTrade, error) {
	switch action {
	case "reserve", "confirm", "withdraw":
	case "cancel-reservation":
		if !actor.IsAdmin {
			return nil, infraerrors.Forbidden("QUOTA_TRADE_ADMIN_REQUIRED", "Only administrators can cancel a reservation")
		}
	default:
		return nil, infraerrors.BadRequest("QUOTA_TRADE_ACTION", "Invalid trade action")
	}
	t, err := s.repo.Act(ctx, actor, id, action)
	s.invalidate(ctx, t, err)
	return t, err
}
func (s *CarpoolQuotaTradeService) invalidate(ctx context.Context, t *CarpoolQuotaTrade, err error) {
	if err == nil && t != nil && s.cache != nil {
		s.cache.InvalidateAuthCacheByGroupID(ctx, t.GroupID)
	}
}
func (s *CarpoolQuotaTradeService) List(ctx context.Context, admin int64, f CarpoolQuotaTradeFilter) (*CarpoolQuotaTradePage, error) {
	f.Page, f.PageSize = quotaTradePagination(f.Page, f.PageSize)
	switch f.Status {
	case "", "open", "reserved", "completed", "withdrawn", "expired":
	default:
		return nil, infraerrors.BadRequest("QUOTA_TRADE_STATUS", "Invalid trade status")
	}
	return s.repo.List(ctx, admin, f)
}
func (s *CarpoolQuotaTradeService) Events(ctx context.Context, admin, id int64) ([]CarpoolQuotaTradeEvent, error) {
	return s.repo.Events(ctx, admin, id)
}
