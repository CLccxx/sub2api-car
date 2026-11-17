package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type quotaTradeHandlerStub struct {
	actor      service.CarpoolQuotaTradeActor
	calls      int
	user, sub  int64
	accepted   bool
	salePrice  float64
	view       string
	page, size int
}

func (s *quotaTradeHandlerStub) Market(_ context.Context, u, id int64, page, size int, view ...string) (*service.CarpoolQuotaTradeMarket, error) {
	s.calls++
	s.user = u
	s.sub = id
	s.page, s.size = page, size
	s.view = view[0]
	return &service.CarpoolQuotaTradeMarket{TermsVersion: service.CarpoolQuotaTradeTermsVersion, TermsNotice: service.CarpoolQuotaTradeTermsNotice}, nil
}
func (s *quotaTradeHandlerStub) Create(_ context.Context, u, id int64, _, price float64, _ string, a bool) (*service.CarpoolQuotaTrade, error) {
	s.calls++
	s.user = u
	s.sub = id
	s.accepted = a
	s.salePrice = price
	return &service.CarpoolQuotaTrade{SalePriceCNY: &price}, nil
}
func (s *quotaTradeHandlerStub) Act(_ context.Context, a service.CarpoolQuotaTradeActor, _ int64, _ string) (*service.CarpoolQuotaTrade, error) {
	s.calls++
	s.actor = a
	return &service.CarpoolQuotaTrade{}, nil
}
func (s *quotaTradeHandlerStub) List(context.Context, int64, service.CarpoolQuotaTradeFilter) (*service.CarpoolQuotaTradePage, error) {
	s.calls++
	return &service.CarpoolQuotaTradePage{}, nil
}
func (s *quotaTradeHandlerStub) Events(context.Context, int64, int64) ([]service.CarpoolQuotaTradeEvent, error) {
	s.calls++
	return []service.CarpoolQuotaTradeEvent{}, nil
}
func TestCarpoolQuotaTradeHandlerAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name   string
		user   int64
		admin  bool
		path   string
		status int
	}{{"unauthenticated", 0, false, "/subscriptions/9/quota-trades", 401}, {"invalid_id", 2, false, "/subscriptions/no/quota-trades", 400}, {"owner_context", 2, false, "/subscriptions/9/quota-trades", 200}, {"invalid_page", 2, false, "/subscriptions/9/quota-trades?page=-1", 400}} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &quotaTradeHandlerStub{}
			h := &CarpoolQuotaTradeHandler{svc: stub}
			r := gin.New()
			r.Use(func(c *gin.Context) {
				if tc.user > 0 {
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: tc.user})
				}
			})
			r.GET("/subscriptions/:id/quota-trades", h.Market)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			require.Equal(t, tc.status, w.Code)
			if w.Code == 200 {
				require.Equal(t, int64(2), stub.user)
				require.Equal(t, int64(9), stub.sub)
			}
		})
	}
}
func TestCarpoolQuotaTradeHandlerMarketView(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		query, want string
	}{
		{"", "current"},
		{"&view=current", "current"},
		{"&view=history", "history"},
		{"&view=", ""},
		{"&view=all", ""},
		{"&view=CURRENT", ""},
		{"&view=current&view=history", ""},
	} {
		t.Run(tc.query, func(t *testing.T) {
			stub := &quotaTradeHandlerStub{}
			h := &CarpoolQuotaTradeHandler{svc: stub}
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2})
			})
			r.GET("/subscriptions/:id/quota-trades", h.Market)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/subscriptions/12/quota-trades?page=3&page_size=2"+tc.query, nil))
			require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			if tc.want == "" {
				require.Equal(t, http.StatusBadRequest, w.Code)
				require.Zero(t, stub.calls)
				return
			}
			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, tc.want, stub.view)
			require.Equal(t, 1, stub.calls)
			require.Equal(t, 3, stub.page)
			require.Equal(t, 2, stub.size)
			var body struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			for _, field := range []string{"items", "total", "page", "page_size", "enabled", "quota", "terms_version", "terms_notice"} {
				require.Contains(t, body.Data, field)
			}
		})
	}
}

func TestCarpoolQuotaTradeHandlerAdminAndBodyIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &quotaTradeHandlerStub{}
	h := &CarpoolQuotaTradeHandler{svc: stub}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
		c.Set(string(middleware.ContextKeyUserRole), service.RoleUser)
	})
	r.POST("/subscriptions/:id/quota-trades", h.Create)
	r.POST("/admin/carpool-quota-trades/:trade_id/confirm", h.AdminConfirm)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/carpool-quota-trades/1/confirm", nil))
	require.Equal(t, 403, w.Code)
	require.Zero(t, stub.calls)
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/10/quota-trades", bytes.NewBufferString(`{"amount_usd":1.23,"sale_price_cny":45.67,"request_id":"client-key","accepted_terms":true,"seller_user_id":99}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	require.Equal(t, int64(7), stub.user)
	require.Equal(t, int64(10), stub.sub)
	require.True(t, stub.accepted)
	require.Equal(t, 45.67, stub.salePrice)
	require.Contains(t, w.Body.String(), `"sale_price_cny":45.67`)
}

func TestCarpoolQuotaTradeHandlerRequiredSalePrice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, field string
		valid       bool
		price       float64
	}{
		{"missing", "", false, 0},
		{"null", `,"sale_price_cny":null`, false, 0},
		{"zero", `,"sale_price_cny":0`, false, 0},
		{"negative", `,"sale_price_cny":-1`, false, 0},
		{"fraction_cent", `,"sale_price_cny":0.001`, false, 0},
		{"third_decimal", `,"sale_price_cny":12.345`, false, 0},
		{"over_max", `,"sale_price_cny":10000000000.01`, false, 0},
		{"extreme", `,"sale_price_cny":1e309`, false, 0},
		{"tiny", `,"sale_price_cny":5e-324`, false, 0},
		{"nan", `,"sale_price_cny":NaN`, false, 0},
		{"infinity", `,"sale_price_cny":Infinity`, false, 0},
		{"string", `,"sale_price_cny":"12.34"`, false, 0},
		{"one_cent", `,"sale_price_cny":0.01`, true, 0.01},
		{"decimal", `,"sale_price_cny":45.67`, true, 45.67},
		{"max", `,"sale_price_cny":10000000000`, true, 10000000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &quotaTradeHandlerStub{}
			h := &CarpoolQuotaTradeHandler{svc: stub}
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
			})
			r.POST("/subscriptions/:id/quota-trades", h.Create)
			body := fmt.Sprintf(`{"amount_usd":1.23,"request_id":"b234508e-16c2-4bb6-aeea-36e79be53fd7","accepted_terms":true%s}`, tc.field)
			req := httptest.NewRequest(http.MethodPost, "/subscriptions/10/quota-trades", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if !tc.valid {
				require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
				require.Zero(t, stub.calls, "handler must reject invalid price before calling service")
				return
			}
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, 1, stub.calls)
			require.Equal(t, tc.price, stub.salePrice)
		})
	}
}
