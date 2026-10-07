package httphandler

// LoginRequest admin login request payload
type LoginRequest struct {
	// Email admin user email address
	Email string `json:"email" binding:"required,email"`
	// Password admin user password (minimum 6 characters)
	Password string `json:"password" binding:"required,min=6"`
}

// CreateAdminUserRequest request payload for creating a new admin user
type CreateAdminUserRequest struct {
	// Email admin user email address (must be unique)
	Email string `json:"email" binding:"required,email"`
	// Password admin user password (minimum 6 characters)
	Password string `json:"password" binding:"required,min=6"`
	// Name admin user display name (optional)
	Name string `json:"name" binding:"omitempty,max=100"`
	// RoleIDs optional list of role IDs to assign after creation
	RoleIDs []string `json:"role_ids" binding:"omitempty,dive,uuid"`
}

// UpdateAdminUserRequest request payload for updating an admin user
type UpdateAdminUserRequest struct {
	// Name admin user display name (optional)
	Name *string `json:"name" binding:"omitempty,max=100"`
	// Password new password (optional, minimum 6 characters)
	Password *string `json:"password" binding:"omitempty,min=6"`
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
	// Roles user's assigned roles (empty when permission system is disabled)
	Roles []RoleInfo `json:"roles,omitempty"`
}

// RoleInfo basic role information
type RoleInfo struct {
	// ID role ID (optional, for backward compatibility)
	ID string `json:"id,omitempty"`
	// Name role name
	Name string `json:"name"`
	// DisplayName role display name
	DisplayName string `json:"display_name,omitempty"`
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

// SendOTPRequest email OTP send request payload
type SendOTPRequest struct {
	// Email admin user email address
	Email string `json:"email" binding:"required,email"`
}

// OTPLoginRequest email OTP login request payload
type OTPLoginRequest struct {
	// Email admin user email address
	Email string `json:"email" binding:"required,email"`
	// OTP verification code (6 digits)
	OTP string `json:"otp" binding:"required,len=6"`
}
