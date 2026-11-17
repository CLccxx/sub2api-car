package handler

import (
	"context"
	"net/http"
	"strconv"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type carpoolQuotaTradeService interface {
	Market(context.Context, int64, int64, int, int, ...string) (*service.CarpoolQuotaTradeMarket, error)
	Create(context.Context, int64, int64, float64, float64, string, bool) (*service.CarpoolQuotaTrade, error)
	Act(context.Context, service.CarpoolQuotaTradeActor, int64, string) (*service.CarpoolQuotaTrade, error)
	List(context.Context, int64, service.CarpoolQuotaTradeFilter) (*service.CarpoolQuotaTradePage, error)
	Events(context.Context, int64, int64) ([]service.CarpoolQuotaTradeEvent, error)
}
type CarpoolQuotaTradeHandler struct{ svc carpoolQuotaTradeService }

func NewCarpoolQuotaTradeHandler(svc *service.CarpoolQuotaTradeService) *CarpoolQuotaTradeHandler {
	return &CarpoolQuotaTradeHandler{svc: svc}
}
func quotaTradeHTTP(c *gin.Context, data any, err error) {
	if err != nil {
		if infraerrors.Code(err) >= http.StatusInternalServerError {
			response.Error(c, http.StatusInternalServerError, infraerrors.UnknownMessage)
		} else {
			response.ErrorFrom(c, err)
		}
		return
	}
	response.Success(c, data)
}
func quotaTradePositiveID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id < 1 {
		response.BadRequest(c, "Invalid "+name)
		return 0, false
	}
	return id, true
}
func quotaTradeActor(c *gin.Context, admin bool) (service.CarpoolQuotaTradeActor, bool) {
	c.Header("Cache-Control", "no-store")
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID < 1 {
		response.Unauthorized(c, "User not found in context")
		return service.CarpoolQuotaTradeActor{}, false
	}
	a := service.CarpoolQuotaTradeActor{UserID: subject.UserID, IsAdmin: admin}
	if admin {
		role, _ := c.Get(string(middleware.ContextKeyUserRole))
		if role != service.RoleAdmin {
			response.Forbidden(c, "Administrator access required")
			return a, false
		}
	} else {
		var valid bool
		a.SubscriptionID, valid = quotaTradePositiveID(c, "id")
		if !valid {
			return a, false
		}
	}
	return a, true
}
func quotaTradePage(c *gin.Context) (int, int, bool) {
	page, size := 1, 20
	for _, p := range []struct {
		name string
		dst  *int
	}{{"page", &page}, {"page_size", &size}} {
		if raw, exists := c.GetQuery(p.name); exists {
			n, e := strconv.Atoi(raw)
			if e != nil || n < 1 {
				response.BadRequest(c, "Invalid "+p.name)
				return 0, 0, false
			}
			*p.dst = n
		}
	}
	if size > 100 {
		size = 100
	}
	if page > 1000000 {
		response.BadRequest(c, "Page is too large")
		return 0, 0, false
	}
	return page, size, true
}
func (h *CarpoolQuotaTradeHandler) Market(c *gin.Context) {
	a, ok := quotaTradeActor(c, false)
	if !ok {
		return
	}
	p, s, ok := quotaTradePage(c)
	if !ok {
		return
	}
	view, err := service.CarpoolQuotaTradeMarketView(c.Request.URL.Query()["view"]...)
	if err != nil {
		quotaTradeHTTP(c, nil, err)
		return
	}
	v, err := h.svc.Market(c.Request.Context(), a.UserID, a.SubscriptionID, p, s, view)
	quotaTradeHTTP(c, v, err)
}
func (h *CarpoolQuotaTradeHandler) Create(c *gin.Context) {
	a, ok := quotaTradeActor(c, false)
	if !ok {
		return
	}
	var req struct {
		AmountUSD     float64 `json:"amount_usd"`
		SalePriceCNY  float64 `json:"sale_price_cny"`
		RequestID     string  `json:"request_id"`
		AcceptedTerms bool    `json:"accepted_terms"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid quota trade request")
		return
	}
	if _, err := service.CarpoolQuotaTradeSalePriceCents(req.SalePriceCNY); err != nil {
		quotaTradeHTTP(c, nil, err)
		return
	}
	t, err := h.svc.Create(c.Request.Context(), a.UserID, a.SubscriptionID, req.AmountUSD, req.SalePriceCNY, req.RequestID, req.AcceptedTerms)
	quotaTradeHTTP(c, t, err)
}
func (h *CarpoolQuotaTradeHandler) userAction(c *gin.Context, action string) {
	a, ok := quotaTradeActor(c, false)
	if !ok {
		return
	}
	id, ok := quotaTradePositiveID(c, "trade_id")
	if !ok {
		return
	}
	t, err := h.svc.Act(c.Request.Context(), a, id, action)
	quotaTradeHTTP(c, t, err)
}
func (h *CarpoolQuotaTradeHandler) Reserve(c *gin.Context)  { h.userAction(c, "reserve") }
func (h *CarpoolQuotaTradeHandler) Confirm(c *gin.Context)  { h.userAction(c, "confirm") }
func (h *CarpoolQuotaTradeHandler) Withdraw(c *gin.Context) { h.userAction(c, "withdraw") }
func (h *CarpoolQuotaTradeHandler) AdminList(c *gin.Context) {
	a, ok := quotaTradeActor(c, true)
	if !ok {
		return
	}
	p, s, ok := quotaTradePage(c)
	if !ok {
		return
	}
	f := service.CarpoolQuotaTradeFilter{Page: p, PageSize: s, Status: c.Query("status")}
	for _, v := range []struct {
		name string
		dst  *int64
	}{{"group_id", &f.GroupID}, {"account_id", &f.AccountID}, {"user_id", &f.UserID}} {
		if raw, exists := c.GetQuery(v.name); exists {
			n, e := strconv.ParseInt(raw, 10, 64)
			if e != nil || n < 1 {
				response.BadRequest(c, "Invalid "+v.name)
				return
			}
			*v.dst = n
		}
	}
	v, err := h.svc.List(c.Request.Context(), a.UserID, f)
	quotaTradeHTTP(c, v, err)
}
func (h *CarpoolQuotaTradeHandler) AdminEvents(c *gin.Context) {
	a, ok := quotaTradeActor(c, true)
	if !ok {
		return
	}
	id, ok := quotaTradePositiveID(c, "trade_id")
	if !ok {
		return
	}
	v, err := h.svc.Events(c.Request.Context(), a.UserID, id)
	quotaTradeHTTP(c, v, err)
}
func (h *CarpoolQuotaTradeHandler) adminAction(c *gin.Context, action string) {
	a, ok := quotaTradeActor(c, true)
	if !ok {
		return
	}
	id, ok := quotaTradePositiveID(c, "trade_id")
	if !ok {
		return
	}
	t, err := h.svc.Act(c.Request.Context(), a, id, action)
	quotaTradeHTTP(c, t, err)
}
func (h *CarpoolQuotaTradeHandler) AdminConfirm(c *gin.Context) { h.adminAction(c, "confirm") }
func (h *CarpoolQuotaTradeHandler) AdminCancelReservation(c *gin.Context) {
	h.adminAction(c, "cancel-reservation")
}
func (h *CarpoolQuotaTradeHandler) AdminWithdraw(c *gin.Context) { h.adminAction(c, "withdraw") }
