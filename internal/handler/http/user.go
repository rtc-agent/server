// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/rtc-agent/server/internal/usecase"
)

// UserHandler handles user-related HTTP requests.
type UserHandler struct {
	userUsecase *usecase.UserUsecase
}

// NewUserHandler creates a new UserHandler.
func NewUserHandler(userUsecase *usecase.UserUsecase) *UserHandler {
	return &UserHandler{
		userUsecase: userUsecase,
	}
}

// RegisterRoutes registers user routes under /api/users.
func (h *UserHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/users", h.ListUsers)
}

// ListUsers returns a paginated list of users.
// Query parameters: page (default 1), page_size (default 20, max 100)
func (h *UserHandler) ListUsers(c *gin.Context) {
	ctx := c.Request.Context()

	page := 1
	if p := c.Query("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}

	pageSize := 20
	if ps := c.Query("page_size"); ps != "" {
		if v, err := strconv.Atoi(ps); err == nil && v > 0 && v <= 100 {
			pageSize = v
		}
	}

	users, total, err := h.userUsecase.ListUsersPaginated(ctx, page, pageSize)
	if err != nil {
		Error(c, "server_error", "获取用户列表失败")
		return
	}

	// Build response with user details and their roles
	result := make([]gin.H, 0, len(users))
	for _, user := range users {
		result = append(result, gin.H{
			"id":         user.ID,
			"email":      user.Email,
			"name":       user.Name,
			"avatar_url": user.AvatarURL,
			"created_at": user.CreatedAt,
			"updated_at": user.UpdatedAt,
		})
	}

	Success(c, gin.H{
		"items":     result,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}
