package handler

import (
	"context"
	"net/http/httptest"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestSubmitUsageRecordTaskCopiesRequestContext(t *testing.T) {
	parent := context.WithValue(context.Background(), ctxkey.ClientRequestID, "client-request-123")
	parent = context.WithValue(parent, ctxkey.RequestID, "request-456")

	var gotClientRequestID string
	var gotRequestID string
	h := &GatewayHandler{}
	h.submitUsageRecordTask(parent, func(ctx context.Context) {
		gotClientRequestID, _ = ctx.Value(ctxkey.ClientRequestID).(string)
		gotRequestID, _ = ctx.Value(ctxkey.RequestID).(string)
	})

	require.Equal(t, "client-request-123", gotClientRequestID)
	require.Equal(t, "request-456", gotRequestID)
}

func TestOpenAISubmitUsageRecordTaskCopiesRequestContext(t *testing.T) {
	parent := context.WithValue(context.Background(), ctxkey.ClientRequestID, "openai-client-request-123")
	parent = context.WithValue(parent, ctxkey.RequestID, "openai-request-456")

	var gotClientRequestID string
	var gotRequestID string
	h := &OpenAIGatewayHandler{}
	h.submitUsageRecordTask(parent, func(ctx context.Context) {
		gotClientRequestID, _ = ctx.Value(ctxkey.ClientRequestID).(string)
		gotRequestID, _ = ctx.Value(ctxkey.RequestID).(string)
	})

	require.Equal(t, "openai-client-request-123", gotClientRequestID)
	require.Equal(t, "openai-request-456", gotRequestID)
}

func TestDeferredUsageTaskCapturesObservationBeforeExecution(t *testing.T) {
	started := time.Now()
	var captured time.Time
	task, _ := wrapUsageRecordTaskContext(context.Background(), func(ctx context.Context) { captured = service.UsageObservationTime(ctx) })
	wrapped := time.Now()
	task(context.Background())
	require.False(t, captured.Before(started))
	require.False(t, captured.After(wrapped), "worker execution must retain enqueue-time observation")
}

func TestDeferredUsageTaskCapturesRequestValuesBeforeGinReuse(t *testing.T) {
	c, engine := gin.CreateTestContext(httptest.NewRecorder())
	engine.ContextWithFallback = true
	original := context.WithValue(context.Background(), ctxkey.ClientRequestID, "original-client")
	original = context.WithValue(original, ctxkey.RequestID, "original-request")
	c.Request = httptest.NewRequest("GET", "/original", nil).WithContext(original)
	var gotClient, gotRequest string
	var gotObservation time.Time
	started := time.Now()
	task, _ := wrapUsageRecordTaskContext(c, func(ctx context.Context) {
		gotClient, _ = ctx.Value(ctxkey.ClientRequestID).(string)
		gotRequest, _ = ctx.Value(ctxkey.RequestID).(string)
		gotObservation = service.UsageObservationTime(ctx)
	})
	wrapped := time.Now()
	next := context.WithValue(context.Background(), ctxkey.ClientRequestID, "next-client")
	next = context.WithValue(next, ctxkey.RequestID, "next-request")
	c.Request = httptest.NewRequest("GET", "/next", nil).WithContext(next)
	task(context.Background())
	require.Equal(t, "original-client", gotClient)
	require.Equal(t, "original-request", gotRequest)
	require.False(t, gotObservation.Before(started))
	require.False(t, gotObservation.After(wrapped))
}
