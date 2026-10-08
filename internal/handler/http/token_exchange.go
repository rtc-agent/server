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
	"github.com/rtc-agent/server/internal/model"
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
func (h *TokenExchangeHandler) HandleTokenExchange(w http.ResponseWriter, r *http.Request) {
	req, err := h.parseRequest(w, r)
	if err != nil {
		return // parseRequest already wrote the error response.
	}

	// Validate grant_type per RFC 8693 — must be the exact token exchange URI.
	if req.GrantType != GrantTypeTokenExchange {
		httputil.WriteError(w, http.StatusBadRequest, usecase.ErrInvalidRequest,
			"grant_type must be "+GrantTypeTokenExchange)
		return
	}

	if req.SubjectToken == "" {
		httputil.WriteError(w, http.StatusBadRequest, usecase.ErrInvalidRequest, "subject_token is required")
		return
	}
	if req.SubjectTokenType == "" {
		httputil.WriteError(w, http.StatusBadRequest, usecase.ErrInvalidRequest, "subject_token_type is required")
		return
	}
	// device_id is a required RTC Agent extension (not part of RFC 8693).
	// The server embeds it in the JWT so RTCs can be filtered per-device.
	if req.DeviceID == "" {
		httputil.WriteError(w, http.StatusBadRequest, usecase.ErrInvalidRequest, "device_id is required")
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
			httputil.WriteError(w, teErr.HTTPStatus, teErr.Code, teErr.Description)
			return
		}
		logger.Error(ctx, "token_exchange.internal_error", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, usecase.ErrServerError, "internal server error")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, tokenExchangeResponse{
		AccessToken:     result.AccessToken,
		IssuedTokenType: result.IssuedTokenType,
		TokenType:       result.TokenType,
		ExpiresIn:       result.ExpiresIn,
	})
}

// parseRequest parses the Token Exchange request from JSON or form-encoded body.
func (h *TokenExchangeHandler) parseRequest(w http.ResponseWriter, r *http.Request) (*tokenExchangeRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminRequestBodySize)
	var req tokenExchangeRequest

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, usecase.ErrInvalidRequest, "invalid JSON body")
			return nil, err
		}
		return &req, nil
	}

	// Form-encoded.
	if err := r.ParseForm(); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, usecase.ErrInvalidRequest, "cannot parse form")
		return nil, err
	}
	req.GrantType = r.FormValue("grant_type")
	req.SubjectToken = r.FormValue("subject_token")
	req.SubjectTokenType = r.FormValue("subject_token_type")
	req.DeviceID = r.FormValue("device_id")
	if deviceName := r.FormValue("device_name"); deviceName != "" {
		req.DeviceName = model.StrPtr(deviceName)
	}
	if userAgent := r.FormValue("user_agent"); userAgent != "" {
		req.UserAgent = model.StrPtr(userAgent)
	}

	return &req, nil
}
