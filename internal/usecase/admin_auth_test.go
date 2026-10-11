package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
)

// mockAdminRefreshTokenRepo implements repo.AdminRefreshTokenRepo for testing.
type mockAdminRefreshTokenRepo struct {
	tokens map[string]*model.AdminRefreshToken // key = token hash
}

func newMockAdminRefreshTokenRepo() *mockAdminRefreshTokenRepo {
	return &mockAdminRefreshTokenRepo{tokens: make(map[string]*model.AdminRefreshToken)}
}

func (m *mockAdminRefreshTokenRepo) Create(ctx context.Context, rt *model.AdminRefreshToken) error {
	if rt.ID == uuid.Nil {
		rt.ID = uuid.Must(uuid.NewV7())
	}
	m.tokens[rt.TokenHash] = rt
	return nil
}

func (m *mockAdminRefreshTokenRepo) FindByHash(ctx context.Context, hash string) (*model.AdminRefreshToken, error) {
	rt, ok := m.tokens[hash]
	if !ok {
		return nil, repo.ErrRefreshTokenNotFound
	}
	return rt, nil
}

func (m *mockAdminRefreshTokenRepo) Revoke(ctx context.Context, id uuid.UUID) error {
	for _, rt := range m.tokens {
		if rt.ID == id {
			rt.Revoked = true
			return nil
		}
	}
	return repo.ErrRefreshTokenNotFound
}

func (m *mockAdminRefreshTokenRepo) RevokeAllByUserID(ctx context.Context, userID uuid.UUID) (int64, error) {
	var count int64
	for _, rt := range m.tokens {
		if rt.UserID == userID && !rt.Revoked {
			rt.Revoked = true
			count++
		}
	}
	return count, nil
}

// mockAdminUserRepo implements repo.AdminUserRepo for testing.
type mockAdminUserRepo struct {
	users map[string]*model.AdminUser // key = email or ID
}

func newMockAdminUserRepo() *mockAdminUserRepo {
	return &mockAdminUserRepo{users: make(map[string]*model.AdminUser)}
}

func (m *mockAdminUserRepo) Create(ctx context.Context, user *model.AdminUser) error {
	if user.ID == uuid.Nil {
		user.ID = uuid.Must(uuid.NewV7())
	}
	m.users[user.Email] = user
	m.users[user.ID.String()] = user
	return nil
}

func (m *mockAdminUserRepo) GetByEmail(ctx context.Context, email string) (*model.AdminUser, error) {
	user, ok := m.users[email]
	if !ok {
		return nil, repo.ErrOAuth2UserNotFound // Using this as a stand-in for admin user not found
	}
	return user, nil
}

func (m *mockAdminUserRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.AdminUser, error) {
	user, ok := m.users[id.String()]
	if !ok {
		return nil, repo.ErrOAuth2UserNotFound
	}
	return user, nil
}

func (m *mockAdminUserRepo) Update(ctx context.Context, user *model.AdminUser) error {
	m.users[user.Email] = user
	m.users[user.ID.String()] = user
	return nil
}

func (m *mockAdminUserRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) ([]*model.AdminUser, error) {
	var result []*model.AdminUser
	for _, id := range ids {
		if user, ok := m.users[id.String()]; ok {
			result = append(result, user)
		}
	}
	return result, nil
}

func (m *mockAdminUserRepo) List(ctx context.Context, page, pageSize int) ([]*model.AdminUser, int64, error) {
	result := make([]*model.AdminUser, 0, len(m.users))
	for _, user := range m.users {
		result = append(result, user)
	}
	return result, int64(len(result)), nil
}

// mockAdminTokenSigner implements usecase.AdminTokenSigner for testing.
type mockAdminTokenSigner struct {
	accessTTL  time.Duration
	refreshTTL time.Duration
}

func (m *mockAdminTokenSigner) SignAccessToken(userID uuid.UUID, email, name string) (string, time.Time, error) {
	return "mock-admin-access-token", time.Now().Add(m.accessTTL), nil
}

func (m *mockAdminTokenSigner) SignRefreshToken(userID uuid.UUID) (string, time.Time, error) {
	return "mock-admin-refresh-token", time.Now().Add(m.refreshTTL), nil
}

