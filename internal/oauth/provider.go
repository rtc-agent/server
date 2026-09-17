// Package oauth implements a simplified Mock OAuth2 server for development
// and testing.
//
// Flow:
// 1. Visit the authorization page -> enter User ID -> generate authorization code
// 2. Exchange authorization code + client_id/client_secret for user info
package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rtc-agent/server/internal/infra/httputil"
)

// Config holds Mock OAuth2 configuration.
type Config struct {
	ClientID     string
	ClientSecret string
}

// Provider is the Mock OAuth2 provider.
type Provider struct {
	config Config
	mu     sync.RWMutex
	codes  map[string]*authCodeData // authorization code -> data
}

// authCodeData holds authorization code data.
type authCodeData struct {
	UserID    string
	ExpiresAt time.Time
	Used      bool
}

// NewProvider creates a new Mock OAuth2 Provider.
func NewProvider(cfg Config) *Provider {
	return &Provider{
		config: cfg,
		codes:  make(map[string]*authCodeData),
	}
}

// RegisterRoutes registers routes to the http.ServeMux.
func (p *Provider) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/oauth2/authorize", p.handleAuthorize)
	mux.HandleFunc("/oauth2/token/exchange", p.handleTokenExchange)
}

// handleAuthorize handles authorization requests.
// GET: displays the HTML authorization page (enter User ID, click authorize)
// POST: generates an authorization code and redirects
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

// serveAuthorizePage displays the HTML authorization page.
func (p *Provider) serveAuthorizePage(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	redirectURI := r.URL.Query().Get("redirect_uri")

	// Validate redirect_uri format to prevent open redirect
	if redirectURI != "" {
		if !isValidRedirectURI(redirectURI) {
			http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// HTML-escape user-supplied values to prevent XSS
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
  <title>Mock OAuth2 Authorization</title>
  <style>
    body { font-family: sans-serif; max-width: 400px; margin: 50px auto; padding: 20px; }
    input, button { display: block; width: 100%%; margin: 10px 0; padding: 10px; box-sizing: border-box; }
    button { background: #4CAF50; color: white; border: none; cursor: pointer; font-size: 16px; }
    button:hover { background: #45a049; }
    h1 { text-align: center; }
  </style>
</head>
<body>
  <h1>Mock OAuth2 Authorization</h1>
  <form method="POST" action="/oauth2/authorize">
    <input type="hidden" name="state" value="%s">
    <input type="hidden" name="redirect_uri" value="%s">
    <label for="user_id">User ID:</label>
    <input type="text" id="user_id" name="user_id" placeholder="Enter user ID" required>
    <label for="username">Username (optional):</label>
    <input type="text" id="username" name="username" placeholder="Username">
    <label for="email">Email (optional):</label>
    <input type="text" id="email" name="email" placeholder="email@example.com">
    <button type="submit">Authorize</button>
  </form>
</body>
</html>`, html.EscapeString(state), html.EscapeString(redirectURI))
}

// handleAuthorizeConfirm handles authorization confirmation.
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

	// Validate redirect_uri to prevent open redirect
	if redirectURI != "" && !isValidRedirectURI(redirectURI) {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}

	// Generate authorization code.
	code := generateCode()

	p.mu.Lock()
	p.codes[code] = &authCodeData{
		UserID:    userID,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	p.mu.Unlock()

	// Redirect back to the client (with code and state).
	if redirectURI != "" {
		sep := "?"
		if strings.Contains(redirectURI, "?") {
			sep = "&" // URI already contains query params, use & to append
		}
		redirectURL := fmt.Sprintf("%s%scode=%s&state=%s&username=%s&email=%s",
			redirectURI, sep, url.QueryEscape(code), url.QueryEscape(state),
			url.QueryEscape(username), url.QueryEscape(email))
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}

	// No redirect_uri, return JSON directly.
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

	// 限制请求体大小，防止恶意大 payload 耗尽内存
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB

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
	if _, err := rand.Read(b); err != nil {
		// rand.Read 失败意味着系统熵源耗尽，无法生成安全随机数
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// isValidRedirectURI 验证 redirect_uri 格式，防止开放重定向攻击。
// 仅允许 http/https 协议的绝对 URL。
func isValidRedirectURI(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	// 必须是绝对 URL 且协议为 http 或 https
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	// 必须有 host
	if u.Host == "" {
		return false
	}
	// 禁止 javascript: 等危险协议（已通过 scheme 检查排除）
	// 禁止 fragment 中的重定向（防止 location.hash 操纵）
	return true
}
