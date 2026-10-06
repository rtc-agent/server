// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
)

// AdminAuthHandler handles admin authentication endpoints.
type AdminAuthHandler struct {
	adminAuthUsecase        *usecase.AdminAuthUsecase
	jwtSigner               *auth.AdminJWTSigner
	db                      *gorm.DB
	roleRepo                roleLookup
	adminUserRoleRepo       adminUserRoleLister
	enforcer                *auth.CasbinEnforcer
	permissionSystemEnabled bool
}

// NewAdminAuthHandler creates a new AdminAuthHandler.
func NewAdminAuthHandler(
	adminAuthUsecase *usecase.AdminAuthUsecase,
	jwtSigner *auth.AdminJWTSigner,
	db *gorm.DB,
) *AdminAuthHandler {
	return &AdminAuthHandler{
		adminAuthUsecase: adminAuthUsecase,
		jwtSigner:        jwtSigner,
		db:               db,
	}
}

// SetPermissionDeps injects permission system dependencies into the handler.
// This must be called before RegisterRoutes if the permission system is enabled.
func (h *AdminAuthHandler) SetPermissionDeps(
	roleRepo roleLookup,
	adminUserRoleRepo adminUserRoleLister,
	enforcer *auth.CasbinEnforcer,
	permissionSystemEnabled bool,
) {
	h.roleRepo = roleRepo
	h.adminUserRoleRepo = adminUserRoleRepo
	h.enforcer = enforcer
	h.permissionSystemEnabled = permissionSystemEnabled
}

// RegisterRoutes registers admin auth routes to the Gin router.
func (h *AdminAuthHandler) RegisterRoutes(r *gin.Engine) {
	// Public routes (no JWT required)
	r.POST("/api/auth/login", h.Login)
	// Refresh is public: the client needs to exchange a refresh_token for a new
	// access_token even when the original access_token has expired.
	r.POST("/api/auth/refresh", h.RefreshToken)
	r.GET("/.well-known/jwks.json", h.JWKS)
	r.GET("/health", h.Health)

	// Protected routes (require JWT authentication)
	protected := r.Group("/api/auth")
	protected.Use(h.JWTAuthMiddleware())
	{
		protected.GET("/me", h.GetCurrentUser)
		protected.POST("/logout", h.Logout)
	}
}

// maxAdminRequestBodySize is the maximum allowed size for admin auth request bodies.
// Prevents malicious clients from sending oversized payloads that could exhaust memory.
const maxAdminRequestBodySize = 1 << 20 // 1MB

// sanitizeBindingError converts a Gin binding error into a safe, user-facing message.
// Internal struct field names (e.g. "LoginRequest.Password") are intentionally omitted
// to avoid leaking API implementation details to potential attackers.
func sanitizeBindingError(err error) string {
	msg := err.Error()
	// JSON syntax errors are safe to surface (they describe the malformed input, not the schema).
	if strings.Contains(msg, "json:") || strings.Contains(msg, "unmarshal") {
		return "invalid JSON format"
	}
	// Validation errors (binding:"required,min=6,...") would expose field names; use a generic message.
	return "request validation failed"
}

// Login handles POST /api/auth/login
func (h *AdminAuthHandler) Login(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "invalid_request", sanitizeBindingError(err))
		return
	}

	ctx := c.Request.Context()
	clientIP := c.ClientIP()
	result, err := h.adminAuthUsecase.Login(ctx, req.Email, req.Password, clientIP)
	if err != nil {
		if errors.Is(err, usecase.ErrInvalidCredentials) {
			Error(c, "invalid_credentials", "邮箱或密码错误")
			return
		}
		if errors.Is(err, usecase.ErrLoginLocked) {
			Error(c, "login_locked", err.Error())
			return
		}
		logger.Error(ctx, "admin_auth.login_failed", zap.Error(err))
		Error(c, "server_error", "服务器内部错误")
		return
	}

	// 登录成功返回统一格式的响应
	// 同时设置 access_token cookie，供 iframe 嵌入场景使用（如 Grafana）
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("access_token", result.AccessToken, int(result.ExpiresIn), "/", "", false, true)

	Success(c, LoginResponse{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    int(result.ExpiresIn),
		TokenType:    "Bearer",
		User: &UserResponse{
			ID:        result.User.ID.String(),
			Email:     result.User.Email,
			Name:      result.User.Name,
			AvatarURL: result.User.AvatarURL,
		},
	})
}

// GetCurrentUser handles GET /api/auth/me
func (h *AdminAuthHandler) GetCurrentUser(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		Error(c, "unauthorized", "管理员未认证")
		return
	}

	// Convert userID from string to uuid.UUID
	userIDStr, ok := userID.(string)
	if !ok {
		Error(c, "unauthorized", "无效的管理员 ID")
		return
	}

	userUUID, err := uuid.Parse(userIDStr)
	if err != nil {
		Error(c, "unauthorized", "管理员 ID 格式错误")
		return
	}

	ctx := c.Request.Context()
	user, err := h.adminAuthUsecase.GetCurrentUser(ctx, userUUID)
	if err != nil {
		if errors.Is(err, usecase.ErrAdminUserNotFound) {
			Error(c, "admin_user_not_found", "管理员不存在")
			return
		}
		logger.Error(ctx, "admin_auth.get_current_admin_user_failed", zap.Error(err))
		Error(c, "server_error", "服务器内部错误")
		return
	}

	// If permission system is enabled and deps are injected, include roles and permissions
	if h.permissionSystemEnabled && h.roleRepo != nil && h.enforcer != nil {
		resp, err := GetCurrentUserWithRoles(ctx, user, h.adminUserRoleRepo, h.roleRepo, h.enforcer)
		if err != nil {
			logger.Error(ctx, "admin_auth.get_current_user_roles_failed", zap.Error(err))
			Error(c, "server_error", "服务器内部错误")
			return
		}
		Success(c, resp)
		return
	}

	// Permission system disabled: return admin user with default admin role
	// This ensures frontend knows the admin user has full access
	Success(c, UserResponse{
		ID:        user.ID.String(),
		Email:     user.Email,
		Name:      user.Name,
		AvatarURL: user.AvatarURL,
		Roles: []RoleInfo{
			{
				Name:        "admin",
				DisplayName: "管理员",
			},
		},
	})
}

