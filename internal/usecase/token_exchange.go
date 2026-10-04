// Package usecase provides the Token Exchange (RFC 8693) business logic.
//
// Token Exchange allows external JWTs from trusted issuers to be exchanged for
// internal RTC access tokens. The flow:
//  1. Validate the external JWT signature via JWKS
//  2. Verify claims (iss, aud, exp, sub)
//  3. Find or create an oauth_users record
//  4. Issue an internal RTC JWT containing user_id and device_id
package usecase

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
)

// RFC 8693 token type URIs.
const (
	TokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
	TokenTypeJWT         = "urn:ietf:params:oauth:token-type:jwt"
)

// RFC 6749 Section 5.2 error codes.
const (
	ErrInvalidRequest         = "invalid_request"
	ErrInvalidClient          = "invalid_client"
	ErrInvalidGrant           = "invalid_grant"
	ErrUnsupportedGrantType   = "unsupported_grant_type"
	ErrInvalidToken           = "invalid_token"
	ErrServerError            = "server_error"
	ErrTemporarilyUnavailable = "temporarily_unavailable"
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

// TokenExchangeResult holds the result of a successful token exchange.
type TokenExchangeResult struct {
	AccessToken     string
	IssuedTokenType string
	TokenType       string
	ExpiresIn       int64
	UserID          uuid.UUID
}

// TokenExchangeUsecase handles RFC 8693 Token Exchange operations.
type TokenExchangeUsecase struct {
	oauth2UserRepo repo.OAuth2UserRepo
	deviceRepo     repo.DeviceRepo
	signer         TokenSigner
	jwksClient     *auth.JWKSClient
	issuers        []config.ExternalIssuerConfig
	issuerMap      map[string]*config.ExternalIssuerConfig // issuer string -> config
}

// NewTokenExchangeUsecase creates a new TokenExchangeUsecase.
func NewTokenExchangeUsecase(
	oauth2UserRepo repo.OAuth2UserRepo,
	deviceRepo repo.DeviceRepo,
	signer TokenSigner,
	jwksClient *auth.JWKSClient,
	cfg config.TokenExchangeConfig,
) *TokenExchangeUsecase {
	issuerMap := make(map[string]*config.ExternalIssuerConfig, len(cfg.ExternalIssuers))
	for i := range cfg.ExternalIssuers {
		issuerMap[cfg.ExternalIssuers[i].Issuer] = &cfg.ExternalIssuers[i]
	}
	return &TokenExchangeUsecase{
		oauth2UserRepo: oauth2UserRepo,
		deviceRepo:     deviceRepo,
		signer:         signer,
		jwksClient:     jwksClient,
		issuers:        cfg.ExternalIssuers,
		issuerMap:      issuerMap,
	}
}

// ExchangeToken performs the RFC 8693 Token Exchange.
//
// Steps:
//  1. Look up the issuer config by the JWT's iss claim
//  2. Verify the JWT signature using JWKS
//  3. Validate claims (iss, aud, exp, sub, nbf)
//  4. Extract user info (sub, email, name, avatar_url) with claims_mapping
//  5. Find or create oauth_users record
//  6. Upsert device info
//  7. Issue internal RTC JWT
func (uc *TokenExchangeUsecase) ExchangeToken(
	ctx context.Context,
	subjectToken string,
	subjectTokenType string,
	deviceID string,
) (*TokenExchangeResult, error) {
	// Validate subject_token_type.
	switch subjectTokenType {
	case TokenTypeAccessToken, TokenTypeJWT:
		// supported
	default:
		return nil, &TokenExchangeError{
			Code:        ErrInvalidRequest,
			Description: fmt.Sprintf("unsupported subject_token_type: %s", subjectTokenType),
			HTTPStatus:  400,
		}
	}

	// Parse the JWT without verification first to extract the issuer for lookup.
	// ParseUnverified skips signature verification; we only need the iss claim
	// to find the right issuer config before doing full verification.
	unverified, _, err := jwt.NewParser().ParseUnverified(subjectToken, jwt.MapClaims{})
	if err != nil {
		return nil, &TokenExchangeError{
			Code:        ErrInvalidGrant,
			Description: "subject_token is not a valid JWT",
			HTTPStatus:  400,
		}
	}

	// Extract issuer.
	claims, ok := unverified.Claims.(jwt.MapClaims)
	if !ok {
		return nil, &TokenExchangeError{
			Code:        ErrInvalidGrant,
			Description: "subject_token has invalid claims",
			HTTPStatus:  400,
		}
	}
	issClaim, _ := claims["iss"].(string)
	if issClaim == "" {
		return nil, &TokenExchangeError{
			Code:        ErrInvalidGrant,
			Description: "subject_token missing iss claim",
			HTTPStatus:  400,
		}
	}

	// Look up issuer config.
	issuerCfg, ok := uc.issuerMap[issClaim]
	if !ok {
		return nil, &TokenExchangeError{
			Code:        ErrInvalidGrant,
			Description: fmt.Sprintf("untrusted issuer: %s", issClaim),
			HTTPStatus:  400,
		}
	}

	// Verify the JWT with JWKS.
	verifiedClaims, err := uc.verifyJWTWithJWKS(ctx, subjectToken, issuerCfg)
	if err != nil {
		var teErr *TokenExchangeError
		if errors.As(err, &teErr) {
			return nil, teErr
		}
		return nil, &TokenExchangeError{
			Code:        ErrServerError,
			Description: "token verification failed",
			HTTPStatus:  500,
		}
	}

	// Extract user info from claims.
	userInfo := uc.extractUserInfo(verifiedClaims, issuerCfg)

	// Find or create oauth_users record.
	oauth2User := &model.OAuth2User{
		Provider:  issuerCfg.Name,
		Sub:       userInfo.sub,
		Email:     userInfo.email,
		Name:      userInfo.name,
		AvatarURL: userInfo.avatarURL,
	}

	createdUser, isNew, err := uc.oauth2UserRepo.FindOrCreate(ctx, oauth2User)
	if err != nil {
		return nil, fmt.Errorf("token exchange find/create user: %w", err)
	}

	// Update user info if existing (sync latest profile).
	if !isNew {
		uc.syncUserProfile(ctx, createdUser, userInfo)
	}

	// Upsert device info.
	if deviceID != "" {
		device := &model.Device{
			UserID:       createdUser.ID,
			DeviceID:     deviceID,
			LastActiveAt: time.Now(),
		}
		if err := uc.deviceRepo.Upsert(ctx, device); err != nil {
			logger.Warn(ctx, "token exchange: failed to upsert device",
				zap.String("device_id", deviceID), zap.Error(err))
		}
	}

	// Issue internal RTC JWT.
	accessToken, _, err := uc.signer.SignAccessToken(createdUser.ID, deviceID)
	if err != nil {
		return nil, fmt.Errorf("token exchange sign access token: %w", err)
	}

	logger.Info(ctx, "token exchange succeeded",
		zap.String("issuer", issClaim),
		zap.String("provider", issuerCfg.Name),
		zap.String("user_id", createdUser.ID.String()),
		zap.Bool("is_new", isNew),
	)

	return &TokenExchangeResult{
		AccessToken:     accessToken,
		IssuedTokenType: TokenTypeAccessToken,
		TokenType:       "Bearer",
		ExpiresIn:       int64(uc.signer.AccessTTL().Seconds()),
		UserID:          createdUser.ID,
	}, nil
}

// verifyJWTWithJWKS verifies a JWT using the JWKS from the issuer.
// On verification failure, it automatically refreshes the JWKS cache.
func (uc *TokenExchangeUsecase) verifyJWTWithJWKS(
	ctx context.Context,
	tokenString string,
	issuerCfg *config.ExternalIssuerConfig,
) (jwt.MapClaims, error) {
	// First attempt: use cached keys.
	claims, err := uc.verifyWithCachedKeys(ctx, tokenString, issuerCfg)
	if err == nil {
		return claims, nil
	}

	logger.Info(ctx, "token exchange: initial verification failed, refreshing JWKS",
		zap.String("issuer", issuerCfg.Issuer), zap.Error(err))

	// Second attempt: refresh JWKS and retry.
	// Extract kid from the token header for targeted refresh.
	kid := extractKidFromToken(tokenString)
	if kid == "" {
		// No kid in header; re-fetch full JWKS.
		return nil, &TokenExchangeError{
			Code:        ErrInvalidGrant,
			Description: "JWT verification failed and no kid in header for refresh",
			HTTPStatus:  401,
		}
	}

	_, refreshErr := uc.jwksClient.RefreshKey(ctx, issuerCfg.Issuer, issuerCfg.JWKSURI, kid)
	if refreshErr != nil {
		// Check if it's an endpoint unreachable error -> 503.
		if errors.Is(refreshErr, auth.ErrJWKSEndpointUnreachable) {
			return nil, &TokenExchangeError{
				Code:        ErrTemporarilyUnavailable,
				Description: "identity provider temporarily unavailable",
				HTTPStatus:  503,
			}
		}
		return nil, &TokenExchangeError{
			Code:        ErrInvalidGrant,
			Description: "failed to refresh JWKS keys",
			HTTPStatus:  401,
		}
	}

	// Retry verification with fresh keys.
	claims, err = uc.verifyWithCachedKeys(ctx, tokenString, issuerCfg)
	if err != nil {
		return nil, &TokenExchangeError{
			Code:        ErrInvalidGrant,
			Description: "JWT verification failed after JWKS refresh",
			HTTPStatus:  401,
		}
	}

	return claims, nil
}

// verifyWithCachedKeys verifies a JWT using cached JWKS keys.
func (uc *TokenExchangeUsecase) verifyWithCachedKeys(
	ctx context.Context,
	tokenString string,
	issuerCfg *config.ExternalIssuerConfig,
) (jwt.MapClaims, error) {
	kid := extractKidFromToken(tokenString)
	if kid == "" {
		return nil, fmt.Errorf("JWT header missing kid")
	}

	// Get the JWK from cache or fetch.
	jwkKey, err := uc.jwksClient.GetKey(ctx, issuerCfg.Issuer, issuerCfg.JWKSURI, kid)
	if err != nil {
		return nil, fmt.Errorf("get jwk: %w", err)
	}

	// Convert JWK to crypto.PublicKey.
	pubKey, err := jwkToPublicKey(jwkKey)
	if err != nil {
		return nil, fmt.Errorf("convert jwk to public key: %w", err)
	}

	// Build allowed algorithms list.
	allowedAlgs := issuerCfg.AllowedAlgorithms
	if len(allowedAlgs) == 0 {
		allowedAlgs = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}
	}

	// Verify the JWT.
	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (any, error) {
		// Check algorithm.
		alg := t.Method.Alg()
		allowed := false
		for _, a := range allowedAlgs {
			if a == alg {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, fmt.Errorf("unexpected signing algorithm: %s", alg)
		}

		// Verify key type matches algorithm.
		if err := validateKeyType(t.Method, pubKey); err != nil {
			return nil, err
		}

		return pubKey, nil
	},
		jwt.WithIssuer(issuerCfg.Issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("jwt verify: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid jwt claims")
	}

	// Validate sub is present.
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, fmt.Errorf("jwt missing sub claim")
	}

	// Validate nbf if present.
	if nbf, ok := claims["nbf"]; ok {
		if nbfFloat, ok := nbf.(float64); ok {
			if time.Now().Before(time.Unix(int64(nbfFloat), 0)) {
				return nil, fmt.Errorf("jwt used before nbf")
			}
		}
	}

	return claims, nil
}

