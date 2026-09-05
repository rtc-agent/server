package httphandler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/oauth"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/svc"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
)

// TokenSigner JWT 签名接口
type TokenSigner interface {
	SignAccessToken(userID uuid.UUID, deviceID string) (token string, expiresAt time.Time, err error)
	AccessTTL() time.Duration
}

// StateStore OAuth2 state 存储接口（CSRF 防护）
type StateStore interface {
	Set(ctx context.Context, state string, value string, ttl time.Duration) error
	GetDel(ctx context.Context, state string) (string, error)
}

// ProviderClient OAuth2 Provider 客户端接口
type ProviderClient interface {
	GetAuthorizationURL(provider string, state string, redirectURI string) (string, error)
	ExchangeCode(ctx context.Context, provider string, code string, redirectURI string) (*oauth.ProviderUserInfo, error)
}

// OAuth2Handler OAuth2 端点处理器
type OAuth2Handler struct {
	svcCtx         *svc.ServiceContext
	signer         TokenSigner
	stateStore     StateStore
	providerClient ProviderClient
	authConfig     config.AuthConfig
}

// NewOAuth2Handler 创建 OAuth2 端点处理器
func NewOAuth2Handler(svcCtx *svc.ServiceContext, signer TokenSigner, stateStore StateStore, providerClient ProviderClient, authCfg config.AuthConfig) *OAuth2Handler {
	return &OAuth2Handler{
		svcCtx:         svcCtx,
		signer:         signer,
		stateStore:     stateStore,
		providerClient: providerClient,
		authConfig:     authCfg,
	}
}

// RegisterRoutes 注册 OAuth2 路由到 HTTP ServeMux
func (h *OAuth2Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth2/authorize", h.handleAuthorize)
	mux.HandleFunc("POST /oauth2/token", h.handleToken)
	mux.HandleFunc("POST /oauth2/refresh", h.handleRefresh)
}

// handleAuthorize 处理 GET /oauth2/authorize
// 生成 state，返回 Provider 授权页面重定向 URL
func (h *OAuth2Handler) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "provider 参数不能为空")
		return
	}
	redirectURI := r.URL.Query().Get("redirect_uri")

	// 生成随机 state
	state, err := generateState()
	if err != nil {
		logger.Error(r.Context(), "生成 state 失败", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "生成 state 失败")
		return
	}

	// 拼接 Provider 授权 URL（同时验证 provider 是否存在）
	redirectURL, err := h.providerClient.GetAuthorizationURL(provider, state, redirectURI)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("不支持的 provider: %s", provider))
		return
	}

	// 存入 StateStore（key=state, value=provider, TTL=10min）
	ctx := r.Context()
	if err := h.stateStore.Set(ctx, state, provider, h.authConfig.OAuth2StateTTL); err != nil {
		logger.Error(ctx, "存储 state 失败", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "存储 state 失败")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, protocol.OAuth2AuthorizeResponse{
		RedirectUrl: redirectURL,
		State:       state,
	})
}

// handleToken 处理 POST /oauth2/token
// 支持 JSON 和 application/x-www-form-urlencoded 两种 Content-Type
func (h *OAuth2Handler) handleToken(w http.ResponseWriter, r *http.Request) {
	req, err := parseTokenExchangeRequest(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "请求解析失败")
		return
	}

	if req.Code == "" || req.State == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "code 和 state 不能为空")
		return
	}

	h.handleAuthorizationCodeGrant(w, r, req)
}

// handleAuthorizationCodeGrant 处理 authorization_code 换取 token
func (h *OAuth2Handler) handleAuthorizationCodeGrant(w http.ResponseWriter, r *http.Request, req *protocol.OAuth2TokenExchangeRequest) {
	ctx := r.Context()

	// 1. 从 StateStore 验证 state（获取关联的 provider），验证后删除
	provider, err := h.stateStore.GetDel(ctx, req.State)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "state 无效或已过期")
		return
	}

	// 2. 用授权码换取用户信息
	userInfo, err := h.providerClient.ExchangeCode(ctx, provider, req.Code, req.RedirectUri)
	if err != nil {
		logger.Error(ctx, "ExchangeCode 失败", zap.String("provider", provider), zap.Error(err))
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "授权码交换失败")
		return
	}

	// 3. 查找或创建 OAuth2User
	user, err := h.findOrCreateUser(ctx, provider, userInfo)
	if err != nil {
		logger.Error(ctx, "查找或创建用户失败", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "认证失败")
		return
	}

	// 4. 查找或更新 Device
	if req.DeviceId != "" {
		if err := h.upsertDevice(ctx, user.ID, req); err != nil {
			logger.Warn(ctx, "更新设备信息失败", zap.Error(err))
		}
	}

	// 5. 签发 token pair
	resp, err := h.issueTokenPair(ctx, user.ID, req.DeviceId)
	if err != nil {
		logger.Error(ctx, "签发 token 失败", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "签发 token 失败")
		return
	}

	logger.Info(ctx, "authorization_code 认证成功", zap.String("provider", provider), zap.String("user_id", user.ID.String()))
	httputil.WriteJSON(w, http.StatusOK, *resp)
}

