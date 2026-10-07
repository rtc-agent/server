// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
)

// SendOTP handles POST /api/auth/otp/send
func (h *AdminAuthHandler) SendOTP(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req SendOTPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "invalid_request", sanitizeBindingError(err))
		return
	}

	ctx := c.Request.Context()
	clientIP := c.ClientIP()

	if h.emailOTPUsecase == nil {
		Error(c, "otp_not_configured", "邮箱验证码功能未配置")
		return
	}

	if err := h.emailOTPUsecase.SendOTP(ctx, req.Email, clientIP); err != nil {
		switch {
		case errors.Is(err, usecase.ErrEmailNotRegistered):
			// SECURITY: Return generic message to prevent email enumeration
			// Don't reveal whether the email exists
			Success(c, gin.H{"message": "如果邮箱已注册，验证码已发送"})
			return
		case errors.Is(err, usecase.ErrOTPRateLimited):
			Error(c, "rate_limited", "请求过于频繁，请稍后再试")
			return
		case errors.Is(err, usecase.ErrOTPLocked):
			Error(c, "locked", "验证码功能已暂时锁定，请稍后再试")
			return
		default:
			logger.Error(ctx, "admin_auth.send_otp_failed", zap.Error(err))
			Error(c, "server_error", "发送验证码失败")
			return
		}
	}

	// Always return success to prevent email enumeration
	Success(c, gin.H{"message": "如果邮箱已注册，验证码已发送"})
}

// LoginWithOTP handles POST /api/auth/login/otp
func (h *AdminAuthHandler) LoginWithOTP(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req OTPLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "invalid_request", sanitizeBindingError(err))
		return
	}

	ctx := c.Request.Context()

	if h.emailOTPUsecase == nil {
		Error(c, "otp_not_configured", "邮箱验证码功能未配置")
		return
	}

	// 1. Verify OTP
	if err := h.emailOTPUsecase.VerifyOTP(ctx, req.Email, req.OTP); err != nil {
		switch {
		case errors.Is(err, usecase.ErrOTPNotFound):
			Error(c, "invalid_otp", "验证码无效或已过期")
			return
		case errors.Is(err, usecase.ErrOTPInvalid):
			Error(c, "invalid_otp", "验证码错误")
			return
		case errors.Is(err, usecase.ErrOTPLocked):
			Error(c, "locked", err.Error())
			return
		default:
			logger.Error(ctx, "admin_auth.verify_otp_failed", zap.Error(err))
			Error(c, "server_error", "验证失败")
			return
		}
	}

	// 2. OTP is valid, perform login (get user and issue tokens)
	result, err := h.adminAuthUsecase.LoginWithOTP(ctx, req.Email)
	if err != nil {
		if errors.Is(err, usecase.ErrAdminUserNotFound) {
			Error(c, "invalid_credentials", "邮箱或验证码错误")
			return
		}
		logger.Error(ctx, "admin_auth.otp_login_failed", zap.Error(err))
		Error(c, "server_error", "服务器内部错误")
		return
	}

	// 3. Set cookie and return response (same as password login)
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
