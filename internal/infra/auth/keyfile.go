// Package auth provides JWT key file management for admin-server.
package auth

import (
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureKeys checks if key files exist at the specified paths.
// If they don't exist, generates a new key pair and saves them to disk.
// Returns nil if keys already exist or were successfully generated.
func EnsureKeys(privateKeyPath, publicKeyPath, algorithm string) error {
	// Check if both keys exist
	if fileExists(privateKeyPath) && fileExists(publicKeyPath) {
		return nil
	}

	// Generate key pair
	privateKey, publicKey, err := generateKeys(algorithm)
	if err != nil {
		return fmt.Errorf("generate keys: %w", err)
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(privateKeyPath), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// Save private key
	if err := savePrivateKey(privateKeyPath, privateKey); err != nil {
		return fmt.Errorf("save private key: %w", err)
	}

	// Save public key
	if err := savePublicKey(publicKeyPath, publicKey); err != nil {
		return fmt.Errorf("save public key: %w", err)
	}

	return nil
}

// fileExists checks if a file exists at the given path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// savePrivateKey saves a private key to a PEM file with restricted permissions.
func savePrivateKey(path string, key crypto.PrivateKey) error {
	keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}

	pemBlock := &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: keyBytes,
	}
	pemData := pem.EncodeToMemory(pemBlock)

	if err := os.WriteFile(path, pemData, 0600); err != nil {
		return fmt.Errorf("write private key file: %w", err)
	}

	return nil
}

// savePublicKey saves a public key to a PEM file.
func savePublicKey(path string, key crypto.PublicKey) error {
	keyBytes, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return fmt.Errorf("marshal public key: %w", err)
	}

	pemBlock := &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: keyBytes,
	}
	pemData := pem.EncodeToMemory(pemBlock)

	if err := os.WriteFile(path, pemData, 0644); err != nil {
		return fmt.Errorf("write public key file: %w", err)
	}

	return nil
}