func (m *mockAdminTokenSigner) ParseAccessToken(token string) (*auth.AdminClaims, error) {
	return &auth.AdminClaims{}, nil
}

func (m *mockAdminTokenSigner) ParseRefreshToken(token string) (*auth.AdminClaims, error) {
	return &auth.AdminClaims{}, nil
}

func (m *mockAdminTokenSigner) AccessTTL() time.Duration {
	return m.accessTTL
}

func (m *mockAdminTokenSigner) RefreshTTL() time.Duration {
	return m.refreshTTL
}

// mockLoginProtection implements usecase.LoginProtectionInterface for testing.
type mockLoginProtection struct {
	allowLogin     bool
	failedAttempts map[string]int
}

func newMockLoginProtection() *mockLoginProtection {
	return &mockLoginProtection{
		allowLogin:     true,
		failedAttempts: make(map[string]int),
	}
}

func (m *mockLoginProtection) CheckLoginAllowed(ctx context.Context, ip, email string) error {
	if !m.allowLogin {
		return repo.ErrPermissionDenied
	}
	return nil
}

func (m *mockLoginProtection) RecordFailedLogin(ctx context.Context, ip, email string) {
	key := ip + ":" + email
	m.failedAttempts[key]++
}

func (m *mockLoginProtection) ResetLoginAttempts(ctx context.Context, ip, email string) {
	key := ip + ":" + email
	delete(m.failedAttempts, key)
}

func (m *mockLoginProtection) Stop() {}

func TestAdminAuthUsecase_Login(t *testing.T) {
	ctx := context.Background()

	// Create test user with hashed password
	password := "test-password-123"
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)

	userID := uuid.Must(uuid.NewV7())
	testUser := &model.AdminUser{
		ID:           userID,
		Email:        "admin@example.com",
		Name:         "Test Admin",
		PasswordHash: string(hashedPassword),
	}

	tests := []struct {
		name        string
		email       string
		password    string
		setupUser   func() *mockAdminUserRepo
		setupLP     func() *mockLoginProtection
		wantErr     bool
		errContains string
		wantResult  bool
	}{
		{
			name:     "successful login",
			email:    "admin@example.com",
			password: password,
			setupUser: func() *mockAdminUserRepo {
				r := newMockAdminUserRepo()
				r.Create(ctx, testUser)
				return r
			},
			setupLP:    newMockLoginProtection,
			wantErr:    false,
			wantResult: true,
		},
		{
			name:     "wrong password",
			email:    "admin@example.com",
			password: "wrong-password",
			setupUser: func() *mockAdminUserRepo {
				r := newMockAdminUserRepo()
				r.Create(ctx, testUser)
				return r
			},
			setupLP:     newMockLoginProtection,
			wantErr:     true,
			errContains: "invalid",
		},
		{
			name:     "user not found",
			email:    "nonexistent@example.com",
			password: "any-password",
			setupUser: func() *mockAdminUserRepo {
				return newMockAdminUserRepo()
			},
			setupLP:     newMockLoginProtection,
			wantErr:     true,
			errContains: "invalid",
		},
		{
			name:     "login protection blocked",
			email:    "admin@example.com",
			password: password,
			setupUser: func() *mockAdminUserRepo {
				r := newMockAdminUserRepo()
				r.Create(ctx, testUser)
				return r
			},
			setupLP: func() *mockLoginProtection {
				lp := newMockLoginProtection()
				lp.allowLogin = false
				return lp
			},
			wantErr:     true,
			errContains: "locked",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adminUserRepo := tt.setupUser()
			refreshTokenRepo := newMockAdminRefreshTokenRepo()
			signer := &mockAdminTokenSigner{
				accessTTL:  time.Hour,
				refreshTTL: 24 * time.Hour,
			}

			uc := usecase.NewAdminAuthUsecase(
				nil, // DB not needed for these tests
				adminUserRepo,
				refreshTokenRepo,
				signer,
			)

			lp := tt.setupLP()
			uc.SetLoginProtection(lp)

			result, err := uc.Login(ctx, tt.email, tt.password, "127.0.0.1")
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			require.NoError(t, err)
			if tt.wantResult {
				require.NotNil(t, result)
				assert.NotEmpty(t, result.AccessToken)
				assert.NotEmpty(t, result.RefreshToken)
				assert.Equal(t, int64(3600), result.ExpiresIn)
				assert.NotNil(t, result.User)
				assert.Equal(t, userID, result.User.ID)
				assert.Equal(t, "admin@example.com", result.User.Email)

				// Verify refresh token was stored
				assert.Len(t, refreshTokenRepo.tokens, 1)

				// Verify login protection counter was reset
				assert.Equal(t, 0, len(lp.failedAttempts))
			}
		})
	}
}