// handleRefresh 处理 POST /oauth2/refresh
func (h *OAuth2Handler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	req, err := parseRefreshRequest(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "请求体解析失败")
		return
	}

	if req.RefreshToken == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "refresh_token 不能为空")
		return
	}

	ctx := r.Context()

	// 1. hash 后查找
	rtHash := hashRefreshToken(req.RefreshToken)
	rt, err := h.svcCtx.RefreshTokenRepo.FindByHash(ctx, rtHash)
	if err != nil {
		if repo.IsNotFound(err) {
			httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "refresh_token 无效")
		} else {
			logger.Error(ctx, "查找 refresh_token 失败", zap.Error(err))
			httputil.WriteError(w, http.StatusInternalServerError, "server_error", "内部错误")
		}
		return
	}

	// 2. 检查未过期、未撤销
	if rt.Revoked {
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "refresh_token 已撤销")
		return
	}
	if time.Now().After(rt.ExpiresAt) {
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_grant", "refresh_token 已过期")
		return
	}

	// 3. 签发新 access_token
	accessToken, expiresAt, err := h.signer.SignAccessToken(rt.UserID, rt.DeviceID)
	if err != nil {
		logger.Error(ctx, "签发 access_token 失败", zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "server_error", "签发 token 失败")
		return
	}

	logger.Info(ctx, "refresh_token 刷新成功", zap.String("user_id", rt.UserID.String()))
	httputil.WriteJSON(w, http.StatusOK, protocol.OAuth2TokenRefreshResponse{
		AccessToken: accessToken,
		ExpiresIn:   int64(time.Until(expiresAt).Seconds()),
	})
}

// ---------- 内部辅助方法 ----------

// parseRequestBody 解析请求体，支持 application/json 和 form-urlencoded。
// JSON 直接解码到 target；form 先 ParseForm 再调用 formFiller 填充 target。
func parseRequestBody(r *http.Request, target any, formFiller func(r *http.Request)) error {
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

// parseTokenExchangeRequest 解析 token 交换请求（支持 JSON 和 form）
func parseTokenExchangeRequest(r *http.Request) (*protocol.OAuth2TokenExchangeRequest, error) {
	var req protocol.OAuth2TokenExchangeRequest
	if err := parseRequestBody(r, &req, func(r *http.Request) {
		req.Code = r.FormValue("code")
		req.RedirectUri = r.FormValue("redirect_uri")
		req.State = r.FormValue("state")
		req.DeviceId = r.FormValue("device_id")
		req.DeviceName = strPtr(r.FormValue("device_name"))
		req.UserAgent = strPtr(r.FormValue("user_agent"))
	}); err != nil {
		return nil, err
	}
	return &req, nil
}

// parseRefreshRequest 解析 refresh_token 请求（支持 JSON 和 form）
func parseRefreshRequest(r *http.Request) (*protocol.OAuth2TokenRefreshRequest, error) {
	var req protocol.OAuth2TokenRefreshRequest
	if err := parseRequestBody(r, &req, func(r *http.Request) {
		req.RefreshToken = r.FormValue("refresh_token")
	}); err != nil {
		return nil, err
	}
	return &req, nil
}

// findOrCreateUser 查找或创建 OAuth2 用户（「先查后建」模式）
func (h *OAuth2Handler) findOrCreateUser(ctx context.Context, provider string, userInfo *oauth.ProviderUserInfo) (*model.OAuth2User, error) {
	user, err := h.svcCtx.OAuth2UserRepo.FindByProvider(ctx, provider, userInfo.ProviderUserID)
	if err != nil {
		if !repo.IsNotFound(err) {
			return nil, fmt.Errorf("find user by provider: %w", err)
		}
		// 记录不存在，创建
		user = &model.OAuth2User{
			Provider:  provider,
			Sub:       userInfo.ProviderUserID,
			Name:      userInfo.Username,
			Email:     userInfo.Email,
			AvatarURL: userInfo.AvatarURL,
		}
		if err := h.svcCtx.OAuth2UserRepo.Create(ctx, user); err != nil {
			return nil, fmt.Errorf("create user: %w", err)
		}
		return user, nil
	}
	// 已存在，更新用户信息
	user.Name = userInfo.Username
	user.Email = userInfo.Email
	user.AvatarURL = userInfo.AvatarURL
	if err := h.svcCtx.OAuth2UserRepo.Update(ctx, user); err != nil {
		logger.Warn(ctx, "更新用户信息失败", zap.Error(err))
	}
	return user, nil
}

// upsertDevice 查找或更新设备信息
func (h *OAuth2Handler) upsertDevice(ctx context.Context, userID uuid.UUID, req *protocol.OAuth2TokenExchangeRequest) error {
	device := &model.Device{
		UserID:       userID,
		DeviceID:     req.DeviceId,
		Name:         derefStr(req.DeviceName),
		UserAgent:    derefStr(req.UserAgent),
		LastActiveAt: time.Now(),
	}
	return h.svcCtx.DeviceRepo.Upsert(ctx, device)
}

// issueTokenPair 签发 access_token + refresh_token
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
		UserId:       protocol.UUID(userID.String()),
	}, nil
}

// generateState 生成随机 state
func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// generateRefreshToken 生成不透明的 refresh_token
func generateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "rt_" + hex.EncodeToString(b), nil
}

// hashRefreshToken 计算 refresh_token 的 SHA-256 哈希
func hashRefreshToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// strPtr 将非空字符串转为 *string，空字符串返回 nil。
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// derefStr 解引用 *string，nil 返回空字符串。
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
