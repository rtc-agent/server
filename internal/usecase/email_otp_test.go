// Package usecase provides tests for email OTP functionality.
package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/infra/email"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockEmailSender is a mock email sender for testing.
type mockEmailSender struct {
	sendFunc func(ctx context.Context, msg *email.Message) error
}

func (m *mockEmailSender) Send(ctx context.Context, msg *email.Message) error {
	if m.sendFunc != nil {
		return m.sendFunc(ctx, msg)
	}
	return nil
}

// mockAdminUserRepo is a simple mock for AdminUserRepo.
type mockAdminUserRepo struct {
	getByEmailFunc func(ctx context.Context, emailAddr string) (*model.AdminUser, error)
}

func (m *mockAdminUserRepo) Create(ctx context.Context, user *model.AdminUser) error {
	return nil
}

func (m *mockAdminUserRepo) GetByEmail(ctx context.Context, emailAddr string) (*model.AdminUser, error) {
	if m.getByEmailFunc != nil {
		return m.getByEmailFunc(ctx, emailAddr)
	}
	return nil, errors.New("not implemented")
}

func (m *mockAdminUserRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.AdminUser, error) {
	return nil, errors.New("not implemented")
}

func (m *mockAdminUserRepo) Update(ctx context.Context, user *model.AdminUser) error {
	return nil
}

func (m *mockAdminUserRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) ([]*model.AdminUser, error) {
	return nil, nil
}

func (m *mockAdminUserRepo) List(ctx context.Context, page, pageSize int) ([]*model.AdminUser, int64, error) {
	return nil, 0, nil
}

// Ensure mockAdminUserRepo implements repo.AdminUserRepo
var _ repo.AdminUserRepo = (*mockAdminUserRepo)(nil)

// TestRedisOTPStore_Store_OverwriteProtection tests that Store uses SETNX to prevent overwrites.
func TestRedisOTPStore_Store_OverwriteProtection(t *testing.T) {
	// This test requires a real Redis connection, so we'll skip it in unit tests
	// and rely on integration tests. The implementation uses SetNX which atomically
	// checks if the key exists before setting.
	t.Skip("Requires Redis connection - covered by integration tests")
}

// TestMemoryOTPStore_Store_OverwriteProtection tests that MemoryOTPStore prevents overwrites.
func TestMemoryOTPStore_Store_OverwriteProtection(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryOTPStore(5, 3, time.Minute, 30*time.Second)
	defer store.Close()

	emailAddr := "test@example.com"
	otp1 := "123456"
	otp2 := "654321"
	ttl := 5 * time.Minute

	// First store should succeed
	err := store.Store(ctx, emailAddr, otp1, ttl)
	require.NoError(t, err, "first Store should succeed")

	// Second store should fail with ErrOTPAlreadyExists
	err = store.Store(ctx, emailAddr, otp2, ttl)
	assert.ErrorIs(t, err, ErrOTPAlreadyExists, "second Store should return ErrOTPAlreadyExists")

	// Verify the original OTP is still stored
	stored, err := store.Get(ctx, emailAddr)
	require.NoError(t, err)
	assert.Equal(t, otp1, stored, "original OTP should not be overwritten")
}

// TestMemoryOTPStore_Store_ExpiredOTPAllowsOverwrite tests that expired OTPs can be overwritten.
func TestMemoryOTPStore_Store_ExpiredOTPAllowsOverwrite(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryOTPStore(5, 3, time.Minute, 30*time.Second)
	defer store.Close()

	emailAddr := "test@example.com"
	otp1 := "123456"
	otp2 := "654321"

	// Store with very short TTL
	err := store.Store(ctx, emailAddr, otp1, 1*time.Millisecond)
	require.NoError(t, err)

	// Wait for OTP to expire
	time.Sleep(5 * time.Millisecond)

	// Store new OTP should succeed since the old one expired
	err = store.Store(ctx, emailAddr, otp2, 5*time.Minute)
	require.NoError(t, err, "Store should succeed after OTP expires")

	// Verify the new OTP is stored
	stored, err := store.Get(ctx, emailAddr)
	require.NoError(t, err)
	assert.Equal(t, otp2, stored, "new OTP should be stored after old one expired")
}

