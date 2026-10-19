package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const resetVoteRequestID = "f9b28370-7b65-47bf-bbdf-9fe5f43604ea"

type resetVoteHandlerCall struct {
	method         string
	userID         int64
	subscriptionID int64
	proposalID     int64
	requestID      string
}

type resetVoteHandlerStub struct {
	calls []resetVoteHandlerCall
	ctx   context.Context
	view  *service.ResetVoteView
	err   error
}

var _ resetVoteService = (*resetVoteHandlerStub)(nil)

func TestSubscriptionResetVoteConstructor(t *testing.T) {
	subscriptions := &service.SubscriptionService{}
	sharedUsage := &service.SharedSubscriptionUsageService{}
	resetVote := &service.CarpoolResetVoteService{}
	h := NewSubscriptionHandler(subscriptions, sharedUsage, resetVote)
	require.Same(t, subscriptions, h.subscriptionService)
	require.Same(t, sharedUsage, h.sharedUsage)
	require.Same(t, resetVote, h.resetVote)
}

func (s *resetVoteHandlerStub) Get(ctx context.Context, userID, subscriptionID int64) (*service.ResetVoteView, error) {
	s.ctx = ctx
	s.calls = append(s.calls, resetVoteHandlerCall{method: "get", userID: userID, subscriptionID: subscriptionID})
	return s.view, s.err
}

func (s *resetVoteHandlerStub) GetRequest(ctx context.Context, userID, subscriptionID int64, requestID string) (*service.ResetVoteView, error) {
	s.ctx = ctx
	s.calls = append(s.calls, resetVoteHandlerCall{method: "get-request", userID: userID, subscriptionID: subscriptionID, requestID: requestID})
	return s.view, s.err
}

func TestSubscriptionResetVoteRequestReconciliation(t *testing.T) {
	for _, key := range []string{resetVoteRequestID, "invalid"} {
		t.Run(key, func(t *testing.T) {
			stub := &resetVoteHandlerStub{view: &service.ResetVoteView{}}
			h := &SubscriptionHandler{resetVote: stub}
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.GET("/subscriptions/:id/reset-vote", func(c *gin.Context) {
				c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7})
				h.GetResetVote(c)
			})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/subscriptions/12/reset-vote?request_id="+key, nil))
			if key == "invalid" {
				require.Equal(t, 400, w.Code)
				require.Empty(t, stub.calls)
			} else {
				require.Equal(t, 200, w.Code)
				require.Equal(t, []resetVoteHandlerCall{{method: "get-request", userID: 7, subscriptionID: 12, requestID: key}}, stub.calls)
			}
		})
	}
}

func (s *resetVoteHandlerStub) Create(ctx context.Context, userID, subscriptionID int64, requestID string) (*service.ResetVoteView, error) {
	s.ctx = ctx
	s.calls = append(s.calls, resetVoteHandlerCall{method: "create", userID: userID, subscriptionID: subscriptionID, requestID: requestID})
	return s.view, s.err
}

func (s *resetVoteHandlerStub) Agree(ctx context.Context, userID, subscriptionID, proposalID int64) (*service.ResetVoteView, error) {
	s.ctx = ctx
	s.calls = append(s.calls, resetVoteHandlerCall{method: "agree", userID: userID, subscriptionID: subscriptionID, proposalID: proposalID})
	return s.view, s.err
}

type resetVoteHandlerEndpoint struct {
	name   string
	method string
	path   string
	body   string
	handle func(*SubscriptionHandler, *gin.Context)
}

func resetVoteHandlerEndpoints() []resetVoteHandlerEndpoint {
	return []resetVoteHandlerEndpoint{
		{"get", http.MethodGet, "/api/v1/subscriptions/:id/reset-vote", "", (*SubscriptionHandler).GetResetVote},
		{"create", http.MethodPost, "/api/v1/subscriptions/:id/reset-vote", `{"request_id":"` + resetVoteRequestID + `"}`, (*SubscriptionHandler).CreateResetVote},
		{"agree", http.MethodPost, "/api/v1/subscriptions/:id/reset-vote/:proposal/agree", `{}`, (*SubscriptionHandler).AgreeResetVote},
	}
}

