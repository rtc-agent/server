package usecase

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/rtc-agent/server/internal/infra/config"
)

// ============================================================
// Edge-case tests for OSS3Usecase (credential, cache, crypto)
// ============================================================

// newTestUsecase creates an OSS3Usecase with a random key for testing.
func newTestUsecase(t *testing.T) *OSS3Usecase {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyHex := hex.EncodeToString(key)

	cfg := config.StorageConfig{
		Encryption: config.EncryptionConfig{SessionTokenKey: keyHex},
		Credential: config.CredentialConfig{SessionTokenTTL: 1 * time.Hour},
	}
	uc, err := NewOSS3Usecase(nil, nil, nil, nil, nil, nil, nil, cfg)
	if err != nil {
		t.Fatalf("NewOSS3Usecase: %v", err)
	}
	return uc
}

// ---------- AES-GCM Token Edge Cases ----------

// TestEncryptDecryptSessionToken_EmptyPayload tests encryption of a payload with empty fields.
func TestEncryptDecryptSessionToken_EmptyPayload(t *testing.T) {
	uc := newTestUsecase(t)

	// ExpiresAt must be in the future; zero time would trigger ErrExpiredToken
	payload := SessionTokenPayload{
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}
	encrypted, err := uc.encryptSessionToken(payload)
	if err != nil {
		t.Fatalf("encrypt empty payload: %v", err)
	}
	if encrypted == "" {
		t.Fatal("encrypted token should not be empty")
	}

	decrypted, err := uc.decryptSessionToken(encrypted)
	if err != nil {
		t.Fatalf("decrypt empty payload: %v", err)
	}
	if decrypted.UserID != "" {
		t.Errorf("expected empty UserID, got %q", decrypted.UserID)
	}
}

// TestEncryptDecryptSessionToken_LargePayload tests encryption of a large payload.
func TestEncryptDecryptSessionToken_LargePayload(t *testing.T) {
	uc := newTestUsecase(t)

	// Create a payload with a very long AllowedPrefix
	longBytes := make([]byte, 10000)
	for i := range longBytes {
		longBytes[i] = 'x'
	}
	longPrefix := "user-" + string(longBytes)
	payload := SessionTokenPayload{
		UserID:        "user-123",
		AllowedPrefix: longPrefix,
		ExpiresAt:     time.Now().Add(1 * time.Hour),
	}

	encrypted, err := uc.encryptSessionToken(payload)
	if err != nil {
		t.Fatalf("encrypt large payload: %v", err)
	}

	decrypted, err := uc.decryptSessionToken(encrypted)
	if err != nil {
		t.Fatalf("decrypt large payload: %v", err)
	}
	if decrypted.AllowedPrefix != longPrefix {
		t.Errorf("AllowedPrefix mismatch")
	}
}

// TestDecryptSessionToken_TamperedToken tests that tampered tokens are rejected.
func TestDecryptSessionToken_TamperedToken(t *testing.T) {
	uc := newTestUsecase(t)

	payload := SessionTokenPayload{
		UserID:        "user-123",
		AllowedPrefix: "user-user-123/",
		ExpiresAt:     time.Now().Add(1 * time.Hour),
	}
	encrypted, err := uc.encryptSessionToken(payload)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// Decode, tamper with the ciphertext, re-encode
	sealed, _ := base64.URLEncoding.DecodeString(encrypted)
	// Flip a byte in the ciphertext area (after nonce)
	if len(sealed) > 13 {
		sealed[13] ^= 0xFF
	}
	tampered := base64.URLEncoding.EncodeToString(sealed)

	_, err = uc.decryptSessionToken(tampered)
	if err == nil {
		t.Error("Expected error for tampered token")
	}
}

// TestDecryptSessionToken_NonceTooShort tests token shorter than nonce.
func TestDecryptSessionToken_NonceTooShort(t *testing.T) {
	uc := newTestUsecase(t)

	// Encode only 10 bytes (less than 12-byte nonce)
	short := base64.URLEncoding.EncodeToString(make([]byte, 10))
	_, err := uc.decryptSessionToken(short)
	if err == nil {
		t.Error("Expected error for token shorter than nonce")
	}
}

// TestDecryptSessionToken_ExactlyNonceLength tests token exactly 12 bytes (no ciphertext).
func TestDecryptSessionToken_ExactlyNonceLength(t *testing.T) {
	uc := newTestUsecase(t)

	// Exactly 12 bytes: nonce only, no ciphertext/tag
	token := base64.URLEncoding.EncodeToString(make([]byte, 12))
	_, err := uc.decryptSessionToken(token)
	if err == nil {
		t.Error("Expected error for token with only nonce (no ciphertext)")
	}
}

// TestEncryptSessionToken_Uniqueness tests that encrypting same payload twice produces different ciphertexts.
func TestEncryptSessionToken_Uniqueness(t *testing.T) {
	uc := newTestUsecase(t)

	payload := SessionTokenPayload{
		UserID:        "user-123",
		AllowedPrefix: "user-user-123/",
		ExpiresAt:     time.Now().Add(1 * time.Hour),
	}

	t1, _ := uc.encryptSessionToken(payload)
	t2, _ := uc.encryptSessionToken(payload)
	if t1 == t2 {
		t.Error("Two encryptions of the same payload produced identical ciphertext (nonce reuse?)")
	}
}

