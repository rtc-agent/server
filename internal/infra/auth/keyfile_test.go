package auth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureKeys_GeneratesNewKeys(t *testing.T) {
	tests := []struct {
		name      string
		algorithm string
	}{
		{"RS256", "RS256"},
		{"ES256", "ES256"},
		{"ES384", "ES384"},
		{"EdDSA", "EdDSA"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			privatePath := filepath.Join(tmpDir, "private.pem")
			publicPath := filepath.Join(tmpDir, "public.pem")

			err := EnsureKeys(privatePath, publicPath, tt.algorithm)
			require.NoError(t, err)

			// Verify files exist
			assert.FileExists(t, privatePath)
			assert.FileExists(t, publicPath)

			// Verify private key permissions (0600)
			privInfo, err := os.Stat(privatePath)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0600), privInfo.Mode().Perm())

			// Verify public key permissions (0644)
			pubInfo, err := os.Stat(publicPath)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0644), pubInfo.Mode().Perm())
		})
	}
}

func TestEnsureKeys_DoesNotOverwriteExisting(t *testing.T) {
	tmpDir := t.TempDir()
	privatePath := filepath.Join(tmpDir, "private.pem")
	publicPath := filepath.Join(tmpDir, "public.pem")

	// Generate initial keys
	err := EnsureKeys(privatePath, publicPath, "RS256")
	require.NoError(t, err)

	// Read original keys
	originalPrivate, err := os.ReadFile(privatePath)
	require.NoError(t, err)
	originalPublic, err := os.ReadFile(publicPath)
	require.NoError(t, err)

	// Call EnsureKeys again
	err = EnsureKeys(privatePath, publicPath, "RS256")
	require.NoError(t, err)

	// Verify keys are not overwritten
	newPrivate, err := os.ReadFile(privatePath)
	require.NoError(t, err)
	newPublic, err := os.ReadFile(publicPath)
	require.NoError(t, err)

	assert.Equal(t, originalPrivate, newPrivate)
	assert.Equal(t, originalPublic, newPublic)
}

func TestEnsureKeys_CreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	privatePath := filepath.Join(tmpDir, "subdir", "nested", "private.pem")
	publicPath := filepath.Join(tmpDir, "subdir", "nested", "public.pem")

	err := EnsureKeys(privatePath, publicPath, "RS256")
	require.NoError(t, err)

	assert.FileExists(t, privatePath)
	assert.FileExists(t, publicPath)
}

func TestEnsureKeys_InvalidAlgorithm(t *testing.T) {
	tmpDir := t.TempDir()
	privatePath := filepath.Join(tmpDir, "private.pem")
	publicPath := filepath.Join(tmpDir, "public.pem")

	err := EnsureKeys(privatePath, publicPath, "INVALID")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported algorithm")
}

func TestEnsureKeys_GeneratedKeysCanBeLoaded(t *testing.T) {
	tests := []struct {
		name      string
		algorithm string
		keyType   string
	}{
		{"RS256", "RS256", "rsa"},
		{"ES256", "ES256", "ecdsa"},
		{"ES384", "ES384", "ecdsa"},
		{"EdDSA", "EdDSA", "ed25519"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			privatePath := filepath.Join(tmpDir, "private.pem")
			publicPath := filepath.Join(tmpDir, "public.pem")

			// Generate keys
			err := EnsureKeys(privatePath, publicPath, tt.algorithm)
			require.NoError(t, err)

			// Load keys using loadKeys
			privateKey, publicKey, err := loadKeys(privatePath, publicPath, tt.algorithm)
			require.NoError(t, err)

			// Verify key types
			switch tt.keyType {
			case "rsa":
				assert.IsType(t, &rsa.PrivateKey{}, privateKey)
				assert.IsType(t, &rsa.PublicKey{}, publicKey)
			case "ecdsa":
				assert.IsType(t, &ecdsa.PrivateKey{}, privateKey)
				assert.IsType(t, &ecdsa.PublicKey{}, publicKey)
			case "ed25519":
				assert.IsType(t, ed25519.PrivateKey{}, privateKey)
				assert.IsType(t, ed25519.PublicKey{}, publicKey)
			}
		})
	}
}

func TestEnsureKeys_GeneratedKeysCanSignAndVerify(t *testing.T) {
	tmpDir := t.TempDir()
	privatePath := filepath.Join(tmpDir, "private.pem")
	publicPath := filepath.Join(tmpDir, "public.pem")

	// Generate keys
	err := EnsureKeys(privatePath, publicPath, "RS256")
	require.NoError(t, err)

	// Create signer with generated keys
	signer, err := NewAdminJWTSigner(AdminJWTConfig{
		Algorithm:      "RS256",
		Issuer:         "test-issuer",
		Audience:       "test-audience",
		PrivateKeyPath: privatePath,
		PublicKeyPath:  publicPath,
	})
	require.NoError(t, err)

	// Sign a token
	userID, err := uuid.Parse("12345678-1234-1234-1234-123456789012")
	require.NoError(t, err)
	token, _, err := signer.SignAccessToken(userID, "test@example.com", "Test User")
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	// Verify the token
	claims, err := signer.ParseAccessToken(token)
	require.NoError(t, err)
	assert.Equal(t, userID, claims.UserID)
	assert.Equal(t, "test@example.com", claims.Email)
	assert.Equal(t, "Test User", claims.Name)
}

func TestSavePrivateKey_UnsupportedKeyType(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "key.pem")

	err := savePrivateKey(path, "invalid-key")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "marshal private key")
}

func TestSavePublicKey_UnsupportedKeyType(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "key.pem")

	err := savePublicKey(path, "invalid-key")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "marshal public key")
}

func TestFileExists(t *testing.T) {
	tmpDir := t.TempDir()
	existingFile := filepath.Join(tmpDir, "exists.txt")
	nonExistingFile := filepath.Join(tmpDir, "does-not-exist.txt")

	// Create a file
	err := os.WriteFile(existingFile, []byte("test"), 0644)
	require.NoError(t, err)

	assert.True(t, fileExists(existingFile))
	assert.False(t, fileExists(nonExistingFile))
}

func TestPEMFormat(t *testing.T) {
	tmpDir := t.TempDir()
	privatePath := filepath.Join(tmpDir, "private.pem")
	publicPath := filepath.Join(tmpDir, "public.pem")

	// Generate RS256 keys
	err := EnsureKeys(privatePath, publicPath, "RS256")
	require.NoError(t, err)

	// Read and parse private key PEM
	privPEM, err := os.ReadFile(privatePath)
	require.NoError(t, err)
	privBlock, _ := pem.Decode(privPEM)
	require.NotNil(t, privBlock)
	assert.Equal(t, "PRIVATE KEY", privBlock.Type)

	// Verify it can be parsed as PKCS8
	_, err = x509.ParsePKCS8PrivateKey(privBlock.Bytes)
	require.NoError(t, err)

	// Read and parse public key PEM
	pubPEM, err := os.ReadFile(publicPath)
	require.NoError(t, err)
	pubBlock, _ := pem.Decode(pubPEM)
	require.NotNil(t, pubBlock)
	assert.Equal(t, "PUBLIC KEY", pubBlock.Type)

	// Verify it can be parsed as PKIX
	_, err = x509.ParsePKIXPublicKey(pubBlock.Bytes)
	require.NoError(t, err)
}