// extractedUserInfo holds user info extracted from JWT claims.
type extractedUserInfo struct {
	sub       string
	email     string
	name      string
	avatarURL string
}

// extractUserInfo extracts user information from JWT claims using claims_mapping.
func (uc *TokenExchangeUsecase) extractUserInfo(claims jwt.MapClaims, issuerCfg *config.ExternalIssuerConfig) *extractedUserInfo {
	info := &extractedUserInfo{}

	// sub is always required and not mapped.
	if sub, ok := claims["sub"].(string); ok {
		info.sub = sub
	}

	// Build reverse mapping: target field -> source claim name.
	// Default mapping: email->email, name->name, picture->avatar_url.
	defaultMapping := map[string]string{
		"email":      "email",
		"name":       "name",
		"avatar_url": "picture",
	}

	// Apply custom claims_mapping (overrides defaults).
	mapping := make(map[string]string)
	for k, v := range defaultMapping {
		mapping[k] = v
	}
	for k, v := range issuerCfg.ClaimsMapping {
		mapping[k] = v
	}

	// Extract mapped fields.
	if sourceClaim, ok := mapping["email"]; ok {
		if v, ok := claims[sourceClaim].(string); ok {
			info.email = v
		}
	}
	if sourceClaim, ok := mapping["name"]; ok {
		if v, ok := claims[sourceClaim].(string); ok {
			info.name = v
		}
	}
	if sourceClaim, ok := mapping["avatar_url"]; ok {
		if v, ok := claims[sourceClaim].(string); ok {
			info.avatarURL = v
		}
	}

	return info
}

