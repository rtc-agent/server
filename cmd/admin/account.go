// Package admin provides the admin-server cobra command and initialization.
package admin

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
)

// accountCmd represents the account command
var accountCmd = &cobra.Command{
	Use:   "account",
	Short: "Account management commands",
	Long:  `Manage admin user accounts (create, list, etc.)`,
}

// createCmd represents the create command
var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new admin user",
	Long:  `Create a new admin user with email and password`,
	Run:   runCreate,
}

var (
	createEmail    string
	createPassword string
	createName     string
)

func init() {
	createCmd.Flags().StringVar(&createEmail, "email", "", "User email (required)")
	createCmd.Flags().StringVar(&createPassword, "password", "", "User password (required)")
	createCmd.Flags().StringVar(&createName, "name", "", "User display name (optional)")

	_ = createCmd.MarkFlagRequired("email")
	_ = createCmd.MarkFlagRequired("password")

	accountCmd.AddCommand(createCmd)
}

func runCreate(cmd *cobra.Command, args []string) {
	// Load admin config
	cfg, err := config.LoadAdminConfig(adminCfgFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load admin config: %v\n", err)
		os.Exit(1)
	}

	// Init logger
	logger.Init("info", "")
	defer logger.Sync()

	// Init database
	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger: logger.NewGormLogger(false, 200*time.Millisecond),
	})
	if err != nil {
		logger.Fatal(context.Background(), "admin.database_connection_failed", zap.Error(err))
	}

	// Check if user already exists
	var count int64
	db.Model(&model.User{}).Where("email = ?", createEmail).Count(&count)
	if count > 0 {
		logger.Error(context.Background(), "admin.user_already_exists", zap.String("email", createEmail))
		os.Exit(1)
	}

	// Hash password
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(createPassword), bcrypt.DefaultCost)
	if err != nil {
		logger.Fatal(context.Background(), "admin.password_hash_failed", zap.Error(err))
	}

	// Create user
	user := &model.User{
		Email:        createEmail,
		Name:         createName,
		PasswordHash: string(passwordHash),
	}

	if err := db.Create(user).Error; err != nil {
		logger.Fatal(context.Background(), "admin.user_creation_failed", zap.Error(err))
	}

	logger.Info(context.Background(), "admin.user_created_successfully",
		zap.String("email", createEmail),
		zap.String("name", createName),
		zap.String("id", user.ID.String()))

	fmt.Printf("✅ User created successfully!\n")
	fmt.Printf("   Email: %s\n", createEmail)
	fmt.Printf("   Name:  %s\n", createName)
	fmt.Printf("   ID:    %s\n", user.ID.String())
}
