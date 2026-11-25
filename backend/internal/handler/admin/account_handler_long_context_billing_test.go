package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountUpdateBindsCarpoolQuotaCorrectionIntent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, body string
		want       *float64
		wantStatus int
	}{
		{"omitted", `{}`, nil, http.StatusOK},
		{"null", `{"carpool_quota_correction_usd":null}`, nil, http.StatusOK},
		{"clear", `{"carpool_quota_correction_usd":0}`, accountCorrectionTestPtr(0), http.StatusOK},
		{"scientific zero clears", `{"carpool_quota_correction_usd":-0.000e999999999999999999999}`, accountCorrectionTestPtr(0), http.StatusOK},
		{"signed", `{"carpool_quota_correction_usd":-12.34}`, accountCorrectionTestPtr(-12.34), http.StatusOK},
		{"trailing zeros", `{"carpool_quota_correction_usd":12.34000}`, accountCorrectionTestPtr(12.34), http.StatusOK},
		{"scientific cents", `{"carpool_quota_correction_usd":12340E-3}`, accountCorrectionTestPtr(12.34), http.StatusOK},
		{"positive bound", `{"carpool_quota_correction_usd":1e+10}`, accountCorrectionTestPtr(1e10), http.StatusOK},
		{"negative bound", `{"carpool_quota_correction_usd":-10000000000.00}`, accountCorrectionTestPtr(-1e10), http.StatusOK},
		{"one cent below bound", `{"carpool_quota_correction_usd":9999999999.99}`, accountCorrectionTestPtr(9999999999.99), http.StatusOK},
		{"raw extra is not typed intent", `{"extra":{"carpool_quota_correction_usd":5}}`, nil, http.StatusOK},
		{"quoted number", `{"carpool_quota_correction_usd":"12.34"}`, nil, http.StatusBadRequest},
		{"boolean", `{"carpool_quota_correction_usd":true}`, nil, http.StatusBadRequest},
		{"object", `{"carpool_quota_correction_usd":{}}`, nil, http.StatusBadRequest},
		{"array", `{"carpool_quota_correction_usd":[12.34]}`, nil, http.StatusBadRequest},
		{"precision lost in float", `{"carpool_quota_correction_usd":12.3400000000000001}`, nil, http.StatusBadRequest},
		{"range lost in float", `{"carpool_quota_correction_usd":10000000000.0000001}`, nil, http.StatusBadRequest},
		{"negative out of range", `{"carpool_quota_correction_usd":-10000000000.01}`, nil, http.StatusBadRequest},
		{"scientific sub-cent", `{"carpool_quota_correction_usd":1e-3}`, nil, http.StatusBadRequest},
		{"float underflow is not clear", `{"carpool_quota_correction_usd":1e-400}`, nil, http.StatusBadRequest},
		{"large positive exponent", `{"carpool_quota_correction_usd":1e1000000000}`, nil, http.StatusBadRequest},
		{"large negative exponent", `{"carpool_quota_correction_usd":1e-1000000000}`, nil, http.StatusBadRequest},
		{"exponent outside int range", `{"carpool_quota_correction_usd":1e999999999999999999999}`, nil, http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := newStubAdminService()
			handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.PUT("/accounts/:id", handler.Update)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPut, "/accounts/1", bytes.NewBufferString(tt.body))
			request.Header.Set("Content-Type", "application/json")

			router.ServeHTTP(recorder, request)

			require.Equal(t, tt.wantStatus, recorder.Code)
			if tt.wantStatus == http.StatusOK {
				require.Equal(t, 1, stub.updateAccountCalls)
				require.Equal(t, tt.want, stub.lastUpdateAccountInput.CarpoolQuotaCorrectionUSD)
			} else {
				require.Zero(t, stub.updateAccountCalls)
			}
		})
	}
}

func accountCorrectionTestPtr(value float64) *float64 { return &value }

func TestAccountUpdateBindsGroupSyncAndCarpoolQuotaCorrectionTogether(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := newStubAdminService()
	handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.PUT("/accounts/:id", handler.Update)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/accounts/1", bytes.NewBufferString(
		`{"upstream_billing_group_sync_enabled":false,"carpool_quota_correction_usd":-12.34}`,
	))
	request.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, stub.updateAccountCalls)
	disabled := false
	require.Equal(t, &disabled, stub.lastUpdateAccountInput.GroupSyncEnabled)
	require.Equal(t, accountCorrectionTestPtr(-12.34), stub.lastUpdateAccountInput.CarpoolQuotaCorrectionUSD)
}

