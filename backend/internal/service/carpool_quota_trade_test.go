package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type quotaTradeRepositoryStub struct {
	created        int
	cents          int64
	salePriceCents int64
	action         string
	marketCalls    int
	view           string
	page, size     int
}

func (r *quotaTradeRepositoryStub) Market(_ context.Context, _, _ int64, page, size int, view ...string) (*CarpoolQuotaTradeMarket, error) {
	r.marketCalls++
	r.page, r.size = page, size
	r.view = view[0]
	return &CarpoolQuotaTradeMarket{}, nil
}
func (r *quotaTradeRepositoryStub) Create(_ context.Context, _, _, cents, salePriceCents int64, _ string) (*CarpoolQuotaTrade, error) {
	r.created++
	r.cents = cents
	r.salePriceCents = salePriceCents
	return &CarpoolQuotaTrade{}, nil
}
func (r *quotaTradeRepositoryStub) Act(_ context.Context, _ CarpoolQuotaTradeActor, _ int64, action string) (*CarpoolQuotaTrade, error) {
	r.action = action
	return &CarpoolQuotaTrade{}, nil
}
func (r *quotaTradeRepositoryStub) List(context.Context, int64, CarpoolQuotaTradeFilter) (*CarpoolQuotaTradePage, error) {
	return &CarpoolQuotaTradePage{}, nil
}
func (r *quotaTradeRepositoryStub) Events(context.Context, int64, int64) ([]CarpoolQuotaTradeEvent, error) {
	return nil, nil
}
func TestCarpoolQuotaTradeServiceMarketView(t *testing.T) {
	for _, tc := range []struct {
		name string
		view []string
		want string
	}{
		{"default", nil, "current"},
		{"current", []string{"current"}, "current"},
		{"history", []string{"history"}, "history"},
		{"empty", []string{""}, ""},
		{"invalid", []string{"all"}, ""},
		{"case_sensitive", []string{"CURRENT"}, ""},
		{"multiple", []string{"current", "history"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &quotaTradeRepositoryStub{}
			s := &CarpoolQuotaTradeService{repo: r}
			_, err := s.Market(context.Background(), 1, 11, 0, 200, tc.view...)
			if tc.want == "" {
				require.Error(t, err)
				require.Equal(t, 400, infraerrors.Code(err))
				require.Zero(t, r.marketCalls)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, r.marketCalls)
			require.Equal(t, tc.want, r.view)
			require.Equal(t, 1, r.page)
			require.Equal(t, 100, r.size)
		})
	}
}

func TestCarpoolQuotaTradeAmountCents(t *testing.T) {
	for _, tc := range []struct {
		amount float64
		cents  int64
	}{{0.01, 1}, {0.29, 29}, {1.23, 123}, {9999999999.99, 999999999999}} {
		got, err := CarpoolQuotaTradeAmountCents(tc.amount)
		require.NoError(t, err)
		require.Equal(t, tc.cents, got)
	}
	for _, v := range []float64{0, -1, 0.001, 1.001, 10000000000.01, math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := CarpoolQuotaTradeAmountCents(v)
		require.Error(t, err)
	}
}
func TestCarpoolQuotaTradeSalePriceCents(t *testing.T) {
	for _, tc := range []struct {
		price float64
		cents int64
	}{{0.01, 1}, {0.29, 29}, {45.67, 4567}, {9999999999.99, 999999999999}, {10000000000, 1000000000000}} {
		t.Run(fmt.Sprint(tc.price), func(t *testing.T) {
			got, err := CarpoolQuotaTradeSalePriceCents(tc.price)
			require.NoError(t, err)
			require.Equal(t, tc.cents, got)
			r := &quotaTradeRepositoryStub{}
			s := &CarpoolQuotaTradeService{repo: r}
			_, err = s.Create(context.Background(), 1, 11, 1.23, tc.price, "b234508e-16c2-4bb6-aeea-36e79be53fd7", true)
			require.NoError(t, err)
			require.Equal(t, tc.cents, r.salePriceCents)
			require.Equal(t, int64(123), r.cents, "CNY price must not replace the USD quota amount")
		})
	}
	for _, price := range []float64{0, -0.01, -1, 0.001, 1.001, 45.678, 10000000000.01, math.MaxFloat64, math.SmallestNonzeroFloat64, math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprint(price), func(t *testing.T) {
			_, err := CarpoolQuotaTradeSalePriceCents(price)
			require.Error(t, err)
			require.Equal(t, 400, infraerrors.Code(err))
			r := &quotaTradeRepositoryStub{}
			s := &CarpoolQuotaTradeService{repo: r}
			_, err = s.Create(context.Background(), 1, 11, 1.23, price, "b234508e-16c2-4bb6-aeea-36e79be53fd7", true)
			require.Error(t, err)
			require.Equal(t, "QUOTA_TRADE_SALE_PRICE", infraerrors.Reason(err))
			require.Zero(t, r.created, "invalid prices must not reach storage or freeze quota")
		})
	}
}

func TestCarpoolQuotaTradeSalePriceJSON(t *testing.T) {
	price := 45.67
	for _, tc := range []struct {
		price *float64
		want  string
	}{{nil, "null"}, {&price, "45.67"}} {
		encoded, err := json.Marshal(CarpoolQuotaTrade{SalePriceCNY: tc.price})
		require.NoError(t, err)
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(encoded, &fields))
		require.Contains(t, fields, "sale_price_cny")
		require.Equal(t, tc.want, string(fields["sale_price_cny"]))
	}
}

func TestCarpoolQuotaTradeServiceValidation(t *testing.T) {
	r := &quotaTradeRepositoryStub{}
	s := &CarpoolQuotaTradeService{repo: r}
	ctx := context.Background()
	key := "b234508e-16c2-4bb6-aeea-36e79be53fd7"
	_, err := s.Create(ctx, 1, 1, 1, 45.67, key, false)
	require.Error(t, err)
	_, err = s.Create(ctx, 1, 1, 1, 45.67, "not-uuid", true)
	require.Error(t, err)
	_, err = s.Create(ctx, 1, 1, 0.001, 45.67, key, true)
	require.Error(t, err)
	require.Zero(t, r.created)
	_, err = s.Create(ctx, 1, 1, 1.23, 45.67, key, true)
	require.NoError(t, err)
	require.Equal(t, int64(123), r.cents)
	require.Equal(t, int64(4567), r.salePriceCents)
	_, err = s.Act(ctx, CarpoolQuotaTradeActor{UserID: 1}, 1, "cancel-reservation")
	require.Error(t, err)
	require.Empty(t, r.action)
	_, err = s.Act(ctx, CarpoolQuotaTradeActor{UserID: 1, IsAdmin: true}, 1, "cancel-reservation")
	require.NoError(t, err)
	require.Equal(t, "cancel-reservation", r.action)
	_, err = s.Act(ctx, CarpoolQuotaTradeActor{UserID: 1, IsAdmin: true}, 1, "refund")
	require.Error(t, err)
}
