package usecase_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/oauth"
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
		30*time.Second, // default grace period for tests
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

// makeDuplicateKeyError constructs a *pgconn.PgError with PostgreSQL
// unique constraint violation code (23505). Used to test the
// "concurrent create" path in FindOrCreateUser.
func makeDuplicateKeyError() error {
	return &pgconn.PgError{Code: "23505"}
}

// --- mockOAuth2UserRepo helpers ---

// mockOAuth2UserRepoForAuth wraps the shared mockOAuth2UserRepo (defined in
// token_exchange_test.go) with extra behaviour needed by auth tests:
// configurable Create error and Update error.
type mockOAuth2UserRepoForAuth struct {
	users       map[string]*model.OAuth2User // key = provider + ":" + sub
	createErr   error                        // returned from Create (before any side effect)
	updateErr   error                        // returned from Update
	findByIDErr error                        // returned from FindByID
}

func newMockOAuth2UserRepoForAuth() *mockOAuth2UserRepoForAuth {
	return &mockOAuth2UserRepoForAuth{users: make(map[string]*model.OAuth2User)}
}

func (m *mockOAuth2UserRepoForAuth) Create(_ context.Context, user *model.OAuth2User) error {
	if m.createErr != nil {
		return m.createErr
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

func (m *mockOAuth2UserRepoForAuth) FindByID(_ context.Context, id uuid.UUID) (*model.OAuth2User, error) {
	if m.findByIDErr != nil {
		return nil, m.findByIDErr
	}
	for _, u := range m.users {
		if u.ID == id {
			return u, nil
		}
	}
	return nil, repo.ErrOAuth2UserNotFound
}

func (m *mockOAuth2UserRepoForAuth) FindByProvider(_ context.Context, provider, sub string) (*model.OAuth2User, error) {
	key := provider + ":" + sub
	u, ok := m.users[key]
	if !ok {
		return nil, repo.ErrOAuth2UserNotFound
	}
	return u, nil
}

func (m *mockOAuth2UserRepoForAuth) FindOrCreate(_ context.Context, user *model.OAuth2User) (*model.OAuth2User, bool, error) {
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

func (m *mockOAuth2UserRepoForAuth) Update(_ context.Context, user *model.OAuth2User) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	key := user.Provider + ":" + user.Sub
	m.users[key] = user
	return nil
}

func (m *mockOAuth2UserRepoForAuth) ListWithFilters(_ context.Context, _ repo.OAuth2UserFilter) ([]*model.OAuth2User, int64, error) {
	users := make([]*model.OAuth2User, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, u)
	}
	return users, int64(len(users)), nil
}

func (m *mockOAuth2UserRepoForAuth) IsUserBanned(_ context.Context, userID uuid.UUID) (bool, error) {
	for _, u := range m.users {
		if u.ID == userID {
			return u.BannedAt != nil, nil
		}
	}
	return false, nil
}

// --- mockDeviceRepo with recording ---

type mockDeviceRepoForAuth struct {
	upserted []*model.Device
	upsertFn func(*model.Device) error
}

func (m *mockDeviceRepoForAuth) Upsert(_ context.Context, device *model.Device) error {
	m.upserted = append(m.upserted, device)
	if m.upsertFn != nil {
		return m.upsertFn(device)
	}
	return nil
}

func (m *mockDeviceRepoForAuth) FindByUserAndDeviceID(_ context.Context, _ uuid.UUID, _ string) (*model.Device, error) {
	return nil, repo.ErrDeviceNotFound
}

func (m *mockDeviceRepoForAuth) ListByUser(_ context.Context, _ uuid.UUID) ([]*model.Device, error) {
	return nil, nil
}

// ============================================================
// Tests: AuthUsecase.FindOrCreateUser
// ============================================================

func TestAuthUsecase_FindOrCreateUser(t *testing.T) {
	ctx := context.Background()
	provider := "github"
	providerUserID := "user-123"
	info := &oauth.ProviderUserInfo{
		ProviderUserID: providerUserID,
		Username:       "alice",
		Email:          "alice@example.com",
		AvatarURL:      "https://example.com/alice.png",
	}

	t.Run("creates new user when not found", func(t *testing.T) {
		repoMock := newMockOAuth2UserRepoForAuth()
		uc := usecase.NewAuthUsecase(nil, repoMock, &mockDeviceRepoForAuth{}, newMockRefreshTokenRepo(), &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		result, err := uc.FindOrCreateUser(ctx, provider, info)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.True(t, result.IsNew, "expected IsNew=true for first-time user")
		assert.NotEqual(t, uuid.Nil, result.UserID)
		assert.Nil(t, result.BannedAt)
	})

	t.Run("updates existing user when found", func(t *testing.T) {
		repoMock := newMockOAuth2UserRepoForAuth()
		existing := &model.OAuth2User{
			ID:        uuid.Must(uuid.NewV7()),
			Provider:  provider,
			Sub:       providerUserID,
			Name:      "old-name",
			Email:     "old@example.com",
			AvatarURL: "old.png",
		}
		repoMock.users[provider+":"+providerUserID] = existing

		uc := usecase.NewAuthUsecase(nil, repoMock, &mockDeviceRepoForAuth{}, newMockRefreshTokenRepo(), &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		result, err := uc.FindOrCreateUser(ctx, provider, info)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.False(t, result.IsNew, "expected IsNew=false for existing user")
		assert.Equal(t, existing.ID, result.UserID)

		// Verify the user record was updated (best-effort profile sync)
		updated := repoMock.users[provider+":"+providerUserID]
		assert.Equal(t, "alice", updated.Name)
		assert.Equal(t, "alice@example.com", updated.Email)
	})

	t.Run("update failure is best-effort (does not fail op)", func(t *testing.T) {
		repoMock := newMockOAuth2UserRepoForAuth()
		existing := &model.OAuth2User{
			ID:       uuid.Must(uuid.NewV7()),
			Provider: provider,
			Sub:      providerUserID,
			Name:     "old-name",
		}
		repoMock.users[provider+":"+providerUserID] = existing
		repoMock.updateErr = errors.New("db connection lost")

		uc := usecase.NewAuthUsecase(nil, repoMock, &mockDeviceRepoForAuth{}, newMockRefreshTokenRepo(), &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		result, err := uc.FindOrCreateUser(ctx, provider, info)
		require.NoError(t, err, "Update failure must not fail FindOrCreateUser")
		require.NotNil(t, result)
		assert.Equal(t, existing.ID, result.UserID)
		assert.False(t, result.IsNew)
	})

	t.Run("re-fetches user after duplicate key error", func(t *testing.T) {
		// Simulate concurrent-create: first FindByProvider returns NotFound,
		// Create returns duplicate-key error, second FindByProvider returns
		// the concurrently-inserted user.
		original := &model.OAuth2User{
			ID:       uuid.Must(uuid.NewV7()),
			Provider: provider,
			Sub:      providerUserID,
			Name:     "concurrent-user",
		}
		createCallCount := 0

		uc := usecase.NewAuthUsecase(nil, &findOrCreateCustomMock{
			t:           t,
			provider:    provider,
			sub:         providerUserID,
			concurrent:  original,
			createCount: &createCallCount,
		}, &mockDeviceRepoForAuth{}, newMockRefreshTokenRepo(), &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		result, err := uc.FindOrCreateUser(ctx, provider, info)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.False(t, result.IsNew, "after concurrent create, IsNew should be false")
		assert.Equal(t, original.ID, result.UserID)
	})

	t.Run("returns error when FindByProvider fails", func(t *testing.T) {
		uc := usecase.NewAuthUsecase(nil, &failingUserRepo{err: errors.New("connection refused")}, &mockDeviceRepoForAuth{}, newMockRefreshTokenRepo(), &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		_, err := uc.FindOrCreateUser(ctx, provider, info)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "find user by provider")
	})

	t.Run("returns error when Create fails with non-duplicate error", func(t *testing.T) {
		uc := usecase.NewAuthUsecase(nil, &createFailingUserRepo{err: errors.New("disk full")}, &mockDeviceRepoForAuth{}, newMockRefreshTokenRepo(), &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		_, err := uc.FindOrCreateUser(ctx, provider, info)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create user")
	})

	t.Run("banned user surfaces BannedAt", func(t *testing.T) {
		repoMock := newMockOAuth2UserRepoForAuth()
		bannedAt := time.Now().Add(-time.Hour)
		existing := &model.OAuth2User{
			ID:       uuid.Must(uuid.NewV7()),
			Provider: provider,
			Sub:      providerUserID,
			BannedAt: &bannedAt,
		}
		repoMock.users[provider+":"+providerUserID] = existing

		uc := usecase.NewAuthUsecase(nil, repoMock, &mockDeviceRepoForAuth{}, newMockRefreshTokenRepo(), &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		result, err := uc.FindOrCreateUser(ctx, provider, info)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.NotNil(t, result.BannedAt, "BannedAt should propagate to caller")
		assert.False(t, result.IsNew)
	})
}

// findOrCreateCustomMock is a test-specific mock for FindOrCreateUser that
// simulates a concurrent-create scenario: the first FindByProvider returns
// NotFound, the Create returns a duplicate-key error, and the subsequent
// FindByProvider returns the concurrently-inserted user.
type findOrCreateCustomMock struct {
	t           *testing.T
	provider    string
	sub         string
	concurrent  *model.OAuth2User
	createCount *int
	findCount   int
}

func (m *findOrCreateCustomMock) Create(_ context.Context, user *model.OAuth2User) error {
	*m.createCount++
	return makeDuplicateKeyError()
}

func (m *findOrCreateCustomMock) FindByID(_ context.Context, _ uuid.UUID) (*model.OAuth2User, error) {
	return nil, repo.ErrOAuth2UserNotFound
}

func (m *findOrCreateCustomMock) FindByProvider(_ context.Context, provider, sub string) (*model.OAuth2User, error) {
	m.findCount++
	if m.findCount == 1 {
		// First call: user does not exist yet
		return nil, repo.ErrOAuth2UserNotFound
	}
	// Second call (after duplicate key): concurrent user exists
	return m.concurrent, nil
}

func (m *findOrCreateCustomMock) FindOrCreate(_ context.Context, _ *model.OAuth2User) (*model.OAuth2User, bool, error) {
	return nil, false, nil
}

func (m *findOrCreateCustomMock) Update(_ context.Context, _ *model.OAuth2User) error {
	return nil
}

func (m *findOrCreateCustomMock) ListWithFilters(_ context.Context, _ repo.OAuth2UserFilter) ([]*model.OAuth2User, int64, error) {
	return nil, 0, nil
}

func (m *findOrCreateCustomMock) IsUserBanned(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, nil
}

// failingUserRepo always returns an error from FindByProvider.
type failingUserRepo struct {
	err error
}

func (m *failingUserRepo) Create(_ context.Context, _ *model.OAuth2User) error { return m.err }
func (m *failingUserRepo) FindByID(_ context.Context, _ uuid.UUID) (*model.OAuth2User, error) {
	return nil, m.err
}
func (m *failingUserRepo) FindByProvider(_ context.Context, _, _ string) (*model.OAuth2User, error) {
	return nil, m.err
}
func (m *failingUserRepo) FindOrCreate(_ context.Context, _ *model.OAuth2User) (*model.OAuth2User, bool, error) {
	return nil, false, m.err
}
func (m *failingUserRepo) Update(_ context.Context, _ *model.OAuth2User) error { return m.err }
func (m *failingUserRepo) ListWithFilters(_ context.Context, _ repo.OAuth2UserFilter) ([]*model.OAuth2User, int64, error) {
	return nil, 0, m.err
}
func (m *failingUserRepo) IsUserBanned(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, m.err
}

// createFailingUserRepo returns NotFound from FindByProvider and fails Create.
type createFailingUserRepo struct {
	err error
}

func (m *createFailingUserRepo) Create(_ context.Context, _ *model.OAuth2User) error { return m.err }
func (m *createFailingUserRepo) FindByID(_ context.Context, _ uuid.UUID) (*model.OAuth2User, error) {
	return nil, repo.ErrOAuth2UserNotFound
}
func (m *createFailingUserRepo) FindByProvider(_ context.Context, _, _ string) (*model.OAuth2User, error) {
	return nil, repo.ErrOAuth2UserNotFound
}
func (m *createFailingUserRepo) FindOrCreate(_ context.Context, _ *model.OAuth2User) (*model.OAuth2User, bool, error) {
	return nil, false, m.err
}
func (m *createFailingUserRepo) Update(_ context.Context, _ *model.OAuth2User) error { return nil }
func (m *createFailingUserRepo) ListWithFilters(_ context.Context, _ repo.OAuth2UserFilter) ([]*model.OAuth2User, int64, error) {
	return nil, 0, nil
}
func (m *createFailingUserRepo) IsUserBanned(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, nil
}

// ============================================================
// Tests: AuthUsecase.UpsertDevice
// ============================================================

func TestAuthUsecase_UpsertDevice(t *testing.T) {
	ctx := context.Background()
	userID := uuid.Must(uuid.NewV7())

	t.Run("success with valid input", func(t *testing.T) {
		deviceRepo := &mockDeviceRepoForAuth{}
		uc := usecase.NewAuthUsecase(nil, newMockOAuth2UserRepoForAuth(), deviceRepo, newMockRefreshTokenRepo(), &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		err := uc.UpsertDevice(ctx, &usecase.UpsertDeviceInput{
			UserID:    userID,
			DeviceID:  "my-device",
			Name:      "Alice's Laptop",
			UserAgent: "Mozilla/5.0",
		})
		require.NoError(t, err)
		require.Len(t, deviceRepo.upserted, 1)
		assert.Equal(t, userID, deviceRepo.upserted[0].UserID)
		assert.Equal(t, "my-device", deviceRepo.upserted[0].DeviceID)
	})

	t.Run("device_id exceeding max length is rejected", func(t *testing.T) {
		deviceRepo := &mockDeviceRepoForAuth{}
		uc := usecase.NewAuthUsecase(nil, newMockOAuth2UserRepoForAuth(), deviceRepo, newMockRefreshTokenRepo(), &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		err := uc.UpsertDevice(ctx, &usecase.UpsertDeviceInput{
			UserID:   userID,
			DeviceID: strings.Repeat("a", 101), // > maxDeviceIDLength (100)
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "device_id exceeds maximum length")
		assert.Empty(t, deviceRepo.upserted, "Upsert should not be called when validation fails")
	})

	t.Run("device_id at boundary (exactly 100 runes) is accepted", func(t *testing.T) {
		deviceRepo := &mockDeviceRepoForAuth{}
		uc := usecase.NewAuthUsecase(nil, newMockOAuth2UserRepoForAuth(), deviceRepo, newMockRefreshTokenRepo(), &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		err := uc.UpsertDevice(ctx, &usecase.UpsertDeviceInput{
			UserID:   userID,
			DeviceID: strings.Repeat("d", 100),
		})
		require.NoError(t, err)
		assert.Len(t, deviceRepo.upserted, 1)
	})

	t.Run("long name and user-agent are truncated", func(t *testing.T) {
		deviceRepo := &mockDeviceRepoForAuth{}
		uc := usecase.NewAuthUsecase(nil, newMockOAuth2UserRepoForAuth(), deviceRepo, newMockRefreshTokenRepo(), &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		err := uc.UpsertDevice(ctx, &usecase.UpsertDeviceInput{
			UserID:    userID,
			DeviceID:  "d1",
			Name:      strings.Repeat("n", 500),  // > maxDeviceNameLength
			UserAgent: strings.Repeat("u", 2000), // > maxUserAgentLength
		})
		require.NoError(t, err)
		require.Len(t, deviceRepo.upserted, 1)
		// The truncation happens via stringutil.TruncateByRune — just verify
		// that the stored values are shorter than the original.
		assert.LessOrEqual(t, len([]rune(deviceRepo.upserted[0].Name)), 500)
		assert.LessOrEqual(t, len([]rune(deviceRepo.upserted[0].UserAgent)), 2000)
	})
}

// ============================================================
// Tests: AuthUsecase.ValidateRefreshToken — additional branches
// ============================================================

func TestAuthUsecase_ValidateRefreshToken_AdditionalBranches(t *testing.T) {
	ctx := context.Background()
	userID := uuid.Must(uuid.NewV7())
	deviceID := "dev-1"

	t.Run("banned user is rejected", func(t *testing.T) {
		plainToken := "rt_banned_user_token"
		rtHash := hashRefreshToken(plainToken)
		bannedAt := time.Now().Add(-time.Hour)
		rt := &model.RefreshToken{
			ID:        uuid.Must(uuid.NewV7()),
			TokenHash: rtHash,
			UserID:    userID,
			DeviceID:  deviceID,
			ExpiresAt: time.Now().Add(24 * time.Hour),
			Revoked:   false,
		}

		userRepo := newMockOAuth2UserRepoForAuth()
		userRepo.users["test:banned"] = &model.OAuth2User{
			ID:       userID,
			Provider: "test",
			Sub:      "banned",
			BannedAt: &bannedAt,
		}

		rtRepo := newMockRefreshTokenRepo()
		rtRepo.tokens[rtHash] = rt

		uc := usecase.NewAuthUsecase(nil, userRepo, &mockDeviceRepoForAuth{}, rtRepo, &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		_, err := uc.ValidateRefreshToken(ctx, plainToken)
		require.Error(t, err)
		assert.ErrorIs(t, err, usecase.ErrAccountBanned)
	})

	t.Run("user not found during validation returns error", func(t *testing.T) {
		plainToken := "rt_orphan_token"
		rtHash := hashRefreshToken(plainToken)
		rt := &model.RefreshToken{
			ID:        uuid.Must(uuid.NewV7()),
			TokenHash: rtHash,
			UserID:    uuid.Must(uuid.NewV7()), // user does not exist in repo
			DeviceID:  deviceID,
			ExpiresAt: time.Now().Add(24 * time.Hour),
			Revoked:   false,
		}

		userRepo := newMockOAuth2UserRepoForAuth() // empty — no users
		rtRepo := newMockRefreshTokenRepo()
		rtRepo.tokens[rtHash] = rt

		uc := usecase.NewAuthUsecase(nil, userRepo, &mockDeviceRepoForAuth{}, rtRepo, &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		_, err := uc.ValidateRefreshToken(ctx, plainToken)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "find user")
	})

	t.Run("successful validation revokes the token", func(t *testing.T) {
		plainToken := "rt_success_token"
		rtHash := hashRefreshToken(plainToken)
		rt := &model.RefreshToken{
			ID:        uuid.Must(uuid.NewV7()),
			TokenHash: rtHash,
			UserID:    userID,
			DeviceID:  deviceID,
			ExpiresAt: time.Now().Add(24 * time.Hour),
			Revoked:   false,
		}

		userRepo := newMockOAuth2UserRepoForAuth()
		userRepo.users["github:alice"] = &model.OAuth2User{
			ID:       userID,
			Provider: "github",
			Sub:      "alice",
		}

		rtRepo := newMockRefreshTokenRepo()
		rtRepo.tokens[rtHash] = rt

		uc := usecase.NewAuthUsecase(nil, userRepo, &mockDeviceRepoForAuth{}, rtRepo, &mockAuthSigner{accessTTL: time.Hour}, 24*time.Hour, 30*time.Second)

		info, err := uc.ValidateRefreshToken(ctx, plainToken)
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Equal(t, userID, info.UserID)
		assert.Equal(t, deviceID, info.DeviceID)
		assert.Equal(t, rt.ID, info.ID)

		// Token must have been revoked (rotation)
		assert.True(t, rtRepo.tokens[rtHash].Revoked, "refresh token should be revoked after use")
	})
}
