package usecase

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"github.com/rtc-agent/server/internal/infra/config"
)

// TestOSS3Usecase_Init tests OSS3Usecase initialization with AES-GCM.
func TestOSS3Usecase_Init(t *testing.T) {
	t.Run("ValidKey", func(t *testing.T) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatalf("generate key: %v", err)
		}
		keyHex := hex.EncodeToString(key)

		cfg := config.StorageConfig{
			Encryption: config.EncryptionConfig{
				SessionTokenKey: keyHex,
			},
		}

		uc, err := NewOSS3Usecase(nil, nil, nil, nil, nil, nil, nil, cfg)
		if err != nil {
			t.Fatalf("NewOSS3Usecase: %v", err)
		}
		if uc == nil {
			t.Fatal("NewOSS3Usecase returned nil")
		}
		if uc.aesGCM == nil {
			t.Fatal("aesGCM not initialized")
		}
	})

	t.Run("InvalidKeyLength", func(t *testing.T) {
		cfg := config.StorageConfig{
			Encryption: config.EncryptionConfig{
				SessionTokenKey: "short-key",
			},
		}

		_, err := NewOSS3Usecase(nil, nil, nil, nil, nil, nil, nil, cfg)
		if err == nil {
			t.Fatal("Expected error for invalid key length")
		}
	})
}

// TestEncryptDecryptSessionToken tests SessionToken encryption and decryption.
func TestEncryptDecryptSessionToken(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyHex := hex.EncodeToString(key)

	cfg := config.StorageConfig{
		Encryption: config.EncryptionConfig{
			SessionTokenKey: keyHex,
		},
	}

	uc, err := NewOSS3Usecase(nil, nil, nil, nil, nil, nil, nil, cfg)
	if err != nil {
		t.Fatalf("NewOSS3Usecase: %v", err)
	}

	t.Run("EncryptDecrypt", func(t *testing.T) {
		payload := SessionTokenPayload{
			UserID:        "user-123",
			AllowedPrefix: "user-user-123/",
			ExpiresAt:     time.Now().Add(1 * time.Hour),
		}

		encrypted, err := uc.encryptSessionToken(payload)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		if encrypted == "" {
			t.Fatal("encrypted token is empty")
		}

		// Verify it's base64 encoded
		sealed, err := base64.URLEncoding.DecodeString(encrypted)
		if err != nil {
			t.Fatalf("base64 decode: %v", err)
		}
		// nonce(12) + ciphertext + tag(16)
		if len(sealed) < 28 {
			t.Fatalf("sealed token too short: %d", len(sealed))
		}

		decrypted, err := uc.decryptSessionToken(encrypted)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if decrypted.UserID != payload.UserID {
			t.Errorf("UserID: got %q, want %q", decrypted.UserID, payload.UserID)
		}
		if decrypted.AllowedPrefix != payload.AllowedPrefix {
			t.Errorf("AllowedPrefix: got %q, want %q", decrypted.AllowedPrefix, payload.AllowedPrefix)
		}
	})

	t.Run("ExpiredToken", func(t *testing.T) {
		payload := SessionTokenPayload{
			UserID:        "user-456",
			AllowedPrefix: "user-user-456/",
			ExpiresAt:     time.Now().Add(-1 * time.Hour), // Already expired
		}

		encrypted, err := uc.encryptSessionToken(payload)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}

		_, err = uc.decryptSessionToken(encrypted)
		if err == nil {
			t.Fatal("Expected error for expired token")
		}
	})

	t.Run("InvalidToken", func(t *testing.T) {
		_, err := uc.decryptSessionToken("invalid-token")
		if err == nil {
			t.Fatal("Expected error for invalid token")
		}
	})
}

// TestGenerateRandomString tests random string generation.
func TestGenerateRandomString(t *testing.T) {
	t.Run("Length", func(t *testing.T) {
		s, err := generateRandomString(20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(s) != 20 {
			t.Errorf("length: got %d, want 20", len(s))
		}
	})

	t.Run("Uniqueness", func(t *testing.T) {
		s1, err := generateRandomString(40)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		s2, err := generateRandomString(40)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s1 == s2 {
			t.Error("Generated two identical strings")
		}
	})

	t.Run("Charset", func(t *testing.T) {
		s, err := generateRandomString(100)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, c := range s {
			if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
				t.Errorf("Invalid character: %c", c)
			}
		}
	})
}