func TestAdminAuthUsecase_Login_WithoutLoginProtection(t *testing.T) {
	ctx := context.Background()

	password := "test-password-123"
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)

	userID := uuid.Must(uuid.NewV7())
	testUser := &model.AdminUser{
		ID:           userID,
		Email:        "admin@example.com",
		Name:         "Test Admin",
		PasswordHash: string(hashedPassword),
	}

	adminUserRepo := newMockAdminUserRepo()
	adminUserRepo.Create(ctx, testUser)

	refreshTokenRepo := newMockAdminRefreshTokenRepo()
	signer := &mockAdminTokenSigner{
		accessTTL:  time.Hour,
		refreshTTL: 24 * time.Hour,
	}

	uc := usecase.NewAdminAuthUsecase(
		nil,
		adminUserRepo,
		refreshTokenRepo,
		signer,
	)
	// Don't set login protection

	result, err := uc.Login(ctx, "admin@example.com", password, "127.0.0.1")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.AccessToken)
	assert.NotEmpty(t, result.RefreshToken)
}

func TestAdminAuthUsecase_GetCurrentUser(t *testing.T) {
	ctx := context.Background()

	userID := uuid.Must(uuid.NewV7())
	testUser := &model.AdminUser{
		ID:    userID,
		Email: "admin@example.com",
		Name:  "Test Admin",
	}

	adminUserRepo := newMockAdminUserRepo()
	adminUserRepo.Create(ctx, testUser)

	uc := usecase.NewAdminAuthUsecase(
		nil,
		adminUserRepo,
		newMockAdminRefreshTokenRepo(),
		&mockAdminTokenSigner{},
	)

	// Test existing user
	user, err := uc.GetCurrentUser(ctx, userID)
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, userID, user.ID)
	assert.Equal(t, "admin@example.com", user.Email)

	// Test non-existing user
	nonExistentID := uuid.Must(uuid.NewV7())
	user, err = uc.GetCurrentUser(ctx, nonExistentID)
	require.Error(t, err)
	assert.Nil(t, user)
	assert.Contains(t, err.Error(), "not found")
}

func TestAdminAuthUsecase_Logout(t *testing.T) {
	ctx := context.Background()

	// Create a refresh token
	tokenPlain := "rt_test_token"
	tokenHash := hashRefreshToken(tokenPlain)
	userID := uuid.Must(uuid.NewV7())

	rt := &model.AdminRefreshToken{
		ID:        uuid.Must(uuid.NewV7()),
		TokenHash: tokenHash,
		UserID:    userID,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		Revoked:   false,
	}

	refreshTokenRepo := newMockAdminRefreshTokenRepo()
	refreshTokenRepo.tokens[tokenHash] = rt

	uc := usecase.NewAdminAuthUsecase(
		nil,
		newMockAdminUserRepo(),
		refreshTokenRepo,
		&mockAdminTokenSigner{},
	)

	// Test successful logout
	err := uc.Logout(ctx, tokenPlain)
	require.NoError(t, err)

	// Verify token was revoked
	storedRT := refreshTokenRepo.tokens[tokenHash]
	assert.True(t, storedRT.Revoked)

	// Test logout with non-existent token (should not error)
	err = uc.Logout(ctx, "rt_nonexistent")
	require.NoError(t, err)

	// Test logout with already revoked token (should not error)
	err = uc.Logout(ctx, tokenPlain)
	require.NoError(t, err)
}
