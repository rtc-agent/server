package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewJWTSigner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		secret    string
		ttl       time.Duration
		expectErr bool
	}{
		{"valid", "my-secret-key", 15 * time.Minute, false},
		{"empty secret", "", 15 * time.Minute, true},
		{"zero ttl", "my-secret-key", 0, true},
		{"negative ttl", "my-secret-key", -1 * time.Minute, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			signer, err := NewJWTSigner(tt.secret, tt.ttl)
			if tt.expectErr {
				assert.Error(t, err)
				assert.Nil(t, signer)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, signer)
				assert.Equal(t, tt.ttl, signer.AccessTTL())
			}
		})
	}
}

func TestSignAndParseAccessToken(t *testing.T) {
	t.Parallel()

	signer, err := NewJWTSigner("test-secret-key-32bytes-long!!!", 15*time.Minute)
	require.NoError(t, err)

	userID := uuid.New()
	deviceID := "test-device-123"

	// Sign
	token, expiresAt, err := signer.SignAccessToken(userID, deviceID)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.False(t, expiresAt.IsZero())
	assert.True(t, expiresAt.After(time.Now()))

	// Parse
	claims, err := signer.ParseAccessToken(token)
	require.NoError(t, err)
	assert.Equal(t, userID, claims.UserID)
	assert.Equal(t, deviceID, claims.DeviceID)
	assert.Equal(t, "rtc-agent", claims.Issuer)
	assert.NotNil(t, claims.ExpiresAt)
	assert.NotNil(t, claims.IssuedAt)
}

func TestParseAccessToken_Empty(t *testing.T) {
	t.Parallel()

	signer, err := NewJWTSigner("test-secret", time.Minute)
	require.NoError(t, err)

	_, err = signer.ParseAccessToken("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestParseAccessToken_Expired(t *testing.T) {
	t.Parallel()

	// Use a very short TTL so we can wait for expiry.
	signer, err := NewJWTSigner("test-secret", 1*time.Millisecond)
	require.NoError(t, err)

	token, _, err := signer.SignAccessToken(uuid.New(), "dev")
	require.NoError(t, err)

	// Wait for token to expire.
	time.Sleep(10 * time.Millisecond)

	_, err = signer.ParseAccessToken(token)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "expired")
}

func TestParseAccessToken_WrongSecret(t *testing.T) {
	t.Parallel()

	signer1, err := NewJWTSigner("secret-one", 15*time.Minute)
	require.NoError(t, err)

	signer2, err := NewJWTSigner("secret-two", 15*time.Minute)
	require.NoError(t, err)

	// Sign with signer1, try to parse with signer2.
	token, _, err := signer1.SignAccessToken(uuid.New(), "dev")
	require.NoError(t, err)

	_, err = signer2.ParseAccessToken(token)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "signature")
}

func TestParseAccessToken_AlgorithmConfusion(t *testing.T) {
	t.Parallel()

	signer, err := NewJWTSigner("test-secret", 15*time.Minute)
	require.NoError(t, err)

	// Create a token with a non-HMAC signing method (none algorithm).
	claims := Claims{
		UserID:   uuid.New(),
		DeviceID: "dev",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "rtc-agent",
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signed, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	_, err = signer.ParseAccessToken(signed)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected signing method")
}

func TestParseAccessToken_MalformedToken(t *testing.T) {
	t.Parallel()

	signer, err := NewJWTSigner("test-secret", 15*time.Minute)
	require.NoError(t, err)

	tests := []struct {
		name  string
		token string
	}{
		{"garbage", "not-a-valid-jwt"},
		{"empty parts", "."},
		{"two parts", "a.b"},
		{"invalid base64", "!!!.!!!.!!!"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := signer.ParseAccessToken(tt.token)
			assert.Error(t, err)
		})
	}
}

func TestClassifyJWTError(t *testing.T) {
	t.Parallel()

	// Direct test with actual errors.
	assert.Equal(t, "expired", classifyJWTError(jwt.ErrTokenExpired))
	assert.Equal(t, "not_yet_valid", classifyJWTError(jwt.ErrTokenNotValidYet))
	assert.Equal(t, "issued_after", classifyJWTError(jwt.ErrTokenUsedBeforeIssued))
	assert.Equal(t, "invalid", classifyJWTError(assert.AnError))
}
