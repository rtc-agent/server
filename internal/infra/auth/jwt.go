// Package auth provides JWT token signing and verification.
package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims is the JWT payload.
type Claims struct {
	UserID   uuid.UUID `json:"user_id"`
	DeviceID string    `json:"device_id"`
	jwt.RegisteredClaims
}

// JWTSigner is an HMAC-SHA256 based token signer implementation.
//
// SECURITY NOTE: HS256 is a symmetric signing algorithm suitable for single-service
// deployments where the same service signs and verifies tokens. For multi-service
// architectures or when tokens need to be verified by external services, consider
// using AdminJWTSigner with asymmetric algorithms (RS256/ES256/EdDSA).
type JWTSigner struct {
	secret    []byte
	accessTTL time.Duration
}

// NewJWTSigner creates a JWT signer.
//
// secret is the HMAC key (recommended >= 32 bytes random string);
// accessTTL is the default validity period for access tokens.
//
// SECURITY REQUIREMENTS:
//   - secret MUST be at least 32 bytes of cryptographically random data
//   - In production, use strong asymmetric algorithms (RS256/ES256) instead
//   - Rotate secrets regularly and store securely (e.g., AWS Secrets Manager, HashiCorp Vault)
func NewJWTSigner(secret string, accessTTL time.Duration) (*JWTSigner, error) {
	if secret == "" {
		return nil, errors.New("auth: jwt secret is required")
	}
	// Enforce minimum secret strength for HMAC-SHA256
	if len(secret) < 32 {
		return nil, fmt.Errorf("auth: jwt secret must be at least 32 bytes, got %d", len(secret))
	}
	if accessTTL <= 0 {
		return nil, errors.New("auth: access token TTL must be positive")
	}
	return &JWTSigner{
		secret:    []byte(secret),
		accessTTL: accessTTL,
	}, nil
}

// SignAccessToken signs an access token (HMAC-SHA256).
func (s *JWTSigner) SignAccessToken(userID uuid.UUID, deviceID string) (string, time.Time, error) {
	expiresAt := time.Now().Add(s.accessTTL)
	claims := Claims{
		UserID:   userID,
		DeviceID: deviceID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "rtc-agent",
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// AccessTTL returns the default validity period for access tokens.
func (s *JWTSigner) AccessTTL() time.Duration {
	return s.accessTTL
}

// ParseAccessToken parses and validates an access token, returning claims.
func (s *JWTSigner) ParseAccessToken(tokenString string) (*Claims, error) {
	if tokenString == "" {
		RecordFailure("jwt", "missing")
		return nil, errors.New("parse access token: token is empty")
	}

	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			RecordFailure("jwt", "signature")
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil {
		// Classify the error reason for metrics
		reason := classifyJWTError(err)
		RecordFailure("jwt", reason)
		return nil, fmt.Errorf("parse access token: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		RecordFailure("jwt", "claims")
		return nil, errors.New("invalid access token")
	}
	return claims, nil
}

// classifyJWTError categorizes a JWT parsing error for metrics.
func classifyJWTError(err error) string {
	errStr := err.Error()
	switch {
	case strings.Contains(errStr, "token is expired"):
		return "expired"
	case strings.Contains(errStr, "token is not valid yet"):
		return "not_yet_valid"
	case strings.Contains(errStr, "token used before issued"):
		return "issued_after"
	case strings.Contains(errStr, "signature"):
		return "signature"
	default:
		return "invalid"
	}
}
