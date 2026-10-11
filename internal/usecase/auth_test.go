package usecase_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
)

// mockRefreshTokenRepo implements repo.RefreshTokenRepo for testing.
type mockRefreshTokenRepo struct {
	tokens map[string]*model.RefreshToken // key = token hash
	revoke func(ctx context.Context, id uuid.UUID) error
}

func newMockRefreshTokenRepo() *mockRefreshTokenRepo {
	return &mockRefreshTokenRepo{tokens: make(map[string]*model.RefreshToken)}
}

func (m *mockRefreshTokenRepo) Create(ctx context.Context, rt *model.RefreshToken) error {
	if rt.ID == uuid.Nil {
		rt.ID = uuid.Must(uuid.NewV7())
	}
	m.tokens[rt.TokenHash] = rt
	return nil
}

func (m *mockRefreshTokenRepo) FindByHash(ctx context.Context, hash string) (*model.RefreshToken, error) {
	rt, ok := m.tokens[hash]
	if !ok {
		return nil, repo.ErrRefreshTokenNotFound
	}
	return rt, nil
}

func (m *mockRefreshTokenRepo) FindByHashForUpdate(ctx context.Context, hash string) (*model.RefreshToken, error) {
	return m.FindByHash(ctx, hash)
}

func (m *mockRefreshTokenRepo) Revoke(ctx context.Context, id uuid.UUID) error {
	if m.revoke != nil {
		return m.revoke(ctx, id)
	}
	for _, rt := range m.tokens {
		if rt.ID == id {
			rt.Revoked = true
			return nil
		}
	}
	return repo.ErrRefreshTokenNotFound
}

func (m *mockRefreshTokenRepo) RevokeAllByUserID(ctx context.Context, userID uuid.UUID) (int64, error) {
	var count int64
	for _, rt := range m.tokens {
		if rt.UserID == userID && !rt.Revoked {
			rt.Revoked = true
			count++
		}
	}
	return count, nil
}

func (m *mockRefreshTokenRepo) RevokeAllByUserAndDevice(ctx context.Context, userID uuid.UUID, deviceID string) (int64, error) {
	var count int64
	for _, rt := range m.tokens {
		if rt.UserID == userID && rt.DeviceID == deviceID && !rt.Revoked {
			rt.Revoked = true
			count++
		}
	}
	return count, nil
}

func (m *mockRefreshTokenRepo) DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error) {
	var count int64
	for hash, rt := range m.tokens {
		if rt.ExpiresAt.Before(cutoff) {
			delete(m.tokens, hash)
			count++
		}
	}
	return count, nil
}

// mockTokenSigner implements usecase.TokenSigner for testing.
type mockAuthSigner struct {
	accessTTL time.Duration
	signErr   error
}

func (m *mockAuthSigner) SignAccessToken(userID uuid.UUID, deviceID string) (string, time.Time, error) {
	if m.signErr != nil {
		return "", time.Time{}, m.signErr
	}
	return "mock-access-token-" + userID.String(), time.Now().Add(m.accessTTL), nil
}

func (m *mockAuthSigner) AccessTTL() time.Duration {
	return m.accessTTL
}

// newTestAuthUsecase creates an AuthUsecase with mock dependencies for testing.
func newTestAuthUsecase(
	oauth2UserRepo repo.OAuth2UserRepo,
	deviceRepo repo.DeviceRepo,
	refreshTokenRepo repo.RefreshTokenRepo,
	signer usecase.TokenSigner,
	refreshTokenTTL time.Duration,
) *usecase.AuthUsecase {
	// Use nil DB for tests that don't need transactions
	return usecase.NewAuthUsecase(
		nil,
		oauth2UserRepo,
		deviceRepo,
		refreshTokenRepo,
		signer,
		refreshTokenTTL,
	)
}

func TestAuthUsecase_IssueTokenPair(t *testing.T) {
	ctx := context.Background()
	userID := uuid.Must(uuid.NewV7())
	deviceID := "test-device-1"

	tests := []struct {
		name        string
		setupSigner func() *mockAuthSigner
		wantErr     bool
		errContains string
	}{
		{
			name: "success",
			setupSigner: func() *mockAuthSigner {
				return &mockAuthSigner{accessTTL: time.Hour}
			},
			wantErr: false,
		},
		{
			name: "signer error",
			setupSigner: func() *mockAuthSigner {
				return &mockAuthSigner{
					accessTTL: time.Hour,
					signErr:   repo.ErrNotFound,
				}
			},
			wantErr:     true,
			errContains: "sign access token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refreshTokenRepo := newMockRefreshTokenRepo()
			signer := tt.setupSigner()

			uc := newTestAuthUsecase(
				newMockOAuth2UserRepo(),
				&mockDeviceRepo{},
				refreshTokenRepo,
				signer,
				24*time.Hour,
			)

			result, err := uc.IssueTokenPair(ctx, userID, deviceID)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			require.NoError(t, err)
			require.NotNil(t, result)
			assert.NotEmpty(t, result.AccessToken)
			assert.NotEmpty(t, result.RefreshToken)
			assert.Equal(t, int64(3600), result.ExpiresIn)
			assert.Equal(t, userID, result.UserID)

			// Verify refresh token was stored
			assert.Len(t, refreshTokenRepo.tokens, 1)
		})
	}
}