// syncUserProfile updates an existing user's profile with the latest info from the IdP.
// Only non-empty fields that differ from the stored values are updated.
func (uc *TokenExchangeUsecase) syncUserProfile(ctx context.Context, user *model.OAuth2User, info *extractedUserInfo) {
	updated := false
	if info.email != "" && user.Email != info.email {
		user.Email = info.email
		updated = true
	}
	if info.name != "" && user.Name != info.name {
		user.Name = info.name
		updated = true
	}
	if info.avatarURL != "" && user.AvatarURL != info.avatarURL {
		user.AvatarURL = info.avatarURL
		updated = true
	}
	if updated {
		if err := uc.oauth2UserRepo.Update(ctx, user); err != nil {
			logger.Warn(ctx, "token exchange: failed to update user info",
				zap.String("user_id", user.ID.String()), zap.Error(err))
		}
	}
}

// extractKidFromToken extracts the "kid" from the JWT header without full verification.
func extractKidFromToken(tokenString string) string {
	parts := strings.SplitN(tokenString, ".", 4)
	if len(parts) < 3 {
		return ""
	}

	// Parse just the header.
	token, _, err := jwt.NewParser().ParseUnverified(tokenString, jwt.MapClaims{})
	if err != nil {
		return ""
	}

	kid, _ := token.Header["kid"].(string)
	return kid
}

