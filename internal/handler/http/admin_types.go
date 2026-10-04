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

// HealthResponse health check response for admin-server
type HealthResponse struct {
	// Status health status ("ok" or "error")
	Status string `json:"status"`
	// Timestamp server timestamp in RFC3339 format
	Timestamp string `json:"timestamp,omitempty"`
}
