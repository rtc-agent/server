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
	TokenURL     string // 授权码换取用户信息的 URL（后端调用）
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
		return nil, fmt.Errorf("exchange code: status %d, body: %s", resp.StatusCode, string(body))
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
