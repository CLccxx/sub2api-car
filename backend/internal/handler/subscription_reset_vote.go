package handler

import (
	"context"
	"net/http"
	"strconv"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type resetVoteService interface {
	Get(context.Context, int64, int64) (*service.ResetVoteView, error)
	GetRequest(context.Context, int64, int64, string) (*service.ResetVoteView, error)
	Create(context.Context, int64, int64, string) (*service.ResetVoteView, error)
	Agree(context.Context, int64, int64, int64) (*service.ResetVoteView, error)
}

// GetResetVote returns the reset vote visible to the authenticated subscriber.
func (h *SubscriptionHandler) GetResetVote(c *gin.Context) {
	userID, subscriptionID, ok := resetVoteScope(c)
	if !ok {
		return
	}
	if key, exists := c.GetQuery("request_id"); exists {
		if err := uuid.Validate(key); err != nil {
			response.BadRequest(c, "Invalid request_id: must be a UUID")
			return
		}
		view, err := h.resetVote.GetRequest(c.Request.Context(), userID, subscriptionID, key)
		resetVoteResponse(c, view, err)
		return
	}
	view, err := h.resetVote.Get(c.Request.Context(), userID, subscriptionID)
	resetVoteResponse(c, view, err)
}

// CreateResetVote creates a proposal using the client's idempotency request ID.
func (h *SubscriptionHandler) CreateResetVote(c *gin.Context) {
	userID, subscriptionID, ok := resetVoteScope(c)
	if !ok {
		return
	}
	var request struct {
		RequestID string `json:"request_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "Invalid request_id: must be a UUID")
		return
	}
	if err := uuid.Validate(request.RequestID); err != nil {
		response.BadRequest(c, "Invalid request_id: must be a UUID")
		return
	}
	view, err := h.resetVote.Create(c.Request.Context(), userID, subscriptionID, request.RequestID)
	resetVoteResponse(c, view, err)
}

// AgreeResetVote preserves the subscription scope for proposal authorization.
func (h *SubscriptionHandler) AgreeResetVote(c *gin.Context) {
	userID, subscriptionID, ok := resetVoteScope(c)
	if !ok {
		return
	}
	proposalID, err := strconv.ParseInt(c.Param("proposal"), 10, 64)
	if err != nil || proposalID <= 0 {
		response.BadRequest(c, "Invalid proposal ID")
		return
	}
	view, err := h.resetVote.Agree(c.Request.Context(), userID, subscriptionID, proposalID)
	resetVoteResponse(c, view, err)
}

func resetVoteScope(c *gin.Context) (userID, subscriptionID int64, ok bool) {
	c.Header("Cache-Control", "no-store")
	subject, authenticated := middleware2.GetAuthSubjectFromContext(c)
	if !authenticated || subject.UserID <= 0 {
		response.Unauthorized(c, "User not found in context")
		return 0, 0, false
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid subscription ID")
		return 0, 0, false
	}
	return subject.UserID, id, true
}

func resetVoteResponse(c *gin.Context, view *service.ResetVoteView, err error) {
	if err != nil {
		if status := infraerrors.Code(err); status >= http.StatusInternalServerError {
			response.Error(c, status, infraerrors.UnknownMessage)
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, view)
}