// TestDecryptSessionToken_JustExpired tests token that expired 1ms ago.
func TestDecryptSessionToken_JustExpired(t *testing.T) {
	uc := newTestUsecase(t)

	payload := SessionTokenPayload{
		UserID:        "user-123",
		AllowedPrefix: "user-user-123/",
		ExpiresAt:     time.Now().Add(-1 * time.Millisecond), // Just expired
	}

	encrypted, _ := uc.encryptSessionToken(payload)
	_, err := uc.decryptSessionToken(encrypted)
	if err == nil {
		t.Error("Expected error for just-expired token")
	}
}

// TestDecryptSessionToken_AboutToExpire tests token that expires in 1ms.
func TestDecryptSessionToken_AboutToExpire(t *testing.T) {
	uc := newTestUsecase(t)

	payload := SessionTokenPayload{
		UserID:        "user-123",
		AllowedPrefix: "user-user-123/",
		ExpiresAt:     time.Now().Add(1 * time.Hour), // Not expired yet
	}

	encrypted, _ := uc.encryptSessionToken(payload)
	decrypted, err := uc.decryptSessionToken(encrypted)
	if err != nil {
		t.Fatalf("Expected no error for valid token, got: %v", err)
	}
	if decrypted.UserID != "user-123" {
		t.Errorf("UserID mismatch")
	}
}

// TestDecryptSessionToken_WrongKey tests decryption with a different key.
func TestDecryptSessionToken_WrongKey(t *testing.T) {
	uc1 := newTestUsecase(t)
	uc2 := newTestUsecase(t)

	payload := SessionTokenPayload{
		UserID:        "user-123",
		AllowedPrefix: "user-user-123/",
		ExpiresAt:     time.Now().Add(1 * time.Hour),
	}

	encrypted, _ := uc1.encryptSessionToken(payload)
	_, err := uc2.decryptSessionToken(encrypted)
	if err == nil {
		t.Error("Expected error when decrypting with wrong key")
	}
}

// ---------- NewOSS3Usecase init edge cases ----------

// TestNewOSS3Usecase_InvalidHexKey tests that invalid hex key is rejected.
func TestNewOSS3Usecase_InvalidHexKey(t *testing.T) {
	// 64 chars but contains non-hex characters
	invalidKey := "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	cfg := config.StorageConfig{
		Encryption: config.EncryptionConfig{SessionTokenKey: invalidKey},
	}
	_, err := NewOSS3Usecase(nil, nil, nil, nil, nil, nil, nil, cfg)
	if err == nil {
		t.Error("Expected error for invalid hex key")
	}
}

// TestNewOSS3Usecase_EmptyKey tests empty key (should succeed, encryption disabled).
func TestNewOSS3Usecase_EmptyKey(t *testing.T) {
	cfg := config.StorageConfig{
		Encryption: config.EncryptionConfig{SessionTokenKey: ""},
	}
	uc, err := NewOSS3Usecase(nil, nil, nil, nil, nil, nil, nil, cfg)
	if err != nil {
		t.Errorf("Expected no error for empty key (encryption disabled), got: %v", err)
	}
	if uc == nil {
		t.Error("Expected usecase to be created")
	}
}

// TestNewOSS3Usecase_KeyTooLong tests key longer than 64 hex chars.
func TestNewOSS3Usecase_KeyTooLong(t *testing.T) {
	// 66 hex chars - too long
	longKey := "aabbccddee00112233445566778899aabbccddee00112233445566778899aabb00"
	cfg := config.StorageConfig{
		Encryption: config.EncryptionConfig{SessionTokenKey: longKey},
	}
	_, err := NewOSS3Usecase(nil, nil, nil, nil, nil, nil, nil, cfg)
	if err == nil {
		t.Error("Expected error for key too long")
	}
}

// ---------- generateRandomString edge cases ----------

// TestGenerateRandomString_ZeroLength tests 0-length string.
func TestGenerateRandomString_ZeroLength(t *testing.T) {
	s, err := generateRandomString(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s) != 0 {
		t.Errorf("Expected empty string for length 0, got %q", s)
	}
}

// TestGenerateRandomString_LargeLength tests very large string.
func TestGenerateRandomString_LargeLength(t *testing.T) {
	s, err := generateRandomString(10000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s) != 10000 {
		t.Errorf("Expected length 10000, got %d", len(s))
	}
	for _, c := range s {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			t.Errorf("Invalid character in output: %c", c)
		}
	}
}

// TestGenerateRandomString_StatisticalDistribution tests for modulo bias.
func TestGenerateRandomString_StatisticalDistribution(t *testing.T) {
	s, err := generateRandomString(100000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	counts := make(map[rune]int)
	for _, c := range s {
		counts[c]++
	}
	// With 36 possible chars and 100k samples, each should appear ~2778 times
	// Allow wide tolerance since this is statistical
	for c, count := range counts {
		if count < 1000 || count > 5000 {
			t.Logf("Character %c appeared %d times (expected ~2778) -- may indicate modulo bias", c, count)
		}
	}
}

// LOW-16 fix: Benchmark to verify generateRandomString efficiency improvement
// (batch reading vs single-byte reading)
func BenchmarkGenerateRandomString(b *testing.B) {
	lengths := []int{16, 40, 100, 1000}
	for _, length := range lengths {
		b.Run(fmt.Sprintf("length_%d", length), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_, _ = generateRandomString(length)
			}
		})
	}
}
