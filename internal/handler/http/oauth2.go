package httphandler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/oauth"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/svc"
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
	GetAuthorizationURL(provider string, state string, redirectURI string) (string, error)
	ExchangeCode(ctx context.Context, provider string, code string, redirectURI string) (*oauth.ProviderUserInfo, error)
	GetProviders() []string
}

// OAuth2Handler handles OAuth2 endpoints.
type OAuth2Handler struct {
	svcCtx         *svc.ServiceContext
	signer         TokenSigner
	stateStore     StateStore
	providerClient ProviderClient
	authConfig     config.AuthConfig
}

// NewOAuth2Handler creates a new OAuth2 endpoint handler.
func NewOAuth2Handler(svcCtx *svc.ServiceContext, signer TokenSigner, stateStore StateStore, providerClient ProviderClient, authCfg config.AuthConfig) *OAuth2Handler {
	return &OAuth2Handler{
		svcCtx:         svcCtx,
		signer:         signer,
		stateStore:     stateStore,
		providerClient: providerClient,
		authConfig:     authCfg,
	}
}

// RegisterRoutes registers OAuth2 routes to the HTTP ServeMux.
func (h *OAuth2Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth2/authorize", h.handleAuthorize)
	mux.HandleFunc("POST /oauth2/token", h.handleToken)
	mux.HandleFunc("POST /oauth2/refresh", h.handleRefresh)
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
	redirectURL, err := h.providerClient.GetAuthorizationURL(provider, state, redirectURI)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("unsupported provider: %s", provider))
		return
	}

	// Store in StateStore (key=state, value=provider, TTL=10min).
	ctx := r.Context()
	if err := h.stateStore.Set(ctx, state, provider, h.authConfig.OAuth2StateTTL); err != nil {
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
func (h *OAuth2Handler) handleToken(w http.ResponseWriter, r *http.Request) {
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

	// 1. Validate state from StateStore (retrieve associated provider), then delete.
	provider, err := h.stateStore.GetDel(ctx, req.State)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "state is invalid or expired")
		return
	}

	// 2. Exchange authorization code for user info.
	userInfo, err := h.providerClient.ExchangeCode(ctx, provider, req.Code, req.RedirectUri)
	if err != nil {
		logger.Error(ctx, "ExchangeCode failed", zap.String("provider", provider), zap.Error(err))
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "authorization code exchange failed")
		return
	}

	// 3. Find or create OAuth2User.
	user, err := h.findOrCreateUser(ctx, provider, userInfo)
	if err != nil {
		logger.Error(ctx, "failed to find or create user", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "authentication failed")
		return
	}

	// 4. Find or update Device.
	if req.DeviceId != "" {
		if err := h.upsertDevice(ctx, user.ID, req); err != nil {
			logger.Warn(ctx, "failed to update device info", zap.Error(err))
		}
	}

	// 5. Issue token pair.
	resp, err := h.issueTokenPair(ctx, user.ID, req.DeviceId)
	if err != nil {
		logger.Error(ctx, "failed to issue token", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "failed to issue token")
		return
	}

	logger.Info(ctx, "authorization_code authentication succeeded", zap.String("provider", provider), zap.String("user_id", user.ID.String()))
	httputil.WriteJSON(w, http.StatusOK, *resp)
}

// handleRefresh handles POST /oauth2/refresh.
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

	// 1. Look up by hashed token.
	rtHash := hashRefreshToken(req.RefreshToken)
	rt, err := h.svcCtx.RefreshTokenRepo.FindByHash(ctx, rtHash)
	if err != nil {
		if repo.IsNotFound(err) {
			httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "refresh_token is invalid")
		} else {
			logger.Error(ctx, "failed to find refresh_token", zap.Error(err))
			httputil.WriteError(w, http.StatusInternalServerError, "server_error", "internal error")
		}
		return
	}

	// 2. Check not expired and not revoked.
	if rt.Revoked {
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "refresh_token has been revoked")
		return
	}
	if time.Now().After(rt.ExpiresAt) {
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "refresh_token has expired")
		return
	}

	// 3. Issue new access_token.
	accessToken, expiresAt, err := h.signer.SignAccessToken(rt.UserID, rt.DeviceID)
	if err != nil {
		logger.Error(ctx, "failed to sign access_token", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "failed to issue token")
		return
	}

	logger.Info(ctx, "refresh_token refresh succeeded", zap.String("user_id", rt.UserID.String()))
	httputil.WriteJSON(w, http.StatusOK, protocol.OAuth2TokenRefreshResponse{
		AccessToken: accessToken,
		ExpiresIn:   int64(time.Until(expiresAt).Seconds()),
	})
}

