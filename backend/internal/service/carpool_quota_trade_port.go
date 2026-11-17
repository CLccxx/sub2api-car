package service

import (
	"context"
	"time"
)

const CarpoolQuotaTradeTermsVersion = "carpool-quota-trading-v2"
const CarpoolQuotaTradeTermsNotice = "挂出即立即冻结相应服务额度，基础额度变化不会解除冻结。账号自然重置或卡重置时，本期挂单及交易效果结束。"

type CarpoolQuotaTrade struct {
	ID                   int64    `json:"id"`
	GroupID              int64    `json:"group_id"`
	GroupName            string   `json:"group_name"`
	AccountID            int64    `json:"account_id"`
	CycleID              string   `json:"cycle_id"`
	SellerUserID         int64    `json:"seller_user_id"`
	SellerName           string   `json:"seller_name"`
	SellerSubscriptionID int64    `json:"seller_subscription_id"`
	BuyerUserID          *int64   `json:"buyer_user_id"`
	BuyerName            *string  `json:"buyer_name"`
	AmountUSD            float64  `json:"amount_usd"`
	SalePriceCNY         *float64 `json:"sale_price_cny"` // Total CNY price; nil for historical unpriced orders.
	Status               string   `json:"status"`
	// Set only on user market results; historical pending intents are not actionable.
	CanAction       *bool      `json:"can_action,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ReservedAt      *time.Time `json:"reserved_at"`
	CompletedAt     *time.Time `json:"completed_at"`
	TermsVersion    string     `json:"terms_version"`
	TermsAcceptedAt time.Time  `json:"terms_accepted_at"`
}
type CarpoolQuotaTradeQuota struct {
	BaseLimitUSD  float64 `json:"base_limit_usd"`
	AdjustmentUSD float64 `json:"adjustment_usd"`
	FrozenUSD     float64 `json:"frozen_usd"`
	UsedUSD       float64 `json:"used_usd"`
	AvailableUSD  float64 `json:"available_usd"`
}
type CarpoolQuotaTradePage struct {
	Items    []CarpoolQuotaTrade `json:"items"`
	Total    int64               `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}
type CarpoolQuotaTradeMarket struct {
	CarpoolQuotaTradePage
	Enabled      bool                   `json:"enabled"`
	Quota        CarpoolQuotaTradeQuota `json:"quota"`
	TermsVersion string                 `json:"terms_version"`
	TermsNotice  string                 `json:"terms_notice"`
}
type CarpoolQuotaTradeEvent struct {
	ID          int64     `json:"id"`
	Action      string    `json:"action"`
	ActorUserID *int64    `json:"actor_user_id"`
	ActorName   string    `json:"actor_name"`
	IsAdmin     bool      `json:"is_admin"`
	BuyerUserID *int64    `json:"buyer_user_id"`
	CreatedAt   time.Time `json:"created_at"`
}
type CarpoolQuotaTradeFilter struct {
	Page, PageSize             int
	Status                     string
	GroupID, AccountID, UserID int64
}
type CarpoolQuotaTradeActor struct {
	UserID         int64
	SubscriptionID int64
	IsAdmin        bool
}
type CarpoolQuotaTradeRepository interface {
	Market(context.Context, int64, int64, int, int, ...string) (*CarpoolQuotaTradeMarket, error)
	Create(ctx context.Context, user, sub, amountCents, salePriceCents int64, requestID string) (*CarpoolQuotaTrade, error)
	Act(context.Context, CarpoolQuotaTradeActor, int64, string) (*CarpoolQuotaTrade, error)
	List(context.Context, int64, CarpoolQuotaTradeFilter) (*CarpoolQuotaTradePage, error)
	Events(context.Context, int64, int64) ([]CarpoolQuotaTradeEvent, error)
}
