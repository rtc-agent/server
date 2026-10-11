// Package usecase provides error types and constants for Token Exchange.
package usecase

import "fmt"

// RFC 8693 token type URIs.
const (
	TokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
	TokenTypeJWT         = "urn:ietf:params:oauth:token-type:jwt"
)

// RFC 6749 Section 5.2 error codes.
const (
	ErrInvalidRequest         = "invalid_request"
	ErrInvalidGrant           = "invalid_grant"
	ErrServerError            = "server_error"
	ErrTemporarilyUnavailable = "temporarily_unavailable"
	ErrAccessDenied           = "access_denied"
)

// TokenExchangeError is an OAuth2-compliant error returned by the Token Exchange usecase.
type TokenExchangeError struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
	HTTPStatus  int    `json:"-"`
}

func (e *TokenExchangeError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Description)
	}
	return e.Code
}