// RefreshToken handles POST /api/auth/refresh
func (h *AdminAuthHandler) RefreshToken(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "invalid_request", sanitizeBindingError(err))
		return
	}

	ctx := c.Request.Context()
	result, err := h.adminAuthUsecase.RefreshToken(ctx, req.RefreshToken)
	if err != nil {
		switch {
		case errors.Is(err, usecase.ErrInvalidRefreshToken):
			Error(c, "invalid_grant", "刷新令牌无效")
		case errors.Is(err, usecase.ErrRefreshTokenRevoked):
			Error(c, "invalid_grant", "刷新令牌已被撤销")
		case errors.Is(err, usecase.ErrRefreshTokenExpired):
			Error(c, "invalid_grant", "刷新令牌已过期")
		default:
			logger.Error(ctx, "admin_auth.refresh_token_failed", zap.Error(err))
			Error(c, "server_error", "服务器内部错误")
		}
		return
	}

	// Update access_token cookie with new token
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("access_token", result.AccessToken, int(result.ExpiresIn), "/", "", false, true)

	Success(c, RefreshResponse{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    int(result.ExpiresIn),
		TokenType:    "Bearer",
	})
}

// Logout handles POST /api/auth/logout
func (h *AdminAuthHandler) Logout(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req LogoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "invalid_request", sanitizeBindingError(err))
		return
	}

	ctx := c.Request.Context()
	if err := h.adminAuthUsecase.Logout(ctx, req.RefreshToken); err != nil {
		logger.Error(ctx, "admin_auth.logout_failed", zap.Error(err))
		Error(c, "server_error", "服务器内部错误")
		return
	}

	// Clear access_token cookie
	c.SetCookie("access_token", "", -1, "/", "", false, true)

	Success(c, gin.H{"status": "ok"})
}

// JWKS handles GET /.well-known/jwks.json
func (h *AdminAuthHandler) JWKS(c *gin.Context) {
	jwks, err := h.jwtSigner.GetJWKS()
	if err != nil {
		logger.Error(c.Request.Context(), "admin_auth.jwks_generation_failed", zap.Error(err))
		Error(c, "server_error", "生成 JWKS 失败")
		return
	}

	// Convert JWK set to JSON
	jwksJSON, err := json.Marshal(jwks)
	if err != nil {
		logger.Error(c.Request.Context(), "admin_auth.jwks_serialization_failed", zap.Error(err))
		Error(c, "server_error", "序列化 JWKS 失败")
		return
	}

	c.Data(http.StatusOK, "application/json", jwksJSON)
}

// Health handles GET /health
// Verifies database connectivity before reporting healthy status.
func (h *AdminAuthHandler) Health(c *gin.Context) {
	// Check database connectivity via a lightweight query.
	sqlDB, err := h.db.DB()
	if err != nil {
		logger.Error(c.Request.Context(), "admin_auth.health_db_unavailable", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, ResponseStructure{
			Success:      false,
			ErrorCode:    "database_unavailable",
			ErrorMessage: "数据库不可用",
		})
		return
	}
	if err := sqlDB.Ping(); err != nil {
		logger.Error(c.Request.Context(), "admin_auth.health_ping_failed", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, ResponseStructure{
			Success:      false,
			ErrorCode:    "database_unavailable",
			ErrorMessage: "数据库不可用",
		})
		return
	}

	Success(c, HealthResponse{
		Status:    "ok",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// JWTAuthMiddleware is a Gin middleware for JWT authentication.
// Reads token from Authorization header first; falls back to "access_token" cookie.
func (h *AdminAuthHandler) JWTAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		var tokenString string

		// 1. Try Authorization header
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				tokenString = parts[1]
			}
		}

		// 2. Fallback to cookie
		if tokenString == "" {
			if cookie, err := c.Cookie("access_token"); err == nil && cookie != "" {
				tokenString = cookie
			}
		}

		if tokenString == "" {
			Error(c, "unauthorized", "缺少认证信息")
			c.Abort()
			return
		}

		claims, err := h.jwtSigner.ParseAccessToken(tokenString)
		if err != nil {
			// SECURITY: log at Info level to detect brute-force patterns without
			// flooding logs with malformed token attempts. Do NOT log the token value.
			logger.Info(c.Request.Context(), "admin_auth.jwt_rejected",
				zap.String("error", err.Error()))
			Error(c, "unauthorized", "令牌无效或已过期")
			c.Abort()
			return
		}

		// Store user info in context for handlers
		c.Set("user_id", claims.UserID.String())
		c.Set("email", claims.Email)
		c.Set("name", claims.Name)

		c.Next()
	}
}