func TestAuthUsecase_ValidateRefreshToken(t *testing.T) {
	ctx := context.Background()
	userID := uuid.Must(uuid.NewV7())
	deviceID := "test-device-1"

	// Create a valid refresh token
	validToken := "rt_valid_token"
	validHash := hashRefreshToken(validToken)
	validRT := &model.RefreshToken{
		ID:        uuid.Must(uuid.NewV7()),
		TokenHash: validHash,
		UserID:    userID,
		DeviceID:  deviceID,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		Revoked:   false,
	}

	// Create a revoked token
	revokedToken := "rt_revoked_token"
	revokedHash := hashRefreshToken(revokedToken)
	revokedRT := &model.RefreshToken{
		ID:        uuid.Must(uuid.NewV7()),
		TokenHash: revokedHash,
		UserID:    userID,
		DeviceID:  deviceID,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		Revoked:   true,
	}

	// Create an expired token
	expiredToken := "rt_expired_token"
	expiredHash := hashRefreshToken(expiredToken)
	expiredRT := &model.RefreshToken{
		ID:        uuid.Must(uuid.NewV7()),
		TokenHash: expiredHash,
		UserID:    userID,
		DeviceID:  deviceID,
		ExpiresAt: time.Now().Add(-1 * time.Hour),
		Revoked:   false,
	}

	tests := []struct {
		name        string
		token       string
		setupRepo   func() *mockRefreshTokenRepo
		setupUser   func() *mockOAuth2UserRepo
		wantErr     bool
		errContains string
		wantInfo    *usecase.RefreshTokenInfo
	}{
		{
			name:  "valid token",
			token: validToken,
			setupRepo: func() *mockRefreshTokenRepo {
				r := newMockRefreshTokenRepo()
				r.tokens[validHash] = validRT
				return r
			},
			setupUser: func() *mockOAuth2UserRepo {
				r := newMockOAuth2UserRepo()
				r.users["test:test"] = &model.OAuth2User{
					ID:       userID,
					Provider: "test",
					Sub:      "test",
				}
				return r
			},
			wantErr: false,
			wantInfo: &usecase.RefreshTokenInfo{
				ID:       validRT.ID,
				UserID:   userID,
				DeviceID: deviceID,
			},
		},
		{
			name:  "invalid token",
			token: "rt_nonexistent",
			setupRepo: func() *mockRefreshTokenRepo {
				return newMockRefreshTokenRepo()
			},
			setupUser:   func() *mockOAuth2UserRepo { return newMockOAuth2UserRepo() },
			wantErr:     true,
			errContains: "invalid",
		},
		{
			name:  "revoked token",
			token: revokedToken,
			setupRepo: func() *mockRefreshTokenRepo {
				r := newMockRefreshTokenRepo()
				r.tokens[revokedHash] = revokedRT
				return r
			},
			setupUser: func() *mockOAuth2UserRepo {
				r := newMockOAuth2UserRepo()
				r.users["test:test"] = &model.OAuth2User{
					ID:       userID,
					Provider: "test",
					Sub:      "test",
				}
				return r
			},
			wantErr:     true,
			errContains: "revoked",
		},
		{
			name:  "expired token",
			token: expiredToken,
			setupRepo: func() *mockRefreshTokenRepo {
				r := newMockRefreshTokenRepo()
				r.tokens[expiredHash] = expiredRT
				return r
			},
			setupUser: func() *mockOAuth2UserRepo {
				r := newMockOAuth2UserRepo()
				r.users["test:test"] = &model.OAuth2User{
					ID:       userID,
					Provider: "test",
					Sub:      "test",
				}
				return r
			},
			wantErr:     true,
			errContains: "expired",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refreshTokenRepo := tt.setupRepo()
			oauth2UserRepo := tt.setupUser()

			uc := newTestAuthUsecase(
				oauth2UserRepo,
				&mockDeviceRepo{},
				refreshTokenRepo,
				&mockAuthSigner{accessTTL: time.Hour},
				24*time.Hour,
			)

			info, err := uc.ValidateRefreshToken(ctx, tt.token)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			require.NoError(t, err)
			require.NotNil(t, info)
			assert.Equal(t, tt.wantInfo.ID, info.ID)
			assert.Equal(t, tt.wantInfo.UserID, info.UserID)
			assert.Equal(t, tt.wantInfo.DeviceID, info.DeviceID)
		})
	}
}

// hashRefreshToken mirrors the internal hashRefreshToken function for test setup.
func hashRefreshToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
