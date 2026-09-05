// Package auth 提供 JWT 令牌签发与验证实现。
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims JWT 载荷
type Claims struct {
	UserID   uuid.UUID `json:"user_id"`
	DeviceID string    `json:"device_id"`
	jwt.RegisteredClaims
}

// JWTSigner 基于 HMAC-SHA256 的 TokenSigner 实现
type JWTSigner struct {
	secret    []byte
	accessTTL time.Duration
}

// NewJWTSigner 创建 JWT 签名器
//
// secret 为 HMAC 密钥（建议 ≥ 32 字节随机字符串）；
// accessTTL 为 access_token 默认有效期。
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

// SignAccessToken 签发 access_token（HMAC-SHA256）
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

// AccessTTL 返回 access_token 默认有效期
func (s *JWTSigner) AccessTTL() time.Duration {
	return s.accessTTL
}

// ParseAccessToken 解析并验证 access_token，返回 claims
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
