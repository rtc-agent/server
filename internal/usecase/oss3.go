package usecase

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// Sentinel errors for presigned URL validation.
// Handlers use errors.Is() to detect these instead of fragile string matching.
var (
	// ErrKeyPrefixViolation indicates the requested key is outside the user's allowed prefix.
	ErrKeyPrefixViolation = errors.New("key must start with user prefix")
	// ErrUnsupportedPresignOperation indicates an operation other than "put" or "get".
	ErrUnsupportedPresignOperation = errors.New("unsupported operation")
	// ErrExpiryExceeded indicates the requested expiry exceeds the maximum allowed.
	ErrExpiryExceeded = errors.New("expires_in must not exceed maximum")
)

// OSS3Usecase encapsulates all OSS3 business logic.
type OSS3Usecase struct {
	backend    rtcoss3.Backend
	db         *gorm.DB // Direct DB access for transactional operations
	fileRepo   repo.FileRepo
	uploadRepo repo.MultipartUploadRepo
	credRepo   repo.TemporaryCredentialRepo
	redis      *redis.Client
	cfg        config.StorageConfig
	scripts    map[string]*redis.Script // OSS3 Lua scripts from 1A-9
	aesGCM     cipher.AEAD              // AES-256-GCM for SessionToken encryption
	credFlight singleflight.Group       // Deduplicates concurrent credential cache misses
}

