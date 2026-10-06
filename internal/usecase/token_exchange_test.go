package usecase_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
)

// --- Mock dependencies ---

type mockOAuth2UserRepo struct {
	users   map[string]*model.OAuth2User // key = provider + ":" + sub
	createF func(ctx context.Context, user *model.OAuth2User) error
}

func newMockOAuth2UserRepo() *mockOAuth2UserRepo {
	return &mockOAuth2UserRepo{users: make(map[string]*model.OAuth2User)}
}

func (m *mockOAuth2UserRepo) Create(ctx context.Context, user *model.OAuth2User) error {
	if m.createF != nil {
		return m.createF(ctx, user)
	}
	key := user.Provider + ":" + user.Sub
	if _, exists := m.users[key]; exists {
		return repo.ErrAlreadyExists
	}
	if user.ID == uuid.Nil {
		user.ID = uuid.Must(uuid.NewV7())
	}
	m.users[key] = user
	return nil
}

func (m *mockOAuth2UserRepo) FindByID(ctx context.Context, id uuid.UUID) (*model.OAuth2User, error) {
	for _, u := range m.users {
		if u.ID == id {
			return u, nil
		}
	}
	return nil, repo.ErrOAuth2UserNotFound
}

func (m *mockOAuth2UserRepo) FindByProvider(ctx context.Context, provider, sub string) (*model.OAuth2User, error) {
	key := provider + ":" + sub
	u, ok := m.users[key]
	if !ok {
		return nil, repo.ErrOAuth2UserNotFound
	}
	return u, nil
}

func (m *mockOAuth2UserRepo) FindOrCreate(ctx context.Context, user *model.OAuth2User) (*model.OAuth2User, bool, error) {
	key := user.Provider + ":" + user.Sub
	if u, ok := m.users[key]; ok {
		return u, false, nil
	}
	if user.ID == uuid.Nil {
		user.ID = uuid.Must(uuid.NewV7())
	}
	m.users[key] = user
	return user, true, nil
}

func (m *mockOAuth2UserRepo) Update(ctx context.Context, user *model.OAuth2User) error {
	key := user.Provider + ":" + user.Sub
	m.users[key] = user
	return nil
}

func (m *mockOAuth2UserRepo) ListWithFilters(ctx context.Context, filter repo.OAuth2UserFilter) ([]*model.OAuth2User, int64, error) {
	// Simple implementation for tests
	users := make([]*model.OAuth2User, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, u)
	}
	return users, int64(len(users)), nil
}

func (m *mockOAuth2UserRepo) IsUserBanned(ctx context.Context, userID uuid.UUID) (bool, error) {
	for _, u := range m.users {
		if u.ID == userID {
			return u.BannedAt != nil, nil
		}
	}
	return false, nil // User not found, not banned
}

type mockDeviceRepo struct{}

func (m *mockDeviceRepo) Upsert(ctx context.Context, device *model.Device) error {
	return nil
}

func (m *mockDeviceRepo) FindByUserAndDeviceID(ctx context.Context, userID uuid.UUID, deviceID string) (*model.Device, error) {
	return nil, repo.ErrDeviceNotFound
}

type mockTokenSigner struct {
	accessTTL time.Duration
}

func (m *mockTokenSigner) SignAccessToken(userID uuid.UUID, deviceID string) (string, time.Time, error) {
	return "mock-access-token", time.Now().Add(m.accessTTL), nil
}

func (m *mockTokenSigner) AccessTTL() time.Duration {
	return m.accessTTL
}

// --- Helper: generate a signed RSA JWT ---

func generateTestJWT(t *testing.T, claims jwt.MapClaims, privKey *rsa.PrivateKey, kid string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(privKey)
	require.NoError(t, err)
	return signed
}

func generateRSAPrivateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return privKey
}

