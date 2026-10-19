package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type sharedUsageHandlerStub struct {
	userID         int64
	subscriptionID int64
	err            error
}

func (s *sharedUsageHandlerStub) Get(_ context.Context, userID, subscriptionID int64) (*service.SharedSubscriptionUsage, error) {
	s.userID, s.subscriptionID = userID, subscriptionID
	return &service.SharedSubscriptionUsage{Status: "no_data"}, s.err
}

func TestSubscriptionSharedUsageHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name, id string
		auth     bool
		err      error
		status   int
	}{
		{"unauthenticated", "1", false, nil, 401},
		{"invalid ID", "abc", true, nil, 400},
		{"negative ID", "-1", true, nil, 400},
		{"own subscription", "12", true, nil, 200},
		{"inaccessible subscription", "12", true, service.ErrSubscriptionNotFound, 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &sharedUsageHandlerStub{err: test.err}
			h := &SubscriptionHandler{sharedUsage: stub}
			router := gin.New()
			router.GET("/subscriptions/:id/shared-usage", func(c *gin.Context) {
				if test.auth {
					c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7})
				}
				h.GetSharedUsage(c)
			})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/subscriptions/"+test.id+"/shared-usage?user_id=999&account_id=888&force=true", nil))
			require.Equal(t, test.status, w.Code, w.Body.String())
			require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			if test.status == 200 || test.status == 404 {
				require.Equal(t, int64(7), stub.userID)
				require.Equal(t, int64(12), stub.subscriptionID)
			} else {
				require.Zero(t, stub.userID)
			}
			if test.status == 200 {
				require.Contains(t, w.Body.String(), `"seven_day":null`)
			}
		})
	}
}
