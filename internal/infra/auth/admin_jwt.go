// Package auth provides JWT token signing and verification for admin-server.
package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/lestrrat-go/jwx/v2/jwk"
)

// Clock abstracts time retrieval for testability.
type Clock interface {
	Now() time.Time
}

// realClock is the default Clock implementation using system time.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// AdminClaims is the JWT payload for admin tokens.
type AdminClaims struct {
	UserID uuid.UUID `json:"sub"`
	Email  string    `json:"email,omitempty"`
	Name   string    `json:"name,omitempty"`
	jwt.RegisteredClaims
}

// AdminJWTSigner is an RSA/ECDSA based token signer for admin-server.
type AdminJWTSigner struct {
	privateKey crypto.PrivateKey
	publicKey  crypto.PublicKey
	keyID      string
	algorithm  string
	issuer     string
	audience   string
	accessTTL  time.Duration
	refreshTTL time.Duration
	clock      Clock
}

// AdminJWTConfig holds the configuration for AdminJWTSigner.
type AdminJWTConfig struct {
	Algorithm      string
	Issuer         string
	Audience       string
	PrivateKeyPath string
	PublicKeyPath  string
	AccessTTL      time.Duration
	RefreshTTL     time.Duration
	// Clock allows injecting a custom clock for testing. If nil, uses real time.
	Clock Clock
}

// NewAdminJWTSigner creates an admin JWT signer from configuration.
func NewAdminJWTSigner(cfg AdminJWTConfig) (*AdminJWTSigner, error) {
	if cfg.Algorithm == "" {
		cfg.Algorithm = "RS256"
	}
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = time.Hour
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 7 * 24 * time.Hour
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}

	var privateKey crypto.PrivateKey
	var publicKey crypto.PublicKey
	var keyID string
	var err error

	// Load or generate keys
	if cfg.PrivateKeyPath != "" && cfg.PublicKeyPath != "" {
		privateKey, publicKey, err = loadKeys(cfg.PrivateKeyPath, cfg.PublicKeyPath, cfg.Algorithm)
		if err != nil {
			return nil, fmt.Errorf("load keys: %w", err)
		}
		keyID = "admin-key-1"
	} else {
		// Generate ephemeral keys for development
		privateKey, publicKey, err = generateKeys(cfg.Algorithm)
		if err != nil {
			return nil, fmt.Errorf("generate keys: %w", err)
		}
		keyID = "admin-key-dev"
	}

	return &AdminJWTSigner{
		privateKey: privateKey,
		publicKey:  publicKey,
		keyID:      keyID,
		algorithm:  cfg.Algorithm,
		issuer:     cfg.Issuer,
		audience:   cfg.Audience,
		accessTTL:  cfg.AccessTTL,
		refreshTTL: cfg.RefreshTTL,
		clock:      cfg.Clock,
	}, nil
}

// SignAccessToken signs an access token with user information.
func (s *AdminJWTSigner) SignAccessToken(userID uuid.UUID, email, name string) (string, time.Time, error) {
	now := s.clock.Now()
	expiresAt := now.Add(s.accessTTL)
	claims := AdminClaims{
		UserID: userID,
		Email:  email,
		Name:   name,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    s.issuer,
			Audience:  jwt.ClaimStrings{s.audience},
			ID:        uuid.New().String(),
		},
	}

	token := jwt.NewWithClaims(s.getSigningMethod(), claims)
	token.Header["kid"] = s.keyID

	signed, err := token.SignedString(s.privateKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// SignRefreshToken signs a refresh token.
func (s *AdminJWTSigner) SignRefreshToken(userID uuid.UUID) (string, time.Time, error) {
	now := s.clock.Now()
	expiresAt := now.Add(s.refreshTTL)
	claims := AdminClaims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    s.issuer,
			Audience:  jwt.ClaimStrings{s.audience},
			ID:        uuid.New().String(),
		},
	}

	token := jwt.NewWithClaims(s.getSigningMethod(), claims)
	token.Header["kid"] = s.keyID

	signed, err := token.SignedString(s.privateKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign refresh token: %w", err)
	}
	return signed, expiresAt, nil
}

// ParseAccessToken parses and validates an access token.
func (s *AdminJWTSigner) ParseAccessToken(tokenString string) (*AdminClaims, error) {
	return s.parseToken(tokenString, "access")
}

// ParseRefreshToken parses and validates a refresh token.
func (s *AdminJWTSigner) ParseRefreshToken(tokenString string) (*AdminClaims, error) {
	return s.parseToken(tokenString, "refresh")
}

// GetJWKS returns the JWKS (JSON Web Key Set) for public key distribution.
func (s *AdminJWTSigner) GetJWKS() (jwk.Set, error) {
	key, err := jwk.FromRaw(s.publicKey)
	if err != nil {
		return nil, fmt.Errorf("create JWK from public key: %w", err)
	}

	if err := key.Set(jwk.KeyIDKey, s.keyID); err != nil {
		return nil, fmt.Errorf("set kid: %w", err)
	}
	if err := key.Set(jwk.AlgorithmKey, s.algorithm); err != nil {
		return nil, fmt.Errorf("set alg: %w", err)
	}
	if err := key.Set(jwk.KeyUsageKey, "sig"); err != nil {
		return nil, fmt.Errorf("set use: %w", err)
	}

	set := jwk.NewSet()
	if err := set.AddKey(key); err != nil {
		return nil, fmt.Errorf("add key to set: %w", err)
	}
	return set, nil
}

