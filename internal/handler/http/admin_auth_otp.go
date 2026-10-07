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
		Error(c, "otp_not_configured", "Email verification is not configured")
		return
	}

	if err := h.emailOTPUsecase.SendOTP(ctx, req.Email, clientIP); err != nil {
		switch {
		case errors.Is(err, usecase.ErrEmailNotRegistered):
			// SECURITY: Return generic message to prevent email enumeration.
			// Do not reveal whether the email exists.
			Success(c, gin.H{"message": "If the email is registered, a verification code has been sent"})
			return
		case errors.Is(err, usecase.ErrOTPRateLimited):
			Error(c, "rate_limited", "Too many requests, please try again later")
			return
		case errors.Is(err, usecase.ErrOTPLocked):
			Error(c, "locked", "Verification is temporarily locked, please try again later")
			return
		default:
			logger.Error(ctx, "admin_auth.send_otp_failed", zap.Error(err))
			Error(c, "server_error", "Failed to send verification code")
			return
		}
	}

	// Always return success to prevent email enumeration.
	Success(c, gin.H{"message": "If the email is registered, a verification code has been sent"})
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
		Error(c, "otp_not_configured", "Email verification is not configured")
		return
	}

	// 1. Verify OTP
	if err := h.emailOTPUsecase.VerifyOTP(ctx, req.Email, req.OTP); err != nil {
		switch {
		case errors.Is(err, usecase.ErrOTPNotFound):
			Error(c, "invalid_otp", "Invalid or expired verification code")
			return
		case errors.Is(err, usecase.ErrOTPInvalid):
			Error(c, "invalid_otp", "Invalid verification code")
			return
		case errors.Is(err, usecase.ErrOTPLocked):
			Error(c, "locked", err.Error())
			return
		default:
			logger.Error(ctx, "admin_auth.verify_otp_failed", zap.Error(err))
			Error(c, "server_error", "Verification failed")
			return
		}
	}

	// 2. OTP is valid, perform login (get user and issue tokens)
	result, err := h.adminAuthUsecase.LoginWithOTP(ctx, req.Email)
	if err != nil {
		if errors.Is(err, usecase.ErrAdminUserNotFound) {
			Error(c, "invalid_credentials", "Invalid email or verification code")
			return
		}
		logger.Error(ctx, "admin_auth.otp_login_failed", zap.Error(err))
		Error(c, "server_error", "Internal server error")
		return
	}

	// 3. Set cookie and return response (same as password login).
	// SameSite=Lax balances CSRF protection with iframe usability.
	// Secure flag is enabled in production HTTPS deployments.
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("access_token", result.AccessToken, int(result.ExpiresIn), "/", "", h.cookieSecure, true)

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