// TestEmailOTPUsecase_SendOTP_EmailFailureRollback tests that OTP is deleted when email send fails.
func TestEmailOTPUsecase_SendOTP_EmailFailureRollback(t *testing.T) {
	ctx := context.Background()
	emailAddr := "admin@example.com"
	clientIP := "192.168.1.1"

	// Mock admin user repo
	mockRepo := &mockAdminUserRepo{
		getByEmailFunc: func(ctx context.Context, email string) (*model.AdminUser, error) {
			return &model.AdminUser{ID: uuid.New(), Email: email}, nil
		},
	}

	// Mock email sender that fails
	failingSender := &mockEmailSender{
		sendFunc: func(ctx context.Context, msg *email.Message) error {
			return errors.New("smtp connection failed")
		},
	}

	// Use memory store
	store := NewMemoryOTPStore(5, 3, time.Minute, 30*time.Second)
	defer store.Close()

	cfg := EmailOTPConfig{
		TTL:               5 * time.Minute,
		Length:            6,
		SendCooldown:      30 * time.Second,
		MaxSendPerIP:      5,
		MaxVerifyAttempts: 3,
		LockDuration:      time.Minute,
	}

	uc := NewEmailOTPUsecase(mockRepo, failingSender, store, cfg)

	// SendOTP should fail
	err := uc.SendOTP(ctx, emailAddr, clientIP)
	assert.Error(t, err, "SendOTP should return error when email send fails")
	assert.Contains(t, err.Error(), "smtp connection failed")

	// Verify OTP was deleted (rolled back)
	_, err = store.Get(ctx, emailAddr)
	assert.ErrorIs(t, err, ErrOTPNotFound, "OTP should be deleted after email send failure")
}

// TestEmailOTPUsecase_SendOTP_AlreadyExistsError tests handling of ErrOTPAlreadyExists.
func TestEmailOTPUsecase_SendOTP_AlreadyExistsError(t *testing.T) {
	ctx := context.Background()
	emailAddr := "admin@example.com"
	clientIP := "192.168.1.1"

	// Mock admin user repo
	mockRepo := &mockAdminUserRepo{
		getByEmailFunc: func(ctx context.Context, email string) (*model.AdminUser, error) {
			return &model.AdminUser{ID: uuid.New(), Email: email}, nil
		},
	}

	// Mock email sender that succeeds
	successSender := &mockEmailSender{}

	// Use memory store with an existing OTP
	store := NewMemoryOTPStore(5, 3, time.Minute, 30*time.Second)
	defer store.Close()

	// Pre-store an OTP to simulate race condition
	err := store.Store(ctx, emailAddr, "999999", 5*time.Minute)
	require.NoError(t, err)

	cfg := EmailOTPConfig{
		TTL:               5 * time.Minute,
		Length:            6,
		SendCooldown:      30 * time.Second,
		MaxSendPerIP:      5,
		MaxVerifyAttempts: 3,
		LockDuration:      time.Minute,
	}

	uc := NewEmailOTPUsecase(mockRepo, successSender, store, cfg)

	// SendOTP should return cooldown error when OTP already exists
	err = uc.SendOTP(ctx, emailAddr, clientIP)
	assert.ErrorIs(t, err, ErrOTPCooldown, "SendOTP should return ErrOTPCooldown when OTP already exists")
}

