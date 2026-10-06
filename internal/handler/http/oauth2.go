package httphandler

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/oauth"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
)

// TokenSigner is the JWT signing interface.
type TokenSigner interface {
	SignAccessToken(userID uuid.UUID, deviceID string) (token string, expiresAt time.Time, err error)
	AccessTTL() time.Duration
}

// StateStore is the OAuth2 state storage interface (CSRF protection).
type StateStore interface {
	Set(ctx context.Context, state string, value string, ttl time.Duration) error
	GetDel(ctx context.Context, state string) (string, error)
}

// ProviderClient is the OAuth2 provider client interface.
type ProviderClient interface {
	GetAuthorizationURL(provider string, state string, redirectURI string, codeChallenge string, codeChallengeMethod string) (string, error)
	ExchangeCode(ctx context.Context, provider string, code string, redirectURI string, codeVerifier string) (*oauth.ProviderUserInfo, error)
	GetProviders() []string
}

// OAuth2Handler handles OAuth2 endpoints.
type OAuth2Handler struct {
	authUsecase    *usecase.AuthUsecase
	signer         TokenSigner
	stateStore     StateStore
	providerClient ProviderClient
	authConfig     config.AuthConfig
	ipRateLimiter  *middleware.IPRateLimiter // Optional: for protecting public endpoints
	tokenExchange  *TokenExchangeHandler     // RFC 8693 Token Exchange handler (nil if not configured)
}

// NewOAuth2Handler creates a new OAuth2 endpoint handler.
func NewOAuth2Handler(authUsecase *usecase.AuthUsecase, signer TokenSigner, stateStore StateStore, providerClient ProviderClient, authCfg config.AuthConfig) *OAuth2Handler {
	return &OAuth2Handler{
		authUsecase:    authUsecase,
		signer:         signer,
		stateStore:     stateStore,
		providerClient: providerClient,
		authConfig:     authCfg,
	}
}

// SetIPRateLimiter sets the IP rate limiter for protecting public endpoints.
func (h *OAuth2Handler) SetIPRateLimiter(limiter *middleware.IPRateLimiter) {
	h.ipRateLimiter = limiter
}

// SetTokenExchangeHandler sets the RFC 8693 Token Exchange handler.
// When set, POST /oauth2/token dispatches to token exchange when grant_type matches.
func (h *OAuth2Handler) SetTokenExchangeHandler(te *TokenExchangeHandler) {
	h.tokenExchange = te
}

// RegisterRoutes registers OAuth2 routes to the HTTP ServeMux.
// Public endpoints (/oauth2/token, /oauth2/refresh, /oauth2/authorize) are
// protected by IP rate limiting if configured.
func (h *OAuth2Handler) RegisterRoutes(mux *http.ServeMux) {
	// Public endpoints - apply IP rate limiting if configured
	authorizeHandler := http.HandlerFunc(h.handleAuthorize)
	tokenHandler := http.HandlerFunc(h.handleToken)
	refreshHandler := http.HandlerFunc(h.handleRefresh)

	if h.ipRateLimiter != nil {
		authorizeHandler = h.ipRateLimiter.WrapHandlerFunc(authorizeHandler)
		tokenHandler = h.ipRateLimiter.WrapHandlerFunc(tokenHandler)
		refreshHandler = h.ipRateLimiter.WrapHandlerFunc(refreshHandler)
	}

	mux.Handle("GET /oauth2/authorize", authorizeHandler)
	mux.Handle("POST /oauth2/token", tokenHandler)
	mux.Handle("POST /oauth2/refresh", refreshHandler)
	// /oauth2/providers is a read-only endpoint, no rate limiting needed
	mux.HandleFunc("GET /oauth2/providers", h.handleProviders)
}

// isAllowedRedirectURI checks if a redirect URI is in the allowed list.
// An empty allowed list means all URIs are permitted (development mode).
func (h *OAuth2Handler) isAllowedRedirectURI(uri string) bool {
	if len(h.authConfig.AllowedRedirectURIs) == 0 {
		return true // no restriction (development mode)
	}
	for _, allowed := range h.authConfig.AllowedRedirectURIs {
		if uri == allowed {
			return true
		}
	}
	return false
}

