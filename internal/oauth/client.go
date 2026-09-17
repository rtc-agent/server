// Package oauth implements the ProviderClient, responsible for communicating
// with upstream OAuth2 providers.
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

// ProviderUserInfo holds user information returned by the provider.
type ProviderUserInfo struct {
	ProviderUserID string
	Username       string
	Email          string
	AvatarURL      string
}

// ProviderConfig holds configuration for a single provider.
type ProviderConfig struct {
	Name         string
	AuthURL      string // Provider authorization page URL (frontend redirect)
	TokenURL     string // Authorization code exchange URL (backend call)
	UserInfoURL  string // Optional: URL to fetch user info. Empty means TokenURL returns user info directly.
	ClientID     string
	ClientSecret string
}

// Client is the ProviderClient implementation, supporting multiple registered providers.
type Client struct {
	providers map[string]*ProviderConfig
	http      *http.Client
}

// NewClient creates a new ProviderClient.
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

// GetProviders returns the list of registered provider names.
func (c *Client) GetProviders() []string {
	names := make([]string, 0, len(c.providers))
	for name := range c.providers {
		names = append(names, name)
	}
	return names
}

// GetAuthorizationURL constructs the provider authorization page URL.
// Returns an error if the provider is not in the registered list
// (used to reject requests for unsupported providers).
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

// ExchangeCode exchanges an authorization code with the provider for user info.
func (c *Client) ExchangeCode(ctx context.Context, provider string, code string, redirectURI string) (*ProviderUserInfo, error) {
	cfg, ok := c.providers[provider]
	if !ok {
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}

	// Determine flow based on UserInfoURL.
	if cfg.UserInfoURL != "" {
		// Two-step mode: code -> access_token -> userinfo
		accessToken, err := c.exchangeCodeForToken(ctx, cfg, code, redirectURI)
		if err != nil {
			return nil, err
		}
		return c.fetchUserInfo(ctx, cfg.UserInfoURL, accessToken)
	}

	// Direct mode: TokenURL returns user info directly (Mock provider).
	return c.parseUserInfoFromTokenResponse(ctx, cfg, code, redirectURI)
}

// exchangeCodeForToken exchanges an authorization code for an access_token.
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

	// Try to parse access_token from JSON response.
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

// fetchUserInfo uses an access_token to retrieve user information.
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

	// Parse user info (supports multiple common field name conventions).
	var result struct {
		// GitHub/GitLab style (ID may be a number or string)
		ID        interface{} `json:"id"`
		Login     string      `json:"login"`
		Email     string      `json:"email"`
		AvatarURL string      `json:"avatar_url"`
		// Google/Generic style
		Sub     string `json:"sub"`
		Name    string `json:"name"`
		Picture string `json:"picture"`
		// Generic style
		ProviderUserID string `json:"provider_user_id"`
		Username       string `json:"username"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode user info response: %w", err)
	}

	// Priority: provider_user_id > sub > id
	providerUserID := result.ProviderUserID
	if providerUserID == "" {
		providerUserID = result.Sub
	}
	if providerUserID == "" && result.ID != nil {
		// ID may be a number (GitHub) or string (Google).
		switch v := result.ID.(type) {
		case float64:
			providerUserID = fmt.Sprintf("%.0f", v)
		case string:
			providerUserID = v
		}
	}

	// Priority: username > login > name
	username := result.Username
	if username == "" {
		username = result.Login
	}
	if username == "" {
		username = result.Name
	}

	// Priority: avatar_url > picture
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

// parseUserInfoFromTokenResponse provides backward compatibility for providers
// whose TokenURL endpoint returns user info directly (instead of requiring a
// separate UserInfo call).
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
