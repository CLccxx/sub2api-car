package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type resetTargetsRepo struct {
	service.UserSubscriptionRepository
	ids   []int64
	err   error
	calls int
}

func (r *resetTargetsRepo) ListActiveIDs(context.Context) ([]int64, error) {
	r.calls++
	return r.ids, r.err
}

func TestSubscriptionResetQuotaTargets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name   string
		ids    []int64
		err    error
		status int
	}{
		{name: "global snapshot", ids: []int64{1, 3, 205}, status: http.StatusOK},
		{name: "empty snapshot", status: http.StatusOK},
		{name: "query failure", err: errors.New("database unavailable"), status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &resetTargetsRepo{ids: test.ids, err: test.err}
			svc := service.NewSubscriptionService(nil, repo, nil, nil, nil)
			t.Cleanup(svc.Stop)
			router := gin.New()
			router.GET("/targets", NewSubscriptionHandler(svc).ResetQuotaTargets)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/targets?page=2&group_id=99&user_id=12&status=revoked&platform=openai", nil))
			require.Equal(t, test.status, response.Code, response.Body.String())
			require.Equal(t, 1, repo.calls)
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			if test.err == nil {
				var body struct {
					Data struct {
						IDs []int64 `json:"subscription_ids"`
					} `json:"data"`
				}
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				require.NotNil(t, body.Data.IDs)
				require.Equal(t, append([]int64{}, test.ids...), body.Data.IDs)
			}
		})
	}
}