// handleProviders handles GET /oauth2/providers.
// Returns the list of currently enabled providers.
func (h *OAuth2Handler) handleProviders(w http.ResponseWriter, r *http.Request) {
	providers := h.providerClient.GetProviders()
	httputil.WriteJSON(w, http.StatusOK, map[string][]string{
		"providers": providers,
	})
}

// handleAuthorize handles GET /oauth2/authorize.
// Generates state and returns the provider authorization page redirect URL.
func (h *OAuth2Handler) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "provider parameter is required")
		return
	}
	redirectURI := r.URL.Query().Get("redirect_uri")

	// PKCE parameters (RFC 7636)
	codeChallenge := r.URL.Query().Get("code_challenge")
	codeChallengeMethod := r.URL.Query().Get("code_challenge_method")
	if codeChallengeMethod == "" {
		codeChallengeMethod = "S256" // Default to S256
	}

	// Validate redirect_uri to prevent Open Redirect attacks
	if redirectURI != "" {
		parsed, err := url.Parse(redirectURI)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri must be a valid absolute URL")
			return
		}
		if !h.isAllowedRedirectURI(redirectURI) {
			httputil.WriteError(w, http.StatusBadRequest, "disallowed_redirect_uri", "redirect_uri is not in the allowed list")
			return
		}
	}

	// Generate random state.
	state, err := generateState()
	if err != nil {
		logger.Error(r.Context(), "failed to generate state", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "failed to generate state")
		return
	}

	// Build provider authorization URL (also validates provider existence).
	redirectURL, err := h.providerClient.GetAuthorizationURL(provider, state, redirectURI, codeChallenge, codeChallengeMethod)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("unsupported provider: %s", provider))
		return
	}

	// Store in StateStore (key=state, value=JSON{provider, redirect_uri, code_challenge}, TTL=10min).
	// The redirect_uri is bound to the state to prevent redirect_uri substitution attacks
	// per RFC 6749 Section 10.6. The code_challenge is bound for PKCE validation per RFC 7636.
	ctx := r.Context()
	stateData := map[string]string{
		"provider":              provider,
		"redirect_uri":          redirectURI,
		"code_challenge":        codeChallenge,
		"code_challenge_method": codeChallengeMethod,
	}
	stateJSON, err := json.Marshal(stateData)
	if err != nil {
		logger.Error(ctx, "failed to marshal state data", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "failed to store state")
		return
	}
	if err := h.stateStore.Set(ctx, state, string(stateJSON), h.authConfig.OAuth2StateTTL); err != nil {
		logger.Error(ctx, "failed to store state", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "failed to store state")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, protocol.OAuth2AuthorizeResponse{
		RedirectUrl: redirectURL,
		State:       state,
	})
}

// handleToken handles POST /oauth2/token.
// Supports both JSON and application/x-www-form-urlencoded Content-Types.
// Dispatches based on grant_type:
//   - "urn:ietf:params:oauth:grant-type:token-exchange" -> RFC 8693 Token Exchange
//   - Otherwise -> Authorization Code Grant (existing flow)
func (h *OAuth2Handler) handleToken(w http.ResponseWriter, r *http.Request) {
	// Peek at grant_type to dispatch.
	grantType := PeekGrantType(r)

	// RFC 8693 Token Exchange dispatch.
	if grantType == GrantTypeTokenExchange {
		if h.tokenExchange != nil {
			h.tokenExchange.HandleTokenExchange(w, r)
			return
		}
		httputil.WriteError(w, http.StatusBadRequest, "unsupported_grant_type",
			"token exchange is not configured")
		return
	}

	// Default: Authorization Code Grant.
	req, err := parseTokenExchangeRequest(w, r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "failed to parse request")
		return
	}

	if req.Code == "" || req.State == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "code and state are required")
		return
	}

	h.handleAuthorizationCodeGrant(w, r, req)
}