// TestEmailOTPUsecase_SendOTP_Success tests successful OTP send flow.
func TestEmailOTPUsecase_SendOTP_Success(t *testing.T) {
	ctx := context.Background()
	emailAddr := "admin@example.com"
	clientIP := "192.168.1.1"

	// Mock admin user repo
	mockRepo := &mockAdminUserRepo{
		getByEmailFunc: func(ctx context.Context, email string) (*model.AdminUser, error) {
			return &model.AdminUser{ID: uuid.New(), Email: email}, nil
		},
	}

	// Track if email was sent
	emailSent := false
	successSender := &mockEmailSender{
		sendFunc: func(ctx context.Context, msg *email.Message) error {
			emailSent = true
			assert.Equal(t, emailAddr, msg.To)
			assert.Contains(t, msg.Subject, "验证码")
			return nil
		},
	}

	store := NewMemoryOTPStore(5, 3, time.Minute, 30*time.Second)
	defer store.Close()

	cfg := EmailOTPConfig{
		TTL:               5 * time.Minute,
		Length:            6,
		SendCooldown:      30 * time.Second,
		MaxSendPerIP:      5,
		MaxVerifyAttempts: 3,
		LockDuration:      time.Minute,
	}

	uc := NewEmailOTPUsecase(mockRepo, successSender, store, cfg)

	// SendOTP should succeed
	err := uc.SendOTP(ctx, emailAddr, clientIP)
	assert.NoError(t, err)
	assert.True(t, emailSent, "email should be sent")

	// Verify OTP is stored
	storedOTP, err := store.Get(ctx, emailAddr)
	require.NoError(t, err)
	assert.Len(t, storedOTP, 6, "OTP should be 6 digits")
}

// TestEmailOTPUsecase_VerifyOTP_Success tests successful OTP verification.
func TestEmailOTPUsecase_VerifyOTP_Success(t *testing.T) {
	ctx := context.Background()
	emailAddr := "admin@example.com"

	mockRepo := &mockAdminUserRepo{}
	successSender := &mockEmailSender{}
	store := NewMemoryOTPStore(5, 3, time.Minute, 30*time.Second)
	defer store.Close()

	// Pre-store an OTP
	expectedOTP := "123456"
	err := store.Store(ctx, emailAddr, expectedOTP, 5*time.Minute)
	require.NoError(t, err)

	cfg := EmailOTPConfig{
		TTL:               5 * time.Minute,
		Length:            6,
		SendCooldown:      30 * time.Second,
		MaxSendPerIP:      5,
		MaxVerifyAttempts: 3,
		LockDuration:      time.Minute,
	}

	uc := NewEmailOTPUsecase(mockRepo, successSender, store, cfg)

	// Verify with correct OTP
	err = uc.VerifyOTP(ctx, emailAddr, expectedOTP)
	assert.NoError(t, err)

	// Verify OTP was consumed (deleted)
	_, err = store.Get(ctx, emailAddr)
	assert.ErrorIs(t, err, ErrOTPNotFound, "OTP should be deleted after successful verification")
}

// TestEmailOTPUsecase_VerifyOTP_InvalidCode tests verification with wrong OTP.
func TestEmailOTPUsecase_VerifyOTP_InvalidCode(t *testing.T) {
	ctx := context.Background()
	emailAddr := "admin@example.com"

	mockRepo := &mockAdminUserRepo{}
	successSender := &mockEmailSender{}
	store := NewMemoryOTPStore(5, 3, time.Minute, 30*time.Second)
	defer store.Close()

	// Pre-store an OTP
	err := store.Store(ctx, emailAddr, "123456", 5*time.Minute)
	require.NoError(t, err)

	cfg := EmailOTPConfig{
		TTL:               5 * time.Minute,
		Length:            6,
		SendCooldown:      30 * time.Second,
		MaxSendPerIP:      5,
		MaxVerifyAttempts: 3,
		LockDuration:      time.Minute,
	}

	uc := NewEmailOTPUsecase(mockRepo, successSender, store, cfg)

	// Verify with wrong OTP
	err = uc.VerifyOTP(ctx, emailAddr, "654321")
	assert.ErrorIs(t, err, ErrOTPInvalid)

	// Verify original OTP is still there (not consumed)
	storedOTP, err := store.Get(ctx, emailAddr)
	require.NoError(t, err)
	assert.Equal(t, "123456", storedOTP, "OTP should not be deleted on failed verification")
}
