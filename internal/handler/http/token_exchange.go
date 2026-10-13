// Package httphandler provides the HTTP handler for RFC 8693 Token Exchange.
//
// The Token Exchange endpoint accepts external JWTs from trusted issuers and
// exchanges them for internal RTC access tokens. The endpoint is registered at
// POST /oauth2/token and dispatched based on the grant_type parameter.
package httphandler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
)

// GrantTypeTokenExchange is the OAuth2 grant type URI for RFC 8693 Token Exchange.
const GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"

// TokenExchangeHandler handles POST /oauth2/token for grant_type=token-exchange.
type TokenExchangeHandler struct {
	tokenExchangeUC *usecase.TokenExchangeUsecase
	ipRateLimiter   *middleware.IPRateLimiter
}

// NewTokenExchangeHandler creates a new TokenExchangeHandler.
func NewTokenExchangeHandler(tokenExchangeUC *usecase.TokenExchangeUsecase) *TokenExchangeHandler {
	return &TokenExchangeHandler{
		tokenExchangeUC: tokenExchangeUC,
	}
}

// SetIPRateLimiter sets the IP rate limiter for protecting the endpoint.
func (h *TokenExchangeHandler) SetIPRateLimiter(limiter *middleware.IPRateLimiter) {
	h.ipRateLimiter = limiter
}

// tokenExchangeRequest is the RFC 8693 Token Exchange request body.
type tokenExchangeRequest struct {
	GrantType        string  `json:"grant_type"`
	SubjectToken     string  `json:"subject_token"`
	SubjectTokenType string  `json:"subject_token_type"`
	DeviceID         string  `json:"device_id,omitempty"`
	DeviceName       *string `json:"device_name,omitempty"`
	UserAgent        *string `json:"user_agent,omitempty"`
	// Additional optional parameters (not yet used).
	// ActorToken     string `json:"actor_token,omitempty"`
	// ActorTokenType string `json:"actor_token_type,omitempty"`
	// Scope          string `json:"scope,omitempty"`
	// Audience       string `json:"audience,omitempty"`
	// ResourceType   string `json:"resource,omitempty"`
}

// tokenExchangeResponse is the RFC 8693 Token Exchange response body.
type tokenExchangeResponse struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int64  `json:"expires_in"`
}

// HandleTokenExchange processes the RFC 8693 Token Exchange request.
//
// Rate limiting strategy (defense in depth):
//   - Outer layer: The OAuth2 handler wraps POST /oauth2/token with a general IP
//     rate limiter (api.ip_rate_limit, default 5 req/s/IP). This limits overall
//     abuse across all grant types.
//   - Inner layer: This handler applies a stricter per-IP rate limiter
//     (api.ip_rate_limit.token_exchange, default 3 req/s/IP). This specifically
//     protects Token Exchange because each request triggers external JWT signature
//     verification (CPU-intensive) and the endpoint is a high-value target for
//     brute-force attacks against trusted issuers.
func (h *TokenExchangeHandler) HandleTokenExchange(w http.ResponseWriter, r *http.Request) {
	// Apply dedicated Token Exchange rate limiter if configured.
	// This provides stricter limits than the outer OAuth2 rate limiter.
	if h.ipRateLimiter != nil {
		ip := middleware.ExtractClientIP(r, nil) // no trusted proxies for now
		limiter := h.ipRateLimiter.GetLimiter(ip)
		if !limiter.Allow() {
			logger.Warn(r.Context(), "token_exchange.ip_rate_limit_exceeded",
				zap.String("remote_addr", r.RemoteAddr),
				zap.String("client_ip", ip))
			httputil.WriteError(r.Context(), w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many token exchange requests from your IP")
			return
		}
	}

	req, err := h.parseRequest(w, r)
	if err != nil {
		return // parseRequest already wrote the error response.
	}

	// Validate grant_type per RFC 8693 — must be the exact token exchange URI.
	if req.GrantType != GrantTypeTokenExchange {
		httputil.WriteError(r.Context(), w, http.StatusBadRequest, usecase.ErrInvalidRequest,
			"grant_type must be "+GrantTypeTokenExchange)
		return
	}

	if req.SubjectToken == "" {
		httputil.WriteError(r.Context(), w, http.StatusBadRequest, usecase.ErrInvalidRequest, "subject_token is required")
		return
	}
	if req.SubjectTokenType == "" {
		httputil.WriteError(r.Context(), w, http.StatusBadRequest, usecase.ErrInvalidRequest, "subject_token_type is required")
		return
	}
	// device_id is a required RTC Agent extension (not part of RFC 8693).
	// The server embeds it in the JWT so RTCs can be filtered per-device.
	if req.DeviceID == "" {
		httputil.WriteError(r.Context(), w, http.StatusBadRequest, usecase.ErrInvalidRequest, "device_id is required")
		return
	}

	ctx := r.Context()

	result, err := h.tokenExchangeUC.ExchangeToken(ctx, req.SubjectToken, req.SubjectTokenType, req.DeviceID, req.DeviceName, req.UserAgent)
	if err != nil {
		var teErr *usecase.TokenExchangeError
		if errors.As(err, &teErr) {
			logger.Info(ctx, "token_exchange.exchange_rejected",
				zap.String("error", teErr.Code),
				zap.String("description", teErr.Description),
			)
			httputil.WriteError(r.Context(), w, teErr.HTTPStatus, teErr.Code, teErr.Description)
			return
		}
		logger.Error(ctx, "token_exchange.internal_error", zap.Error(err))
		httputil.WriteError(r.Context(), w, http.StatusInternalServerError, usecase.ErrServerError, "internal server error")
		return
	}

	httputil.WriteJSON(r.Context(), w, http.StatusOK, tokenExchangeResponse{
		AccessToken:     result.AccessToken,
		IssuedTokenType: result.IssuedTokenType,
		TokenType:       result.TokenType,
		ExpiresIn:       result.ExpiresIn,
	})
}

// parseRequest parses the Token Exchange request from JSON or form-encoded body.
func (h *TokenExchangeHandler) parseRequest(w http.ResponseWriter, r *http.Request) (*tokenExchangeRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	var req tokenExchangeRequest

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httputil.WriteError(r.Context(), w, http.StatusBadRequest, usecase.ErrInvalidRequest, "invalid JSON body")
			return nil, err
		}
		return &req, nil
	}

	// Form-encoded.
	if err := r.ParseForm(); err != nil {
		httputil.WriteError(r.Context(), w, http.StatusBadRequest, usecase.ErrInvalidRequest, "cannot parse form")
		return nil, err
	}
	req.GrantType = r.FormValue("grant_type")
	req.SubjectToken = r.FormValue("subject_token")
	req.SubjectTokenType = r.FormValue("subject_token_type")
	req.DeviceID = r.FormValue("device_id")
	if deviceName := r.FormValue("device_name"); deviceName != "" {
		req.DeviceName = httputil.StrPtr(deviceName)
	}
	if userAgent := r.FormValue("user_agent"); userAgent != "" {
		req.UserAgent = httputil.StrPtr(userAgent)
	}

	return &req, nil
}