// handleAuthorizationCodeGrant handles the authorization_code grant flow.
func (h *OAuth2Handler) handleAuthorizationCodeGrant(w http.ResponseWriter, r *http.Request, req *protocol.OAuth2TokenExchangeRequest) {
	ctx := r.Context()

	// 1. Validate state from StateStore (retrieve associated provider and redirect_uri), then delete.
	stateValue, err := h.stateStore.GetDel(ctx, req.State)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "state is invalid or expired")
		return
	}

	// Parse state data (JSON format with provider, redirect_uri, and PKCE challenge).
	var stateData map[string]string
	if err := json.Unmarshal([]byte(stateValue), &stateData); err != nil {
		// Backward compatibility: if not JSON, treat as plain provider string.
		stateData = map[string]string{"provider": stateValue}
	}
	provider := stateData["provider"]
	storedRedirectURI := stateData["redirect_uri"]
	storedCodeChallenge := stateData["code_challenge"]
	storedCodeChallengeMethod := stateData["code_challenge_method"]
	if storedCodeChallengeMethod == "" {
		storedCodeChallengeMethod = "S256"
	}

	// Validate redirect_uri matches the one from authorization request (RFC 6749 Section 10.6).
	// This prevents authorization code injection attacks.
	if storedRedirectURI != "" && req.RedirectUri != storedRedirectURI {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match authorization request")
		return
	}

	// Validate PKCE code_verifier if code_challenge was provided during authorization (RFC 7636).
	if storedCodeChallenge != "" {
		if req.CodeVerifier == nil || *req.CodeVerifier == "" {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_grant", "code_verifier is required")
			return
		}
		if !verifyCodeChallenge(storedCodeChallenge, storedCodeChallengeMethod, *req.CodeVerifier) {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match code_challenge")
			return
		}
	}

	// 2. Exchange authorization code for user info.
	codeVerifier := ""
	if req.CodeVerifier != nil {
		codeVerifier = *req.CodeVerifier
	}
	userInfo, err := h.providerClient.ExchangeCode(ctx, provider, req.Code, req.RedirectUri, codeVerifier)
	if err != nil {
		logger.Error(ctx, "ExchangeCode failed", zap.String("provider", provider), zap.Error(err))
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "authorization code exchange failed")
		return
	}

	// 3. Find or create OAuth2User via Usecase.
	userResult, err := h.authUsecase.FindOrCreateUser(ctx, provider, userInfo)
	if err != nil {
		logger.Error(ctx, "failed to find or create user", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "authentication failed")
		return
	}

	// 3.5. Check if user is banned.
	if userResult.BannedAt != nil {
		logger.Warn(ctx, "banned user attempted to login",
			zap.String("provider", provider),
			zap.String("user_id", userResult.UserID.String()),
			zap.Time("banned_at", *userResult.BannedAt))
		httputil.WriteError(w, http.StatusForbidden, "access_denied", "account has been banned")
		return
	}

	// 4. Find or update Device via Usecase.
	if req.DeviceId != "" {
		if err := h.authUsecase.UpsertDevice(ctx, &usecase.UpsertDeviceInput{
			UserID:    userResult.UserID,
			DeviceID:  req.DeviceId,
			Name:      model.DerefStr(req.DeviceName),
			UserAgent: model.DerefStr(req.UserAgent),
		}); err != nil {
			logger.Warn(ctx, "failed to update device info", zap.Error(err))
		}
	}

	// 5. Issue token pair via Usecase.
	resp, err := h.authUsecase.IssueTokenPair(ctx, userResult.UserID, req.DeviceId)
	if err != nil {
		logger.Error(ctx, "failed to issue token", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "failed to issue token")
		return
	}

	logger.Info(ctx, "authorization_code authentication succeeded", zap.String("provider", provider), zap.String("user_id", userResult.UserID.String()))
	httputil.WriteJSON(w, http.StatusOK, protocol.OAuth2TokenExchangeResponse{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresIn:    resp.ExpiresIn,
		UserId:       resp.UserID.String(),
	})
}