func TestAccountAdminBoundariesRejectMalformedOpenAILongContextBillingValue(t *testing.T) {
	const malformedExtra = `"extra":{"openai_long_context_billing_enabled":"true"}`

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		mount  func(*gin.Engine, *AccountHandler)
		setup  func(*stubAdminService)
	}{
		{
			name:   "create",
			method: http.MethodPost,
			path:   "/accounts",
			body:   `{"name":"account","platform":"openai","type":"apikey","credentials":{"api_key":"test"},` + malformedExtra + `}`,
			mount:  func(router *gin.Engine, handler *AccountHandler) { router.POST("/accounts", handler.Create) },
		},
		{
			name:   "update",
			method: http.MethodPut,
			path:   "/accounts/1",
			body:   `{` + malformedExtra + `}`,
			mount:  func(router *gin.Engine, handler *AccountHandler) { router.PUT("/accounts/:id", handler.Update) },
			setup: func(stub *stubAdminService) {
				stub.updateAccountErr = infraerrors.BadRequest("OPENAI_LONG_CONTEXT_BILLING_INVALID", "invalid")
			},
		},
		{
			name:   "bulk update",
			method: http.MethodPost,
			path:   "/accounts/bulk-update",
			body:   `{"account_ids":[1],` + malformedExtra + `}`,
			mount: func(router *gin.Engine, handler *AccountHandler) {
				router.POST("/accounts/bulk-update", handler.BulkUpdate)
			},
			setup: func(stub *stubAdminService) {
				stub.bulkUpdateAccountErr = infraerrors.BadRequest("OPENAI_LONG_CONTEXT_BILLING_INVALID", "invalid")
			},
		},
		{
			name:   "batch create",
			method: http.MethodPost,
			path:   "/accounts/batch",
			body:   `{"accounts":[{"name":"account","platform":"openai","type":"apikey","credentials":{"api_key":"test"},` + malformedExtra + `}]}`,
			mount:  func(router *gin.Engine, handler *AccountHandler) { router.POST("/accounts/batch", handler.BatchCreate) },
		},
		{
			name:   "Codex session import",
			method: http.MethodPost,
			path:   "/accounts/import-codex-session",
			body:   `{"content":"token",` + malformedExtra + `}`,
			mount: func(router *gin.Engine, handler *AccountHandler) {
				router.POST("/accounts/import-codex-session", handler.ImportCodexSession)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			stub := newStubAdminService()
			if tt.setup != nil {
				tt.setup(stub)
			}
			handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			tt.mount(router, handler)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, tt.path, bytes.NewBufferString(tt.body))
			request.Header.Set("Content-Type", "application/json")

			router.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusBadRequest, recorder.Code)
			var responseBody struct {
				Reason string `json:"reason"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &responseBody))
			require.Equal(t, "OPENAI_LONG_CONTEXT_BILLING_INVALID", responseBody.Reason)
		})
	}
}

func TestAccountCreateBoundaryDoesNotApplyOpenAIValidationToOtherPlatforms(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewAccountHandler(newStubAdminService(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/accounts", handler.Create)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts", bytes.NewBufferString(
		`{"name":"account","platform":"anthropic","type":"apikey","credentials":{"api_key":"test"},"extra":{"openai_long_context_billing_enabled":"provider-owned"}}`,
	))
	request.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestApplyOAuthCredentialsRejectsMalformedOpenAILongContextBillingBeforeMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := newStubAdminService()
	stub.getAccountResult = &service.Account{
		ID:       1,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
	}
	handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/accounts/:id/apply-oauth-credentials", handler.ApplyOAuthCredentials)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts/1/apply-oauth-credentials", bytes.NewBufferString(
		`{"type":"oauth","credentials":{"access_token":"new-token"},"extra":{"openai_long_context_billing_enabled":"true"}}`,
	))
	request.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var responseBody struct {
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &responseBody))
	require.Equal(t, "OPENAI_LONG_CONTEXT_BILLING_INVALID", responseBody.Reason)
	require.Zero(t, stub.updateAccountCalls)
	require.Zero(t, stub.updateAccountExtraCalls)
}

func TestApplyOAuthCredentialsPreservesExistingNonAuthCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := newStubAdminService()
	stub.getAccountResult = &service.Account{
		ID:       1,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "old-token",
			"refresh_token": "old-refresh-token",
			"model_mapping": map[string]any{"gpt-5": "gpt-5"},
			"account_id":    "existing-account-id",
			"password":      "must-not-survive",
			"sso_token":     "must-not-survive",
			"cookie":        "must-not-survive",
		},
	}
	handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/accounts/:id/apply-oauth-credentials", handler.ApplyOAuthCredentials)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts/1/apply-oauth-credentials", bytes.NewBufferString(
		`{"type":"oauth","credentials":{"access_token":"new-token","refresh_token":"new-refresh-token"}}`,
	))
	request.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, stub.updateAccountCalls)
	require.Equal(t, map[string]any{
		"access_token":  "new-token",
		"refresh_token": "new-refresh-token",
		"model_mapping": map[string]any{"gpt-5": "gpt-5"},
		"account_id":    "existing-account-id",
	}, stub.lastUpdateAccountInput.Credentials)
}

func TestOpenAIOAuthCodexPATBoundaryRejectsMalformedOpenAILongContextBillingValueBeforeTokenValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewOpenAIOAuthHandler(nil, newStubAdminService(), nil, nil)
	router := gin.New()
	router.Use(gin.Recovery())
	router.POST("/openai/create-from-codex-pat", handler.CreateAccountFromCodexPAT)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/openai/create-from-codex-pat", bytes.NewBufferString(
		`{"access_token":"token","extra":{"openai_long_context_billing_enabled":1}}`,
	))
	request.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var responseBody struct {
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &responseBody))
	require.Equal(t, "OPENAI_LONG_CONTEXT_BILLING_INVALID", responseBody.Reason)
}
