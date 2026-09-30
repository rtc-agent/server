package usecase

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// OSS3Usecase encapsulates all OSS3 business logic.
type OSS3Usecase struct {
	backend    rtcoss3.Backend
	fileRepo   repo.FileRepo
	uploadRepo repo.MultipartUploadRepo
	credRepo   repo.TemporaryCredentialRepo
	redis      *redis.Client
	cfg        config.StorageConfig
	scripts    map[string]*redis.Script // OSS3 Lua scripts from 1A-9
	aesGCM     cipher.AEAD              // AES-256-GCM for SessionToken encryption
}

// NewOSS3Usecase creates the OSS3 usecase and initializes AES-GCM.
// Returns error if the encryption key is not 32 bytes.
func NewOSS3Usecase(
	backend rtcoss3.Backend,
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

// generateRandomString generates a cryptographically secure random string
// of the given length using rejection sampling to avoid modulo bias.
func generateRandomString(length int) string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const maxByte = byte(255 - 255%len(charset)) // 252 for charset len 36

	b := make([]byte, length)
	buf := make([]byte, 1)
	for i := range b {
		for {
			if _, err := rand.Read(buf); err != nil {
				panic(fmt.Errorf("generate random string: %w", err))
			}
			if buf[0] < maxByte {
				b[i] = charset[buf[0]%byte(len(charset))]
				break
			}
		}
	}
	return string(b)
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

// CreateMultipartUploadRecord creates a multipart upload tracking record in the database.
func (uc *OSS3Usecase) CreateMultipartUploadRecord(ctx context.Context, userID, bucket, key, uploadID string) error {
	upload := &model.MultipartUpload{
		UserID:    userID,
		Key:       key,
		UploadID:  uploadID,
		Status:    "uploading",
		ExpiresAt: time.Now().Add(24 * time.Hour), // 24h expiry
	}
	return uc.uploadRepo.Create(ctx, upload)
}

// DeleteMultipartUploadRecord deletes a multipart upload tracking record from the database.
func (uc *OSS3Usecase) DeleteMultipartUploadRecord(ctx context.Context, uploadID string) error {
	// First get the upload by uploadID to get the UUID
	upload, err := uc.uploadRepo.GetByUploadID(ctx, uploadID)
	if err != nil {
		return err
	}
	// Delete parts first
	if err := uc.uploadRepo.DeleteParts(ctx, upload.ID); err != nil {
		return err
	}
	// Then delete the upload record
	return uc.uploadRepo.Delete(ctx, upload.ID)
}
