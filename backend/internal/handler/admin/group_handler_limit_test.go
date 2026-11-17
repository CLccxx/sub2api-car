package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUpdateGroupRequestLimitFieldsTriState(t *testing.T) {
	t.Run("omitted means unchanged", func(t *testing.T) {
		var req UpdateGroupRequest
		require.NoError(t, json.Unmarshal([]byte(`{}`), &req))
		require.Nil(t, req.DailyLimitUSD.ToServiceInput())
		require.Nil(t, req.WeeklyLimitUSD.ToServiceInput())
		require.Nil(t, req.MonthlyLimitUSD.ToServiceInput())
	})

	t.Run("null means unlimited", func(t *testing.T) {
		var req UpdateGroupRequest
		require.NoError(t, json.Unmarshal([]byte(`{"daily_limit_usd":null}`), &req))
		limit := req.DailyLimitUSD.ToServiceInput()
		require.NotNil(t, limit)
		require.Negative(t, *limit)
	})

	t.Run("number is preserved", func(t *testing.T) {
		var req UpdateGroupRequest
		require.NoError(t, json.Unmarshal([]byte(`{"weekly_limit_usd":0,"monthly_limit_usd":42.5}`), &req))
		require.Equal(t, 0.0, *req.WeeklyLimitUSD.ToServiceInput())
		require.Equal(t, 42.5, *req.MonthlyLimitUSD.ToServiceInput())
	})
}

func TestGroupRequestsDecodeCarpoolBonusUSD(t *testing.T) {
	for _, tt := range []struct {
		name    string
		body    string
		present bool
		want    float64
	}{
		{name: "omitted", body: `{}`},
		{name: "null", body: `{"carpool_bonus_usd":null}`},
		{name: "zero", body: `{"carpool_bonus_usd":0}`, present: true},
		{name: "cents", body: `{"carpool_bonus_usd":125.75}`, present: true, want: 125.75},
		{name: "maximum", body: `{"carpool_bonus_usd":10000000000}`, present: true, want: 10000000000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var create CreateGroupRequest
			var update UpdateGroupRequest
			require.NoError(t, json.Unmarshal([]byte(tt.body), &create))
			require.NoError(t, json.Unmarshal([]byte(tt.body), &update))
			for _, bonus := range []*float64{create.CarpoolBonusUSD, update.CarpoolBonusUSD} {
				if !tt.present {
					require.Nil(t, bonus)
					continue
				}
				require.NotNil(t, bonus)
				require.Equal(t, tt.want, *bonus)
			}
		})
	}
}

func TestGroupHandlerCarpoolBonusUSDInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name    string
		field   string
		present bool
		want    float64
		invalid bool
	}{
		{name: "omitted"},
		{name: "null", field: `,"carpool_bonus_usd":null`},
		{name: "zero", field: `,"carpool_bonus_usd":0`, present: true},
		{name: "cents", field: `,"carpool_bonus_usd":125.75`, present: true, want: 125.75},
		{name: "maximum", field: `,"carpool_bonus_usd":10000000000`, present: true, want: 10000000000},
		// The service, not the request binder, owns the sub-cent precision check.
		{name: "precision passed to service", field: `,"carpool_bonus_usd":1.001`, present: true, want: 1.001},
		{name: "negative", field: `,"carpool_bonus_usd":-0.01`, invalid: true},
		{name: "above maximum", field: `,"carpool_bonus_usd":10000000000.01`, invalid: true},
		{name: "non number", field: `,"carpool_bonus_usd":"NaN"`, invalid: true},
		{name: "overflow", field: `,"carpool_bonus_usd":1e309`, invalid: true},
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			t.Run(tt.name+"/"+method, func(t *testing.T) {
				svc := newStubAdminService()
				h := NewGroupHandler(svc, nil, nil)
				r := gin.New()
				r.POST("/groups", h.Create)
				r.PUT("/groups/:id", h.Update)
				path := "/groups"
				if method == http.MethodPut {
					path += "/1"
				}
				req := httptest.NewRequest(method, path, strings.NewReader(`{"name":"carpool"`+tt.field+`}`))
				req.Header.Set("Content-Type", "application/json")
				res := httptest.NewRecorder()
				r.ServeHTTP(res, req)
				if tt.invalid {
					require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
					require.Empty(t, svc.createdGroups)
					require.Empty(t, svc.updatedGroups)
					return
				}
				require.Equal(t, http.StatusOK, res.Code, res.Body.String())
				var bonus *float64
				if method == http.MethodPost {
					require.Len(t, svc.createdGroups, 1)
					bonus = svc.createdGroups[0].CarpoolBonusUSD
				} else {
					require.Len(t, svc.updatedGroups, 1)
					bonus = svc.updatedGroups[0].CarpoolBonusUSD
				}
				if !tt.present {
					require.Nil(t, bonus)
					return
				}
				require.NotNil(t, bonus)
				require.Equal(t, tt.want, *bonus)
			})
		}
	}
}