func performResetVoteRequest(t *testing.T, h *SubscriptionHandler, endpoint resetVoteHandlerEndpoint, subject any, id, proposal, body string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Handle(endpoint.method, endpoint.path, func(c *gin.Context) {
		if subject != nil {
			c.Set(string(middleware2.ContextKeyUser), subject)
		}
		c.Set("user_id", int64(999))
		endpoint.handle(h, c)
	})
	path := strings.NewReplacer(":id", id, ":proposal", proposal).Replace(endpoint.path)
	req := httptest.NewRequest(endpoint.method, path+"?user_id=999&subscription_id=888&proposal_id=777", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	return w
}

func TestSubscriptionResetVoteRequiresAuthSubject(t *testing.T) {
	for _, endpoint := range resetVoteHandlerEndpoints() {
		t.Run(endpoint.name, func(t *testing.T) {
			for _, test := range []struct {
				name    string
				subject any
			}{
				{"missing", nil},
				{"legacy user object", &service.User{ID: 7}},
				{"raw user ID", int64(7)},
				{"zero user ID", middleware2.AuthSubject{}},
				{"negative user ID", middleware2.AuthSubject{UserID: -1}},
			} {
				t.Run(test.name, func(t *testing.T) {
					stub := &resetVoteHandlerStub{}
					w := performResetVoteRequest(t, &SubscriptionHandler{resetVote: stub}, endpoint, test.subject, "invalid", "invalid", `{`, context.Background())
					require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
					require.Empty(t, stub.calls)
				})
			}
		})
	}
}

func TestSubscriptionResetVoteRejectsInvalidIDs(t *testing.T) {
	invalidIDs := []string{"0", "-1", "abc", "1.5", "9223372036854775808", "-9223372036854775809"}
	for _, endpoint := range resetVoteHandlerEndpoints() {
		t.Run(endpoint.name, func(t *testing.T) {
			for _, id := range invalidIDs {
				t.Run("subscription/"+id, func(t *testing.T) {
					stub := &resetVoteHandlerStub{}
					w := performResetVoteRequest(t, &SubscriptionHandler{resetVote: stub}, endpoint, middleware2.AuthSubject{UserID: 7}, id, "34", endpoint.body, context.Background())
					require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
					require.JSONEq(t, `{"code":400,"message":"Invalid subscription ID"}`, w.Body.String())
					require.Empty(t, stub.calls)
				})
				if endpoint.name == "agree" {
					t.Run("proposal/"+id, func(t *testing.T) {
						stub := &resetVoteHandlerStub{}
						w := performResetVoteRequest(t, &SubscriptionHandler{resetVote: stub}, endpoint, middleware2.AuthSubject{UserID: 7}, "12", id, endpoint.body, context.Background())
						require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
						require.JSONEq(t, `{"code":400,"message":"Invalid proposal ID"}`, w.Body.String())
						require.Empty(t, stub.calls)
					})
				}
			}
		})
	}
}

func TestSubscriptionResetVoteCreateValidatesRequestID(t *testing.T) {
	endpoint := resetVoteHandlerEndpoints()[1]
	for _, body := range []string{
		"", `{`, `{}`, `null`, `[]`, `{"request_id":null}`, `{"request_id":123}`,
		`{"request_id":""}`, `{"request_id":"not-a-uuid"}`,
		`{"request_id":"f9b28370-7b65-47bf-bbdf-9fe5f43604eg"}`,
		`{"request_id":" f9b28370-7b65-47bf-bbdf-9fe5f43604ea"}`,
	} {
		t.Run(body, func(t *testing.T) {
			stub := &resetVoteHandlerStub{}
			w := performResetVoteRequest(t, &SubscriptionHandler{resetVote: stub}, endpoint, middleware2.AuthSubject{UserID: 7}, "12", "34", body, context.Background())
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.JSONEq(t, `{"code":400,"message":"Invalid request_id: must be a UUID"}`, w.Body.String())
			require.Empty(t, stub.calls)
		})
	}
}

func TestSubscriptionResetVoteForwardsAuthenticatedScopeAndView(t *testing.T) {
	for _, endpoint := range resetVoteHandlerEndpoints() {
		t.Run(endpoint.name, func(t *testing.T) {
			stub := &resetVoteHandlerStub{view: &service.ResetVoteView{
				RequiredCount: 2,
				MemberCount:   3,
				Proposal: &service.ResetVoteProposalView{
					ID: 34, Status: "pending", AgreedCount: 1, RequiredCount: 2, CanAgree: true,
					ExpiresAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC),
				},
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := `{"request_id":"` + resetVoteRequestID + `","user_id":999,"subscription_id":888,"proposal_id":777}`
			w := performResetVoteRequest(t, &SubscriptionHandler{resetVote: stub}, endpoint, middleware2.AuthSubject{UserID: 7}, "12", "34", body, ctx)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			wantCall := resetVoteHandlerCall{method: endpoint.name, userID: 7, subscriptionID: 12}
			if endpoint.name == "create" {
				wantCall.requestID = resetVoteRequestID
			}
			if endpoint.name == "agree" {
				wantCall.proposalID = 34
			}
			require.Equal(t, []resetVoteHandlerCall{wantCall}, stub.calls)
			require.Same(t, ctx, stub.ctx)
			require.JSONEq(t, `{
				"code":0,"message":"success","data":{
					"required_count":2,"member_count":3,"can_create":false,"blocked_reason":null,
					"proposal":{"id":34,"status":"pending","agreed_count":1,"required_count":2,
						"my_vote":false,"can_agree":true,"expires_at":"2026-10-08T12:00:00Z","reason":null}
				}
			}`, w.Body.String())
		})
	}
}

func TestSubscriptionResetVoteAcceptsPositiveInt64IDs(t *testing.T) {
	for _, endpoint := range resetVoteHandlerEndpoints() {
		t.Run(endpoint.name, func(t *testing.T) {
			const maxID = int64(9223372036854775807)
			stub := &resetVoteHandlerStub{view: &service.ResetVoteView{}}
			w := performResetVoteRequest(t, &SubscriptionHandler{resetVote: stub}, endpoint, middleware2.AuthSubject{UserID: maxID}, "9223372036854775807", "9223372036854775807", endpoint.body, context.Background())
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Len(t, stub.calls, 1)
			require.Equal(t, maxID, stub.calls[0].userID)
			require.Equal(t, maxID, stub.calls[0].subscriptionID)
			if endpoint.name == "agree" {
				require.Equal(t, maxID, stub.calls[0].proposalID)
			}
		})
	}
}

func TestSubscriptionResetVoteGetWithoutProposal(t *testing.T) {
	stub := &resetVoteHandlerStub{view: &service.ResetVoteView{RequiredCount: 2, MemberCount: 3, CanCreate: true}}
	endpoint := resetVoteHandlerEndpoints()[0]
	w := performResetVoteRequest(t, &SubscriptionHandler{resetVote: stub}, endpoint, middleware2.AuthSubject{UserID: 7}, "12", "34", "", context.Background())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, `{"code":0,"message":"success","data":{"required_count":2,"member_count":3,"can_create":true,"blocked_reason":null,"proposal":null}}`, w.Body.String())
}

func TestSubscriptionResetVoteMapsSafeServiceErrors(t *testing.T) {
	for _, endpoint := range resetVoteHandlerEndpoints() {
		t.Run(endpoint.name, func(t *testing.T) {
			for _, test := range []struct {
				name   string
				err    error
				status int
			}{
				{"inaccessible subscription", service.ErrSubscriptionNotFound, http.StatusNotFound},
				{"forbidden", infraerrors.Forbidden("VOTE_FORBIDDEN", "Vote unavailable"), http.StatusForbidden},
				{"conflict", infraerrors.Conflict("VOTE_CONFLICT", "Vote already closed"), http.StatusConflict},
				{"rate limited", infraerrors.TooManyRequests("VOTE_LIMITED", "Try again later"), http.StatusTooManyRequests},
			} {
				t.Run(test.name, func(t *testing.T) {
					stub := &resetVoteHandlerStub{err: fmt.Errorf("repository detail: %w", test.err)}
					w := performResetVoteRequest(t, &SubscriptionHandler{resetVote: stub}, endpoint, middleware2.AuthSubject{UserID: 7}, "12", "34", endpoint.body, context.Background())
					require.Equal(t, test.status, w.Code, w.Body.String())
					require.Contains(t, w.Body.String(), infraerrors.Reason(test.err))
					require.Contains(t, w.Body.String(), infraerrors.Message(test.err))
					require.NotContains(t, w.Body.String(), "repository detail")
					require.Len(t, stub.calls, 1)
				})
			}
		})
	}
}

func TestSubscriptionResetVoteSanitizesInternalErrors(t *testing.T) {
	const secret = "postgres://admin:password@db.private/reset_votes"
	for _, endpoint := range resetVoteHandlerEndpoints() {
		t.Run(endpoint.name, func(t *testing.T) {
			for _, err := range []error{
				errors.New(secret),
				infraerrors.InternalServer(secret, secret).WithMetadata(map[string]string{"source": secret}),
				fmt.Errorf("wrapped: %w", infraerrors.InternalServer(secret, secret)),
				infraerrors.ServiceUnavailable(secret, secret).WithMetadata(map[string]string{"source": secret}),
			} {
				stub := &resetVoteHandlerStub{err: err}
				w := performResetVoteRequest(t, &SubscriptionHandler{resetVote: stub}, endpoint, middleware2.AuthSubject{UserID: 7}, "12", "34", endpoint.body, context.Background())
				require.Equal(t, infraerrors.Code(err), w.Code, w.Body.String())
				require.JSONEq(t, fmt.Sprintf(`{"code":%d,"message":"internal error"}`, infraerrors.Code(err)), w.Body.String())
				require.NotContains(t, w.Body.String(), secret)
			}
		})
	}
}

func TestSubscriptionResetVoteAgreeForwardsCrossProposal(t *testing.T) {
	stub := &resetVoteHandlerStub{err: infraerrors.NotFound("RESET_VOTE_NOT_FOUND", "Reset vote not found")}
	endpoint := resetVoteHandlerEndpoints()[2]
	w := performResetVoteRequest(t, &SubscriptionHandler{resetVote: stub}, endpoint, middleware2.AuthSubject{UserID: 7}, "12", "987", `{}`, context.Background())
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.Equal(t, []resetVoteHandlerCall{{method: "agree", userID: 7, subscriptionID: 12, proposalID: 987}}, stub.calls)
}
