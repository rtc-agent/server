// Package admin provides the keygen cobra command for JWT key generation.
package admin

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/spf13/cobra"
)

var (
	keygenAlgorithm string
	keygenOutputDir string
	keygenForce     bool
)

// keygenCmd represents the keygen command
var keygenCmd = &cobra.Command{
	Use:   "keygen",
	Short: "Generate JWT key pair for admin-server",
	Long: `Generate a new JWT key pair (private and public keys) for admin-server.

Supported algorithms:
  - RS256, RS384, RS512 (RSA)
  - ES256, ES384, ES512 (ECDSA)
  - EdDSA (Ed25519)

Examples:
  # Generate RS256 keys in default directory
  rtc-agent admin keygen

  # Generate ES256 keys in custom directory
  rtc-agent admin keygen --algorithm ES256 --output-dir /app/etc/keys

  # Force overwrite existing keys
  rtc-agent admin keygen --force`,
	RunE: runKeygen,
}

func init() {
	keygenCmd.Flags().StringVar(&keygenAlgorithm, "algorithm", "RS256", "JWT signing algorithm (RS256, ES256, EdDSA, etc.)")
	keygenCmd.Flags().StringVar(&keygenOutputDir, "output-dir", "etc/keys", "Output directory for key files")
	keygenCmd.Flags().BoolVar(&keygenForce, "force", false, "Force overwrite existing keys")
}

func runKeygen(cmd *cobra.Command, args []string) error {
	privatePath := filepath.Join(keygenOutputDir, "admin-private.pem")
	publicPath := filepath.Join(keygenOutputDir, "admin-public.pem")

	// Check if keys already exist
	if !keygenForce {
		if _, err := os.Stat(privatePath); err == nil {
			if _, err := os.Stat(publicPath); err == nil {
				return fmt.Errorf("keys already exist at %s (use --force to overwrite)", keygenOutputDir)
			}
		}
	}

	// Remove existing keys if force is enabled
	if keygenForce {
		if err := os.Remove(privatePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove existing private key: %w", err)
		}
		if err := os.Remove(publicPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove existing public key: %w", err)
		}
	}

	// Generate keys
	fmt.Printf("Generating %s key pair...\n", keygenAlgorithm)
	if err := auth.EnsureKeys(privatePath, publicPath, keygenAlgorithm); err != nil {
		return fmt.Errorf("generate keys: %w", err)
	}

	fmt.Printf("✓ Private key: %s\n", privatePath)
	fmt.Printf("✓ Public key:  %s\n", publicPath)
	fmt.Printf("\nKey pair generated successfully!\n")

	return nil
}
