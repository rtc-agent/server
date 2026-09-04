// Package mockoauth2 实现简化的 Mock OAuth2 服务器，用于开发测试。
//
// 流程：
// 1. 访问授权页面 → 输入 User ID → 生成授权码
// 2. 使用授权码 + client_id/client_secret 换取用户信息
package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rtc-agent/server/internal/infra/httputil"
)

// Config Mock OAuth2 配置
type Config struct {
	ClientID     string
	ClientSecret string
}

// Provider Mock OAuth2 提供者
type Provider struct {
	config Config
	mu     sync.RWMutex
	codes  map[string]*authCodeData // 授权码 → 数据
}

// authCodeData 授权码数据
type authCodeData struct {
	UserID    string
	ExpiresAt time.Time
	Used      bool
}

// NewProvider 创建 Mock OAuth2 Provider
func NewProvider(cfg Config) *Provider {
	return &Provider{
		config: cfg,
		codes:  make(map[string]*authCodeData),
	}
}

// RegisterRoutes 注册路由到 http.ServeMux
func (p *Provider) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/oauth2/authorize", p.handleAuthorize)
	mux.HandleFunc("/oauth2/token/exchange", p.handleTokenExchange)
}

// handleAuthorize 处理授权请求
// GET: 显示 HTML 授权页面（输入 User ID，点击授权）
// POST: 生成授权码并重定向
func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.serveAuthorizePage(w, r)
	case http.MethodPost:
		p.handleAuthorizeConfirm(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveAuthorizePage 显示 HTML 授权页面
func (p *Provider) serveAuthorizePage(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	redirectURI := r.URL.Query().Get("redirect_uri")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
  <title>Mock OAuth2 授权</title>
  <style>
    body { font-family: sans-serif; max-width: 400px; margin: 50px auto; padding: 20px; }
    input, button { display: block; width: 100%%; margin: 10px 0; padding: 10px; box-sizing: border-box; }
    button { background: #4CAF50; color: white; border: none; cursor: pointer; font-size: 16px; }
    button:hover { background: #45a049; }
    h1 { text-align: center; }
  </style>
</head>
<body>
  <h1>Mock OAuth2 授权</h1>
  <form method="POST" action="/oauth2/authorize">
    <input type="hidden" name="state" value="%s">
    <input type="hidden" name="redirect_uri" value="%s">
    <label for="user_id">User ID:</label>
    <input type="text" id="user_id" name="user_id" placeholder="输入用户 ID" required>
    <label for="username">Username (可选):</label>
    <input type="text" id="username" name="username" placeholder="用户名">
    <label for="email">Email (可选):</label>
    <input type="text" id="email" name="email" placeholder="email@example.com">
    <button type="submit">授权</button>
  </form>
</body>
</html>`, state, redirectURI)
}

// handleAuthorizeConfirm 处理授权确认
func (p *Provider) handleAuthorizeConfirm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	userID := r.FormValue("user_id")
	username := r.FormValue("username")
	email := r.FormValue("email")
	state := r.FormValue("state")
	redirectURI := r.FormValue("redirect_uri")

	if userID == "" {
		http.Error(w, "user_id is required", http.StatusBadRequest)
		return
	}

	// 生成授权码
	code := generateCode()

	p.mu.Lock()
	p.codes[code] = &authCodeData{
		UserID:    userID,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	p.mu.Unlock()

	// 重定向回客户端（携带 code 和 state）
	if redirectURI != "" {
		sep := "?"
		if strings.Contains(redirectURI, "?") {
			sep = "&" // URI 已含查询参数，用 & 拼接
		}
		redirectURL := fmt.Sprintf("%s%scode=%s&state=%s&username=%s&email=%s",
			redirectURI, sep, code, state, username, email)
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}

	// 没有 redirect_uri，直接返回 JSON
	httputil.WriteJSON(w, http.StatusOK, map[string]string{
		"code":  code,
		"state": state,
	})
}

// handleTokenExchange 处理授权码换取用户信息
// POST /oauth2/token/exchange
// 参数: client_id, client_secret, code, redirect_uri
// 返回: 用户信息 JSON
func (p *Provider) handleTokenExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		// 尝试 JSON 解析
		var body struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			Code         string `json:"code"`
			RedirectURI  string `json:"redirect_uri"`
			Username     string `json:"username"`
			Email        string `json:"email"`
		}
		if jsonErr := json.NewDecoder(r.Body).Decode(&body); jsonErr != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "无法解析请求参数")
			return
		}
		p.processExchange(w, body.ClientID, body.ClientSecret, body.Code, body.Username, body.Email)
		return
	}

	clientID := r.FormValue("client_id")
	clientSecret := r.FormValue("client_secret")
	code := r.FormValue("code")
	username := r.FormValue("username")
	email := r.FormValue("email")

	p.processExchange(w, clientID, clientSecret, code, username, email)
}

// processExchange 处理授权码交换
func (p *Provider) processExchange(w http.ResponseWriter, clientID, clientSecret, code, username, email string) {
	// 验证 client 凭据
	if clientID != p.config.ClientID || clientSecret != p.config.ClientSecret {
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_client", "无效的客户端凭据")
		return
	}

	if code == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "code 不能为空")
		return
	}

	// 查找授权码
	p.mu.Lock()
	defer p.mu.Unlock()
	codeData, ok := p.codes[code]
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_grant", "授权码无效")
		return
	}
	if codeData.Used {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_grant", "授权码已使用")
		return
	}
	if time.Now().After(codeData.ExpiresAt) {
		delete(p.codes, code)
		httputil.WriteError(w, http.StatusBadRequest, "invalid_grant", "授权码已过期")
		return
	}
	codeData.Used = true

	// 构造用户信息
	userID := codeData.UserID
	if username == "" {
		username = "user_" + userID
	}
	if email == "" {
		email = userID + "@example.com"
	}

	// 直接返回用户信息
	httputil.WriteJSON(w, http.StatusOK, map[string]string{
		"provider_user_id": userID,
		"username":         username,
		"email":            email,
		"avatar_url":       "",
	})
}

// generateCode 生成随机授权码
func generateCode() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
