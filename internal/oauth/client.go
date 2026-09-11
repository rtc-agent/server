// Package oauth2provider 实现 ProviderClient，负责与上游 OAuth2 Provider 通信。
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProviderUserInfo Provider 返回的用户信息
type ProviderUserInfo struct {
	ProviderUserID string
	Username       string
	Email          string
	AvatarURL      string
}

// ProviderConfig 单个 Provider 的配置
type ProviderConfig struct {
	Name         string
	AuthURL      string // Provider 授权页面 URL（前端跳转）
	TokenURL     string // 授权码换取 Token 的 URL（后端调用）
	UserInfoURL  string // 可选：获取用户信息的 URL。为空表示 TokenURL 直接返回用户信息
	ClientID     string
	ClientSecret string
}

// Client ProviderClient 实现，支持多个 Provider 注册
type Client struct {
	providers map[string]*ProviderConfig
	http      *http.Client
}

// NewClient 创建 ProviderClient
func NewClient(providers []*ProviderConfig, httpTimeout time.Duration) *Client {
	m := make(map[string]*ProviderConfig, len(providers))
	for _, p := range providers {
		m[p.Name] = p
	}
	return &Client{
		providers: m,
		http:      &http.Client{Timeout: httpTimeout},
	}
}

// GetProviders 返回已注册的 provider 名称列表
func (c *Client) GetProviders() []string {
	names := make([]string, 0, len(c.providers))
	for name := range c.providers {
		names = append(names, name)
	}
	return names
}

// GetAuthorizationURL 拼接 Provider 授权页面 URL
//
// 如果 provider 不在已注册列表中，返回错误（用于拒绝"不支持的 provider"请求）。
func (c *Client) GetAuthorizationURL(provider string, state string, redirectURI string) (string, error) {
	cfg, ok := c.providers[provider]
	if !ok {
		return "", fmt.Errorf("unsupported provider: %s", provider)
	}
	u, err := url.Parse(cfg.AuthURL)
	if err != nil {
		return "", fmt.Errorf("parse auth url for %s: %w", provider, err)
	}
	q := u.Query()
	q.Set("state", state)
	q.Set("client_id", cfg.ClientID)
	q.Set("response_type", "code")
	if redirectURI != "" {
		q.Set("redirect_uri", redirectURI)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ExchangeCode 用授权码向 Provider 换取用户信息
func (c *Client) ExchangeCode(ctx context.Context, provider string, code string, redirectURI string) (*ProviderUserInfo, error) {
	cfg, ok := c.providers[provider]
	if !ok {
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}

	// 根据 UserInfoURL 判断流程
	if cfg.UserInfoURL != "" {
		// 两步模式：code → access_token → userinfo
		accessToken, err := c.exchangeCodeForToken(ctx, cfg, code, redirectURI)
		if err != nil {
			return nil, err
		}
		return c.fetchUserInfo(ctx, cfg.UserInfoURL, accessToken)
	}

	// 直接模式：TokenURL 直接返回用户信息（Mock provider）
	return c.parseUserInfoFromTokenResponse(ctx, cfg, code, redirectURI)
}

// exchangeCodeForToken 用授权码换取 access_token
func (c *Client) exchangeCodeForToken(ctx context.Context, cfg *ProviderConfig, code string, redirectURI string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"code":          {code},
	}
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange code for token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("exchange code for token: provider returned status %d", resp.StatusCode)
	}

	// 尝试解析 JSON 格式的 access_token
	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("exchange code for token: access_token is empty")
	}

	return tokenResp.AccessToken, nil
}

// fetchUserInfo 使用 access_token 获取用户信息
func (c *Client) fetchUserInfo(ctx context.Context, userInfoURL string, accessToken string) (*ProviderUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build user info request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch user info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read user info response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch user info: provider returned status %d", resp.StatusCode)
	}

	// 解析用户信息（支持多种常见字段名）
	var result struct {
		// GitHub/GitLab 风格（ID 可能是数字或字符串）
		ID        interface{} `json:"id"`
		Login     string      `json:"login"`
		Email     string      `json:"email"`
		AvatarURL string      `json:"avatar_url"`
		// Google/Generic 风格
		Sub     string `json:"sub"`
		Name    string `json:"name"`
		Picture string `json:"picture"`
		// 通用风格
		ProviderUserID string `json:"provider_user_id"`
		Username       string `json:"username"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode user info response: %w", err)
	}

	// 优先级：provider_user_id > sub > id
	providerUserID := result.ProviderUserID
	if providerUserID == "" {
		providerUserID = result.Sub
	}
	if providerUserID == "" && result.ID != nil {
		// ID 可能是数字（GitHub）或字符串（Google）
		switch v := result.ID.(type) {
		case float64:
			providerUserID = fmt.Sprintf("%.0f", v)
		case string:
			providerUserID = v
		}
	}

	// 优先级：username > login > name
	username := result.Username
	if username == "" {
		username = result.Login
	}
	if username == "" {
		username = result.Name
	}

	// 优先级：avatar_url > picture
	avatarURL := result.AvatarURL
	if avatarURL == "" {
		avatarURL = result.Picture
	}

	if providerUserID == "" {
		return nil, fmt.Errorf("fetch user info: user id is empty")
	}

	return &ProviderUserInfo{
		ProviderUserID: providerUserID,
		Username:       username,
		Email:          result.Email,
		AvatarURL:      avatarURL,
	}, nil
}

// parseUserInfoFromTokenResponse 向后兼容：TokenURL 直接返回用户信息
func (c *Client) parseUserInfoFromTokenResponse(ctx context.Context, cfg *ProviderConfig, code string, redirectURI string) (*ProviderUserInfo, error) {
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"code":          {code},
	}
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read exchange response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange code: provider returned status %d", resp.StatusCode)
	}

	var result struct {
		ProviderUserID string `json:"provider_user_id"`
		Username       string `json:"username"`
		Email          string `json:"email"`
		AvatarURL      string `json:"avatar_url"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode exchange response: %w", err)
	}
	if result.ProviderUserID == "" {
		return nil, fmt.Errorf("exchange code: provider_user_id is empty")
	}

	return &ProviderUserInfo{
		ProviderUserID: result.ProviderUserID,
		Username:       result.Username,
		Email:          result.Email,
		AvatarURL:      result.AvatarURL,
	}, nil
}