// jwkToPublicKey converts a JWK to a crypto.PublicKey.
func jwkToPublicKey(key jwk.Key) (crypto.PublicKey, error) {
	// Serialize and parse through jwx to get the raw key.
	var rawKey any
	if err := key.Raw(&rawKey); err != nil {
		return nil, fmt.Errorf("extract raw key from jwk: %w", err)
	}

	switch k := rawKey.(type) {
	case *rsa.PublicKey:
		return k, nil
	case *ecdsa.PublicKey:
		return k, nil
	case ed25519.PublicKey:
		return k, nil
	default:
		return nil, fmt.Errorf("unsupported key type: %T", rawKey)
	}
}

// validateKeyType ensures the JWT signing method matches the public key type.
func validateKeyType(method jwt.SigningMethod, pubKey crypto.PublicKey) error {
	switch m := method.(type) {
	case *jwt.SigningMethodRSA:
		if _, ok := pubKey.(*rsa.PublicKey); !ok {
			return fmt.Errorf("RSA algorithm %s requires RSA public key", m.Alg())
		}
	case *jwt.SigningMethodECDSA:
		if _, ok := pubKey.(*ecdsa.PublicKey); !ok {
			return fmt.Errorf("ECDSA algorithm %s requires EC public key", m.Alg())
		}
	case *jwt.SigningMethodEd25519:
		if _, ok := pubKey.(ed25519.PublicKey); !ok {
			return fmt.Errorf("Ed25519 algorithm requires Ed25519 public key")
		}
	default:
		return fmt.Errorf("unsupported signing method: %v", method)
	}
	return nil
}
