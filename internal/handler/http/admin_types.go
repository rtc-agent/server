package httphandler

// LoginRequest admin login request payload
type LoginRequest struct {
	// Email admin user email address
	Email string `json:"email" binding:"required,email"`
	// Password admin user password (minimum 6 characters)
	Password string `json:"password" binding:"required,min=6"`
}

// LoginResponse admin login success response
type LoginResponse struct {
	// AccessToken JWT access token for authenticating API requests
	AccessToken string `json:"access_token"`
	// RefreshToken JWT refresh token for obtaining new access tokens
	RefreshToken string `json:"refresh_token"`
	// ExpiresIn access token lifetime in seconds
	ExpiresIn int `json:"expires_in"`
	// TokenType token type (always "Bearer")
	TokenType string `json:"token_type"`
	// User authenticated user information
	User *UserResponse `json:"user"`
}

// UserResponse basic user information returned in authentication responses
type UserResponse struct {
	// ID unique user identifier
	ID string `json:"id"`
	// Email user email address
	Email string `json:"email"`
	// Name user display name (optional)
	Name string `json:"name,omitempty"`
	// AvatarURL URL to user avatar image (optional)
	AvatarURL string `json:"avatar_url,omitempty"`
}

// RefreshRequest request payload for refreshing an access token
type RefreshRequest struct {
	// RefreshToken previously issued refresh token
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// RefreshResponse response containing new access and refresh tokens
type RefreshResponse struct {
	// AccessToken new JWT access token
	AccessToken string `json:"access_token"`
	// RefreshToken new JWT refresh token (rotated)
	RefreshToken string `json:"refresh_token"`
	// ExpiresIn new access token lifetime in seconds
	ExpiresIn int `json:"expires_in"`
	// TokenType token type (always "Bearer")
	TokenType string `json:"token_type"`
}

// LogoutRequest request payload for revoking a refresh token
type LogoutRequest struct {
	// RefreshToken refresh token to revoke
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// TokenExchangeRequest Token Exchange request following RFC 8693
type TokenExchangeRequest struct {
	// GrantType must be "urn:ietf:params:oauth:grant-type:token-exchange"
	GrantType string `form:"grant_type" binding:"required"`
	// SubjectToken the token to be exchanged
	SubjectToken string `form:"subject_token" binding:"required"`
	// SubjectTokenType type of the subject token (e.g. "urn:ietf:params:oauth:token-type:access_token")
	SubjectTokenType string `form:"subject_token_type" binding:"required"`
	// DeviceID client device identifier
	DeviceID string `form:"device_id" binding:"required"`
	// RequestedTokenType desired type of the issued token (optional)
	RequestedTokenType string `form:"requested_token_type"`
	// Audience intended audience for the issued token (optional)
	Audience string `form:"audience"`
	// Resource target resource for the issued token (optional)
	Resource string `form:"resource"`
	// Scope requested scope for the issued token (optional)
	Scope string `form:"scope"`
}

// TokenExchangeResponse Token Exchange success response
type TokenExchangeResponse struct {
	// AccessToken the issued access token
	AccessToken string `json:"access_token"`
	// IssuedTokenType type of the issued token
	IssuedTokenType string `json:"issued_token_type"`
	// TokenType token type (always "Bearer")
	TokenType string `json:"token_type"`
	// ExpiresIn issued token lifetime in seconds
	ExpiresIn int `json:"expires_in"`
	// Scope granted scope (optional)
	Scope string `json:"scope,omitempty"`
}

// OAuthError OAuth error response following RFC 6749 Section 5.2
type OAuthError struct {
	// Error error code (e.g. "invalid_request", "invalid_grant")
	Error string `json:"error"`
	// ErrorDescription human-readable error description
	ErrorDescription string `json:"error_description,omitempty"`
}

// HealthResponse health check response for admin-server
type HealthResponse struct {
	// Status health status ("ok" or "error")
	Status string `json:"status"`
	// Timestamp server timestamp in RFC3339 format
	Timestamp string `json:"timestamp,omitempty"`
	// Error error message when status is "error"
	Error string `json:"error,omitempty"`
}