// handleRefresh handles POST /oauth2/refresh.
// Implements refresh token rotation: the old refresh token is revoked and a new
// token pair is issued. This limits the damage of a leaked refresh token.
func (h *OAuth2Handler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	req, err := parseRefreshRequest(w, r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "failed to parse request body")
		return
	}

	if req.RefreshToken == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}

	ctx := r.Context()

	// 1. Validate refresh token (lookup + check + revoke) via Usecase.
	rtInfo, err := h.authUsecase.ValidateRefreshToken(ctx, req.RefreshToken)
	if err != nil {
		switch err {
		case usecase.ErrInvalidRefreshToken:
			httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "refresh_token is invalid")
		case usecase.ErrRefreshTokenRevoked:
			httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "refresh_token has been revoked")
		case usecase.ErrRefreshTokenExpired:
			httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "refresh_token has expired")
		default:
			logger.Error(ctx, "failed to validate refresh_token", zap.Error(err))
			httputil.WriteError(w, http.StatusInternalServerError, "server_error", "internal error")
		}
		return
	}

	// 2. Issue new token pair (access_token + new refresh_token).
	resp, err := h.authUsecase.IssueTokenPair(ctx, rtInfo.UserID, rtInfo.DeviceID)
	if err != nil {
		logger.Error(ctx, "failed to issue new token pair", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "failed to issue token")
		return
	}

	logger.Info(ctx, "refresh_token rotation succeeded", zap.String("user_id", rtInfo.UserID.String()))
	// Return the new token pair. The response includes both access_token and refresh_token.
	httputil.WriteJSON(w, http.StatusOK, protocol.OAuth2TokenExchangeResponse{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresIn:    resp.ExpiresIn,
		UserId:       resp.UserID.String(),
	})
}

// ---------- Internal helper methods ----------

// PeekGrantType reads the grant_type from the request body without consuming it.
// The body is restored via io.NopCloser so downstream handlers can re-read it.
// Supports both JSON and form-encoded Content-Types.
// Exported for testing; used internally by handleToken.
func PeekGrantType(r *http.Request) string {
	// Read the body.
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit.
	if err != nil {
		return ""
	}
	// Restore the body for downstream handlers.
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var peek struct {
			GrantType string `json:"grant_type"`
		}
		if json.Unmarshal(bodyBytes, &peek) == nil {
			return peek.GrantType
		}
		return ""
	}

	// Form-encoded: parse values.
	if vals, err := url.ParseQuery(string(bodyBytes)); err == nil {
		return vals.Get("grant_type")
	}
	return ""
}

// parseRequestBody parses the request body, supporting application/json and
// form-urlencoded. JSON is decoded directly into target; form calls ParseForm
// then uses formFiller to populate target. A 1MB size limit is applied to
// prevent malicious clients from consuming excessive memory.
func parseRequestBody(w http.ResponseWriter, r *http.Request, target any, formFiller func(r *http.Request)) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit for all content types
	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		return json.NewDecoder(r.Body).Decode(target)
	}
	if err := r.ParseForm(); err != nil {
		return err
	}
	formFiller(r)
	return nil
}

// parseTokenExchangeRequest parses a token exchange request (supports JSON and form).
func parseTokenExchangeRequest(w http.ResponseWriter, r *http.Request) (*protocol.OAuth2TokenExchangeRequest, error) {
	var req protocol.OAuth2TokenExchangeRequest
	if err := parseRequestBody(w, r, &req, func(r *http.Request) {
		req.Code = r.FormValue("code")
		req.RedirectUri = r.FormValue("redirect_uri")
		req.State = r.FormValue("state")
		req.DeviceId = r.FormValue("device_id")
		req.DeviceName = model.StrPtr(r.FormValue("device_name"))
		req.UserAgent = model.StrPtr(r.FormValue("user_agent"))
	}); err != nil {
		return nil, err
	}
	return &req, nil
}

// parseRefreshRequest parses a refresh_token request (supports JSON and form).
func parseRefreshRequest(w http.ResponseWriter, r *http.Request) (*protocol.OAuth2TokenRefreshRequest, error) {
	var req protocol.OAuth2TokenRefreshRequest
	if err := parseRequestBody(w, r, &req, func(r *http.Request) {
		req.RefreshToken = r.FormValue("refresh_token")
	}); err != nil {
		return nil, err
	}
	return &req, nil
}

// generateState generates a random state string.
func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// verifyCodeChallenge validates a PKCE code_verifier against a stored code_challenge.
// Supports S256 (SHA-256 + base64url) and plain methods per RFC 7636.
func verifyCodeChallenge(codeChallenge, codeChallengeMethod, codeVerifier string) bool {
	switch codeChallengeMethod {
	case "S256":
		// SHA-256 hash the verifier, then base64url encode (no padding)
		h := sha256.Sum256([]byte(codeVerifier))
		computed := base64.RawURLEncoding.EncodeToString(h[:])
		return computed == codeChallenge
	case "plain":
		return codeVerifier == codeChallenge
	default:
		return false
	}
}
