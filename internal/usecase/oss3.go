package usecase

import (
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

// generateRandomString generates a random string of the given length.
// Uses crypto/rand for cryptographic security.
func generateRandomString(length int) string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("generate random string: %w", err))
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}
