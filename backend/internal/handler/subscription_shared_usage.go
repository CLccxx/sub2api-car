package handler

import (
	"context"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type sharedSubscriptionUsageReader interface {
	Get(context.Context, int64, int64) (*service.SharedSubscriptionUsage, error)
}

// GetSharedUsage exposes only the usage of the authenticated subscriber's group.
func (h *SubscriptionHandler) GetSharedUsage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not found in context")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid subscription ID")
		return
	}
	usage, err := h.sharedUsage.Get(c.Request.Context(), subject.UserID, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, usage)
}
