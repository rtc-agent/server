package auth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAdminJWTSigner(t *testing.T) {
	tests := []struct {
		name    string
		cfg     AdminJWTConfig
		wantErr bool
	}{
		{
			name: "valid RS256 config",
			cfg: AdminJWTConfig{
				Algorithm:  "RS256",
				Issuer:     "test-issuer",
				Audience:   "test-audience",
				AccessTTL:  time.Hour,
				RefreshTTL: 7 * 24 * time.Hour,
			},
			wantErr: false,
		},
		{
			name: "valid ES256 config",
			cfg: AdminJWTConfig{
				Algorithm:  "ES256",
				Issuer:     "test-issuer",
				Audience:   "test-audience",
				AccessTTL:  time.Hour,
				RefreshTTL: 7 * 24 * time.Hour,
			},
			wantErr: false,
		},
		{
			name: "default values",
			cfg: AdminJWTConfig{
				Issuer:   "test-issuer",
				Audience: "test-audience",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signer, err := NewAdminJWTSigner(tt.cfg)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, signer)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, signer)
			}
		})
	}
}

func TestAdminJWTSigner_SignAndParseAccessToken(t *testing.T) {
	signer, err := NewAdminJWTSigner(AdminJWTConfig{
		Algorithm:  "RS256",
		Issuer:     "test-issuer",
		Audience:   "test-audience",
		AccessTTL:  time.Hour,
		RefreshTTL: 7 * 24 * time.Hour,
	})
	require.NoError(t, err)

	userID := uuid.New()
	email := "test@example.com"
	name := "Test User"

	// Sign access token
	token, expiresAt, err := signer.SignAccessToken(userID, email, name)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.False(t, expiresAt.IsZero())

	// Parse access token
	claims, err := signer.ParseAccessToken(token)
	require.NoError(t, err)
	assert.Equal(t, userID, claims.UserID)
	assert.Equal(t, email, claims.Email)
	assert.Equal(t, name, claims.Name)
	assert.Equal(t, "test-issuer", claims.Issuer)
	assert.Contains(t, []string(claims.Audience), "test-audience")
}

func TestAdminJWTSigner_SignAndParseRefreshToken(t *testing.T) {
	signer, err := NewAdminJWTSigner(AdminJWTConfig{
		Algorithm:  "RS256",
		Issuer:     "test-issuer",
		Audience:   "test-audience",
		AccessTTL:  time.Hour,
		RefreshTTL: 7 * 24 * time.Hour,
	})
	require.NoError(t, err)

	userID := uuid.New()

	// Sign refresh token
	token, expiresAt, err := signer.SignRefreshToken(userID)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.False(t, expiresAt.IsZero())

	// Parse refresh token
	claims, err := signer.ParseRefreshToken(token)
	require.NoError(t, err)
	assert.Equal(t, userID, claims.UserID)
	assert.Equal(t, "test-issuer", claims.Issuer)
	assert.Contains(t, []string(claims.Audience), "test-audience")
}

func TestAdminJWTSigner_ParseAccessToken_Invalid(t *testing.T) {
	signer, err := NewAdminJWTSigner(AdminJWTConfig{
		Algorithm:  "RS256",
		Issuer:     "test-issuer",
		Audience:   "test-audience",
		AccessTTL:  time.Hour,
		RefreshTTL: 7 * 24 * time.Hour,
	})
	require.NoError(t, err)

	tests := []struct {
		name  string
		token string
	}{
		{
			name:  "empty token",
			token: "",
		},
		{
			name:  "invalid token",
			token: "invalid.token.here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, err := signer.ParseAccessToken(tt.token)
			assert.Error(t, err)
			assert.Nil(t, claims)
		})
	}
}

func TestAdminJWTSigner_GetJWKS(t *testing.T) {
	signer, err := NewAdminJWTSigner(AdminJWTConfig{
		Algorithm:  "RS256",
		Issuer:     "test-issuer",
		Audience:   "test-audience",
		AccessTTL:  time.Hour,
		RefreshTTL: 7 * 24 * time.Hour,
	})
	require.NoError(t, err)

	jwks, err := signer.GetJWKS()
	require.NoError(t, err)
	assert.NotNil(t, jwks)

	// Verify JWKS contains at least one key
	keyCount := 0
	for iter := jwks.Keys(context.Background()); iter.Next(context.Background()); {
		keyCount++
	}
	assert.Greater(t, keyCount, 0)
}

func TestAdminJWTSigner_ExpiredToken(t *testing.T) {
	signer, err := NewAdminJWTSigner(AdminJWTConfig{
		Algorithm:  "RS256",
		Issuer:     "test-issuer",
		Audience:   "test-audience",
		AccessTTL:  time.Hour,
		RefreshTTL: 7 * 24 * time.Hour,
	})
	require.NoError(t, err)

	userID := uuid.New()
	token, _, err := signer.SignAccessToken(userID, "test@example.com", "Test")
	require.NoError(t, err)

	// Parse should succeed initially
	claims, err := signer.ParseAccessToken(token)
	require.NoError(t, err)
	assert.NotNil(t, claims)

	// Now create a signer with very short TTL and wait for expiration
	signer2, err := NewAdminJWTSigner(AdminJWTConfig{
		Algorithm:  "RS256",
		Issuer:     "test-issuer",
		Audience:   "test-audience",
		AccessTTL:  time.Millisecond,
		RefreshTTL: 7 * 24 * time.Hour,
	})
	require.NoError(t, err)

	token2, _, err := signer2.SignAccessToken(userID, "test@example.com", "Test")
	require.NoError(t, err)

	// Wait for token to expire
	time.Sleep(10 * time.Millisecond)

	// Parse should fail due to expiration
	claims2, err := signer2.ParseAccessToken(token2)
	assert.Error(t, err)
	assert.Nil(t, claims2)
}
