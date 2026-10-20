//go:build unit

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSystemManagedUpdatePreparesWithoutLegacySwap(t *testing.T) {
	svc := &systemHandlerUpdateServiceStub{managed: true, managedStatus: &service.ManagedUpdateStatus{Phase: "preparing", OperationID: "sysop-prepared"}}
	router := newSystemHandlerTestRouter(t, svc, newMemoryIdempotencyRepoStub())
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/admin/system/update", nil))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), `"need_restart":false`)
	require.Contains(t, recorder.Body.String(), `"phase":"preparing"`)
	require.Len(t, svc.preparedIDs, 1)
	require.Zero(t, svc.performCall)
	require.Positive(t, time.Until(svc.managedDeadline))
	require.LessOrEqual(t, time.Until(svc.managedDeadline), managedSystemRequestTimeout)
}

func TestSystemManagedRestartRequiresAndActivatesPreparedOperation(t *testing.T) {
	svc := &systemHandlerUpdateServiceStub{managed: true, managedStatus: &service.ManagedUpdateStatus{Phase: "applying", OperationID: "sysop-prepared"}}
	router := newSystemHandlerTestRouter(t, svc, newMemoryIdempotencyRepoStub())
	missing := httptest.NewRecorder()
	router.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/v1/admin/system/restart", nil))
	require.Equal(t, http.StatusBadRequest, missing.Code)
	require.Empty(t, svc.activatedIDs)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/admin/system/restart", strings.NewReader(`{"operation_id":"sysop-prepared"}`)))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, []string{"sysop-prepared"}, svc.activatedIDs)
	require.Contains(t, recorder.Body.String(), `"phase":"applying"`)
	require.Positive(t, time.Until(svc.managedDeadline))
	require.LessOrEqual(t, time.Until(svc.managedDeadline), managedSystemRequestTimeout)
}

func TestSystemManagedEnqueueSurvivesBrowserDisconnectWithBoundedContext(t *testing.T) {
	svc := &systemHandlerUpdateServiceStub{managed: true, managedStatus: &service.ManagedUpdateStatus{Phase: "preparing", OperationID: "sysop-prepared"}}
	router := newSystemHandlerTestRouter(t, svc, newMemoryIdempotencyRepoStub())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/system/update", nil).WithContext(ctx)
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NoError(t, svc.managedContextError)
	require.Positive(t, time.Until(svc.managedDeadline))
	require.LessOrEqual(t, time.Until(svc.managedDeadline), managedSystemRequestTimeout)
}

func TestSystemManagedUpdaterMissingDoesNotRunLegacyUpdate(t *testing.T) {
	svc := &systemHandlerUpdateServiceStub{managed: true, managedErr: service.ErrManagedUpdaterUnavailable}
	router := newSystemHandlerTestRouter(t, svc, newMemoryIdempotencyRepoStub())
	for _, path := range []string{"/api/v1/admin/system/update", "/api/v1/admin/system/update-status"} {
		method := http.MethodPost
		if strings.HasSuffix(path, "status") {
			method = http.MethodGet
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	}
	require.Zero(t, svc.performCall)
}