// AccessTTL returns the access token TTL.
func (s *AdminJWTSigner) AccessTTL() time.Duration {
	return s.accessTTL
}

// RefreshTTL returns the refresh token TTL.
func (s *AdminJWTSigner) RefreshTTL() time.Duration {
	return s.refreshTTL
}

// parseToken is a helper to parse and validate tokens.
func (s *AdminJWTSigner) parseToken(tokenString string, tokenType string) (*AdminClaims, error) {
	if tokenString == "" {
		return nil, errors.New("token is empty")
	}

	token, err := jwt.ParseWithClaims(tokenString, &AdminClaims{}, func(t *jwt.Token) (any, error) {
		// Validate signing method
		if t.Method.Alg() != s.algorithm {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.publicKey, nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse %s token: %w", tokenType, err)
	}

	claims, ok := token.Claims.(*AdminClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid %s token", tokenType)
	}

	// Validate issuer
	if claims.Issuer != s.issuer {
		return nil, fmt.Errorf("invalid issuer: %s", claims.Issuer)
	}

	// Validate audience
	if !containsAudience(claims.Audience, s.audience) {
		return nil, fmt.Errorf("invalid audience: %v", claims.Audience)
	}

	return claims, nil
}

// getSigningMethod returns the JWT signing method based on algorithm.
func (s *AdminJWTSigner) getSigningMethod() jwt.SigningMethod {
	switch s.algorithm {
	case "RS256":
		return jwt.SigningMethodRS256
	case "RS384":
		return jwt.SigningMethodRS384
	case "RS512":
		return jwt.SigningMethodRS512
	case "ES256":
		return jwt.SigningMethodES256
	case "ES384":
		return jwt.SigningMethodES384
	case "ES512":
		return jwt.SigningMethodES512
	default:
		return jwt.SigningMethodRS256
	}
}

// containsAudience checks if the audience claim contains the expected audience.
func containsAudience(audience jwt.ClaimStrings, expected string) bool {
	for _, aud := range audience {
		if aud == expected {
			return true
		}
	}
	return false
}

// loadKeys loads private and public keys from PEM files.
func loadKeys(privateKeyPath, publicKeyPath, algorithm string) (crypto.PrivateKey, crypto.PublicKey, error) {
	privKeyPEM, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read private key: %w", err)
	}

	pubKeyPEM, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read public key: %w", err)
	}

	privBlock, _ := pem.Decode(privKeyPEM)
	if privBlock == nil {
		return nil, nil, errors.New("failed to decode private key PEM")
	}

	pubBlock, _ := pem.Decode(pubKeyPEM)
	if pubBlock == nil {
		return nil, nil, errors.New("failed to decode public key PEM")
	}

	return parseKeyPair(privBlock, pubBlock, algorithm)
}

// parseKeyPair parses PEM-encoded private and public keys, validating that they
// match the expected algorithm family (RSA or ECDSA).
func parseKeyPair(privBlock, pubBlock *pem.Block, algorithm string) (crypto.PrivateKey, crypto.PublicKey, error) {
	privKey, err := x509.ParsePKCS8PrivateKey(privBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse private key: %w", err)
	}

	pubKey, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse public key: %w", err)
	}

	switch algorithm {
	case "RS256", "RS384", "RS512":
		rsaPriv, ok := privKey.(*rsa.PrivateKey)
		if !ok {
			return nil, nil, errors.New("private key is not RSA")
		}
		rsaPub, ok := pubKey.(*rsa.PublicKey)
		if !ok {
			return nil, nil, errors.New("public key is not RSA")
		}
		return rsaPriv, rsaPub, nil

	case "ES256", "ES384", "ES512":
		ecPriv, ok := privKey.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, errors.New("private key is not ECDSA")
		}
		ecPub, ok := pubKey.(*ecdsa.PublicKey)
		if !ok {
			return nil, nil, errors.New("public key is not ECDSA")
		}
		return ecPriv, ecPub, nil

	default:
		return nil, nil, fmt.Errorf("unsupported algorithm: %s", algorithm)
	}
}

// generateKeys generates ephemeral keys for development.
func generateKeys(algorithm string) (crypto.PrivateKey, crypto.PublicKey, error) {
	switch algorithm {
	case "RS256", "RS384", "RS512":
		privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, nil, fmt.Errorf("generate RSA key: %w", err)
		}
		return privateKey, &privateKey.PublicKey, nil

	case "ES256":
		privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, fmt.Errorf("generate ECDSA P256 key: %w", err)
		}
		return privateKey, &privateKey.PublicKey, nil

	case "ES384":
		privateKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			return nil, nil, fmt.Errorf("generate ECDSA P384 key: %w", err)
		}
		return privateKey, &privateKey.PublicKey, nil

	case "ES512":
		privateKey, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
		if err != nil {
			return nil, nil, fmt.Errorf("generate ECDSA P521 key: %w", err)
		}
		return privateKey, &privateKey.PublicKey, nil

	default:
		return nil, nil, fmt.Errorf("unsupported algorithm: %s", algorithm)
	}
}