// ---------- Internal helper methods ----------

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

// findOrCreateUser finds or creates an OAuth2 user using a "lookup + unique
// constraint fallback" pattern.
//
// Concurrency safe: when two requests with the same provider+sub arrive
// simultaneously, one Create succeeds and the other triggers a unique
// constraint violation (23505), falling back to re-lookup.
func (h *OAuth2Handler) findOrCreateUser(ctx context.Context, provider string, userInfo *oauth.ProviderUserInfo) (*model.OAuth2User, error) {
	user, err := h.svcCtx.OAuth2UserRepo.FindByProvider(ctx, provider, userInfo.ProviderUserID)
	if err != nil {
		if !repo.IsNotFound(err) {
			return nil, fmt.Errorf("find user by provider: %w", err)
		}
		// Record does not exist, create it.
		user = &model.OAuth2User{
			Provider:  provider,
			Sub:       userInfo.ProviderUserID,
			Name:      userInfo.Username,
			Email:     userInfo.Email,
			AvatarURL: userInfo.AvatarURL,
		}
		if err := h.svcCtx.OAuth2UserRepo.Create(ctx, user); err != nil {
			// Unique constraint violation → concurrent creation → re-lookup.
			if isDuplicateKeyError(err) {
				logger.Info(ctx, "findOrCreateUser: concurrent create detected, re-fetching",
					zap.String("provider", provider))
				return h.svcCtx.OAuth2UserRepo.FindByProvider(ctx, provider, userInfo.ProviderUserID)
			}
			return nil, fmt.Errorf("create user: %w", err)
		}
		return user, nil
	}
	// Already exists, update user info.
	user.Name = userInfo.Username
	user.Email = userInfo.Email
	user.AvatarURL = userInfo.AvatarURL
	if err := h.svcCtx.OAuth2UserRepo.Update(ctx, user); err != nil {
		logger.Warn(ctx, "failed to update user info", zap.Error(err))
	}
	return user, nil
}

// isDuplicateKeyError checks whether the error is a PostgreSQL unique
// constraint violation (code 23505).
func isDuplicateKeyError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// upsertDevice finds or updates device information.
func (h *OAuth2Handler) upsertDevice(ctx context.Context, userID uuid.UUID, req *protocol.OAuth2TokenExchangeRequest) error {
	device := &model.Device{
		UserID:       userID,
		DeviceID:     req.DeviceId,
		Name:         model.DerefStr(req.DeviceName),
		UserAgent:    model.DerefStr(req.UserAgent),
		LastActiveAt: time.Now(),
	}
	return h.svcCtx.DeviceRepo.Upsert(ctx, device)
}

// issueTokenPair issues an access_token + refresh_token pair.
func (h *OAuth2Handler) issueTokenPair(ctx context.Context, userID uuid.UUID, deviceID string) (*protocol.OAuth2TokenExchangeResponse, error) {
	accessToken, expiresAt, err := h.signer.SignAccessToken(userID, deviceID)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	rtPlain, err := generateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}
	rtHash := hashRefreshToken(rtPlain)

	rt := &model.RefreshToken{
		TokenHash: rtHash,
		UserID:    userID,
		DeviceID:  deviceID,
		ExpiresAt: expiresAt.Add(h.authConfig.RefreshTokenTTL),
		Revoked:   false,
	}
	if err := h.svcCtx.RefreshTokenRepo.Create(ctx, rt); err != nil {
		return nil, fmt.Errorf("store refresh token: %w", err)
	}

	return &protocol.OAuth2TokenExchangeResponse{
		AccessToken:  accessToken,
		RefreshToken: rtPlain,
		ExpiresIn:    int64(h.signer.AccessTTL().Seconds()),
		UserId:       userID.String(),
	}, nil
}

// generateState generates a random state string.
func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// generateRefreshToken generates an opaque refresh_token.
func generateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "rt_" + hex.EncodeToString(b), nil
}

// hashRefreshToken computes the SHA-256 hash of a refresh_token.
func hashRefreshToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