// NewOSS3Usecase creates the OSS3 usecase and initializes AES-GCM.
// Returns error if the encryption key is not 32 bytes.
func NewOSS3Usecase(
	backend rtcoss3.Backend,
	db *gorm.DB,
	fileRepo repo.FileRepo,
	uploadRepo repo.MultipartUploadRepo,
	credRepo repo.TemporaryCredentialRepo,
	redisClient *redis.Client,
	scripts map[string]*redis.Script,
	cfg config.StorageConfig,
) (*OSS3Usecase, error) {
	// Initialize AES-256-GCM
	// Key is hex-encoded (64 chars = 32 bytes)
	keyHex := cfg.Encryption.SessionTokenKey
	if len(keyHex) != 64 {
		return nil, fmt.Errorf("encryption key must be 64 hex chars (32 bytes), got %d", len(keyHex))
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("decode encryption key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new aes cipher: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}

	return &OSS3Usecase{
		backend:    backend,
		db:         db,
		fileRepo:   fileRepo,
		uploadRepo: uploadRepo,
		credRepo:   credRepo,
		redis:      redisClient,
		cfg:        cfg,
		scripts:    scripts,
		aesGCM:     aesGCM,
	}, nil
}

// SessionTokenPayload is the structure encrypted in SessionToken.
type SessionTokenPayload struct {
	UserID        string    `json:"user_id"`
	AllowedPrefix string    `json:"allowed_prefix"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// encryptSessionToken encrypts a SessionTokenPayload using AES-256-GCM.
// Returns base64-encoded string: nonce(12) + ciphertext + tag(16)
func (uc *OSS3Usecase) encryptSessionToken(payload SessionTokenPayload) (string, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	nonce := make([]byte, 12) // 96-bit nonce
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	sealed := uc.aesGCM.Seal(nonce, nonce, plaintext, nil)
	return base64.URLEncoding.EncodeToString(sealed), nil
}

// decryptSessionToken decrypts a SessionToken string.
func (uc *OSS3Usecase) decryptSessionToken(token string) (*SessionTokenPayload, error) {
	sealed, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}

	if len(sealed) < 12 {
		return nil, fmt.Errorf("invalid token: too short")
	}

	nonce, ciphertext := sealed[:12], sealed[12:]
	plaintext, err := uc.aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt token: %w", err)
	}

	var payload SessionTokenPayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal token: %w", err)
	}

	if payload.ExpiresAt.Before(time.Now()) {
		return nil, rtcoss3.ErrExpiredToken
	}

	return &payload, nil
}

// DecryptSessionToken decrypts a SessionToken string (exported for middleware use).
func (uc *OSS3Usecase) DecryptSessionToken(token string) (*SessionTokenPayload, error) {
	return uc.decryptSessionToken(token)
}

// encryptString encrypts a plain string using AES-256-GCM.
// Returns base64-encoded string: nonce(12) + ciphertext + tag(16)
func (uc *OSS3Usecase) encryptString(plaintext string) (string, error) {
	nonce := make([]byte, 12) // 96-bit nonce
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	sealed := uc.aesGCM.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.URLEncoding.EncodeToString(sealed), nil
}

// decryptString decrypts a base64-encoded AES-256-GCM encrypted string.
func (uc *OSS3Usecase) decryptString(encoded string) (string, error) {
	sealed, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode string: %w", err)
	}

	if len(sealed) < 12 {
		return "", fmt.Errorf("invalid encrypted string: too short")
	}

	nonce, ciphertext := sealed[:12], sealed[12:]
	plaintext, err := uc.aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt string: %w", err)
	}

	return string(plaintext), nil
}

// generateRandomString generates a cryptographically secure random string
// of the given length using rejection sampling to avoid modulo bias.
func generateRandomString(length int) (string, error) {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const maxByte = byte(255 - 255%len(charset)) // 252 for charset len 36

	b := make([]byte, length)
	buf := make([]byte, 1)
	for i := range b {
		for {
			if _, err := rand.Read(buf); err != nil {
				return "", fmt.Errorf("generate random string: %w", err)
			}
			if buf[0] < maxByte {
				b[i] = charset[buf[0]%byte(len(charset))]
				break
			}
		}
	}
	return string(b), nil
}

// Backend returns the storage backend (exposed for handler layer).
func (uc *OSS3Usecase) Backend() rtcoss3.Backend {
	return uc.backend
}

// FileRecord represents a file metadata record for handler layer.
type FileRecord struct {
	UserID      string
	Bucket      string
	Key         string
	Size        int64
	ContentType string
	ETag        string
}

// CreateFileRecord creates a file metadata record in the database.
func (uc *OSS3Usecase) CreateFileRecord(ctx context.Context, record *FileRecord) error {
	// Convert FileRecord to model.File
	file := &model.File{
		UserID:      record.UserID,
		Bucket:      record.Bucket,
		Key:         record.Key,
		Size:        record.Size,
		ContentType: record.ContentType,
		ETag:        record.ETag,
	}
	return uc.fileRepo.Create(ctx, file)
}

// DeleteFileRecord deletes a file metadata record from the database.
func (uc *OSS3Usecase) DeleteFileRecord(ctx context.Context, userID, key string) error {
	return uc.fileRepo.DeleteByUserAndKey(ctx, userID, key)
}

// GetFileRecord looks up a file metadata record by user_id and key.
// Returns nil and no error if the file does not exist.
func (uc *OSS3Usecase) GetFileRecord(ctx context.Context, userID, key string) (*FileRecord, error) {
	file, err := uc.fileRepo.GetByUserAndKey(ctx, userID, key)
	if err != nil {
		return nil, fmt.Errorf("get file record: %w", err)
	}
	if file == nil {
		return nil, nil
	}
	return &FileRecord{
		UserID:      file.UserID,
		Bucket:      file.Bucket,
		Key:         file.Key,
		Size:        file.Size,
		ContentType: file.ContentType,
		ETag:        file.ETag,
	}, nil
}

// CreateMultipartUploadRecord creates a multipart upload tracking record in the database.
func (uc *OSS3Usecase) CreateMultipartUploadRecord(ctx context.Context, userID, bucket, key, uploadID string) error {
	upload := &model.MultipartUpload{
		UserID:    userID,
		Bucket:    bucket,
		Key:       key,
		UploadID:  uploadID,
		Status:    "uploading",
		ExpiresAt: time.Now().Add(24 * time.Hour), // 24h expiry
	}
	return uc.uploadRepo.Create(ctx, upload)
}

// GeneratePresignedURL generates a presigned URL for the given operation.
// Users can only generate presigned URLs for keys under their own prefix (user-{userID}/).
func (uc *OSS3Usecase) GeneratePresignedURL(ctx context.Context, userID, operation, key string, expiresIn time.Duration) (string, time.Time, error) {
	// Validate key prefix — user can only access their own prefix
	expectedPrefix := "user-" + userID + "/"
	if !strings.HasPrefix(key, expectedPrefix) {
		return "", time.Time{}, fmt.Errorf("%w: key must start with %q", ErrKeyPrefixViolation, expectedPrefix)
	}

	// Validate expiry
	maxExpiry := 7 * 24 * time.Hour // 7 days
	if expiresIn <= 0 {
		expiresIn = time.Hour // default 1h
	}
	if expiresIn > maxExpiry {
		return "", time.Time{}, fmt.Errorf("%w: expires_in must not exceed %v", ErrExpiryExceeded, maxExpiry)
	}

	bucket := uc.cfg.MinIO.Bucket
	var url string
	var err error
	switch operation {
	case "put":
		url, err = uc.backend.PresignPut(ctx, bucket, key, expiresIn)
	case "get":
		url, err = uc.backend.PresignGet(ctx, bucket, key, expiresIn)
	default:
		return "", time.Time{}, fmt.Errorf("%w: %q (must be 'put' or 'get')", ErrUnsupportedPresignOperation, operation)
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presign %s: %w", operation, err)
	}

	expiresAt := time.Now().Add(expiresIn)
	return url, expiresAt, nil
}

// DeleteMultipartUploadRecord deletes a multipart upload tracking record from the database.
// Parts and upload record are deleted atomically within a transaction to prevent
// orphaned parts records if the second delete fails.
func (uc *OSS3Usecase) DeleteMultipartUploadRecord(ctx context.Context, uploadID string) error {
	// First get the upload by uploadID to get the UUID (outside transaction is fine)
	upload, err := uc.uploadRepo.GetByUploadID(ctx, uploadID)
	if err != nil {
		return err
	}

	// Wrap DeleteParts + Delete in a transaction for atomicity
	return uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)
		if err := uc.uploadRepo.DeleteParts(txCtx, upload.ID); err != nil {
			return fmt.Errorf("delete parts: %w", err)
		}
		if err := uc.uploadRepo.Delete(txCtx, upload.ID); err != nil {
			return fmt.Errorf("delete upload record: %w", err)
		}
		return nil
	})
}
