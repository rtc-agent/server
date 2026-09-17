// Package auth provides JWT token signing and verification.
package auth

import (
	"errors"
	"fmt"
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
type JWTSigner struct {
	secret    []byte
	accessTTL time.Duration
}

// NewJWTSigner creates a JWT signer.
//
// secret is the HMAC key (recommended >= 32 bytes random string);
// accessTTL is the default validity period for access tokens.
func NewJWTSigner(secret string, accessTTL time.Duration) (*JWTSigner, error) {
	if secret == "" {
		return nil, errors.New("auth: jwt secret is required")
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
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse access token: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid access token")
	}
	return claims, nil
}