func TestTokenExchangeUsecase_ExchangeToken_UnsupportedTokenType(t *testing.T) {
	uc := usecase.NewTokenExchangeUsecase(
		newMockOAuth2UserRepo(),
		&mockDeviceRepo{},
		&mockTokenSigner{accessTTL: time.Hour},
		nil, // JWKS not needed for this test.
		config.TokenExchangeConfig{},
	)

	_, err := uc.ExchangeToken(context.Background(), "token", "unsupported_type", "device-1")
	require.Error(t, err)
	var teErr *usecase.TokenExchangeError
	require.ErrorAs(t, err, &teErr)
	assert.Equal(t, usecase.ErrInvalidRequest, teErr.Code)
	assert.Equal(t, 400, teErr.HTTPStatus)
}

func TestTokenExchangeUsecase_ExchangeToken_InvalidJWT(t *testing.T) {
	uc := usecase.NewTokenExchangeUsecase(
		newMockOAuth2UserRepo(),
		&mockDeviceRepo{},
		&mockTokenSigner{accessTTL: time.Hour},
		nil,
		config.TokenExchangeConfig{
			ExternalIssuers: []config.ExternalIssuerConfig{
				{
					Name:    "test",
					Issuer:  "https://issuer.example.com",
					JWKSURI: "https://issuer.example.com/.well-known/jwks.json",
				},
			},
		},
	)

	_, err := uc.ExchangeToken(context.Background(), "not-a-jwt", usecase.TokenTypeJWT, "device-1")
	require.Error(t, err)
	var teErr *usecase.TokenExchangeError
	require.ErrorAs(t, err, &teErr)
	assert.Equal(t, usecase.ErrInvalidGrant, teErr.Code)
}

func TestTokenExchangeUsecase_ExchangeToken_UntrustedIssuer(t *testing.T) {
	privKey := generateRSAPrivateKey(t)

	claims := jwt.MapClaims{
		"iss": "https://untrusted-issuer.example.com",
		"sub": "user-123",
		"exp": jwt.NewNumericDate(time.Now().Add(time.Hour)),
		"iat": jwt.NewNumericDate(time.Now()),
	}
	tokenStr := generateTestJWT(t, claims, privKey, "key-1")

	uc := usecase.NewTokenExchangeUsecase(
		newMockOAuth2UserRepo(),
		&mockDeviceRepo{},
		&mockTokenSigner{accessTTL: time.Hour},
		nil,
		config.TokenExchangeConfig{
			ExternalIssuers: []config.ExternalIssuerConfig{
				{
					Name:    "trusted",
					Issuer:  "https://trusted.example.com",
					JWKSURI: "https://trusted.example.com/.well-known/jwks.json",
				},
			},
		},
	)

	_, err := uc.ExchangeToken(context.Background(), tokenStr, usecase.TokenTypeJWT, "device-1")
	require.Error(t, err)
	var teErr *usecase.TokenExchangeError
	require.ErrorAs(t, err, &teErr)
	assert.Equal(t, usecase.ErrInvalidGrant, teErr.Code)
	assert.Contains(t, teErr.Description, "untrusted issuer")
}

func TestTokenExchangeError_Error(t *testing.T) {
	err := &usecase.TokenExchangeError{
		Code:        "invalid_grant",
		Description: "token expired",
	}
	assert.Equal(t, "invalid_grant: token expired", err.Error())

	err2 := &usecase.TokenExchangeError{
		Code: "invalid_request",
	}
	assert.Equal(t, "invalid_request", err2.Error())
}

func TestTokenExchangeUsecase_ExtractUserInfo(t *testing.T) {
	uc := usecase.NewTokenExchangeUsecase(
		newMockOAuth2UserRepo(),
		&mockDeviceRepo{},
		&mockTokenSigner{accessTTL: time.Hour},
		nil,
		config.TokenExchangeConfig{},
	)

	// Test via ExchangeToken that missing sub results in error.
	claims := jwt.MapClaims{
		"iss": "https://issuer.example.com",
		"sub": "",
		"exp": jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}

	privKey := generateRSAPrivateKey(t)
	tokenStr := generateTestJWT(t, claims, privKey, "key-1")

	_, err := uc.ExchangeToken(context.Background(), tokenStr, usecase.TokenTypeJWT, "device-1")
	require.Error(t, err)
}
