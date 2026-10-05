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

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
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

// bindRoleCmd represents the bind-role command
var bindRoleCmd = &cobra.Command{
	Use:   "bind-role",
	Short: "Bind a role to a user",
	Long:  `Bind a role (e.g., admin, operator, viewer) to an existing user by email`,
	Run:   runBindRole,
}

var (
	createEmail    string
	createPassword string
	createName     string
	createRole     string

	bindRoleEmail string
	bindRoleName  string
)

func init() {
	createCmd.Flags().StringVar(&createEmail, "email", "", "User email (required)")
	createCmd.Flags().StringVar(&createPassword, "password", "", "User password (required)")
	createCmd.Flags().StringVar(&createName, "name", "", "User display name (optional)")
	createCmd.Flags().StringVar(&createRole, "role", "", "Role name to bind after creation (optional, e.g., admin, operator, viewer)")

	_ = createCmd.MarkFlagRequired("email")
	_ = createCmd.MarkFlagRequired("password")

	// bind-role command
	bindRoleCmd.Flags().StringVar(&bindRoleEmail, "email", "", "User email (required)")
	bindRoleCmd.Flags().StringVar(&bindRoleName, "role", "", "Role name to bind (required, e.g., admin, operator, viewer)")
	_ = bindRoleCmd.MarkFlagRequired("email")
	_ = bindRoleCmd.MarkFlagRequired("role")

	accountCmd.AddCommand(createCmd)
	accountCmd.AddCommand(bindRoleCmd)
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

	// Bind role if specified
	if createRole != "" {
		fmt.Printf("\n🔗 Binding role: %s\n", createRole)

		// Find role
		roleRepo := repo.NewRoleRepo(db)
		role, err := roleRepo.GetByName(context.Background(), createRole)
		if err != nil {
			fmt.Printf("⚠️  Role not found: %s (user created without role)\n", createRole)
			return
		}

		// Create user_role record
		userRole := &model.UserRole{
			UserID:     user.ID,
			RoleID:     role.ID,
			AssignedAt: time.Now(),
		}
		if err := db.Create(userRole).Error; err != nil {
			fmt.Printf("⚠️  Failed to bind role: %v (user created without role)\n", err)
			return
		}

		// Add Casbin grouping policy
		enforcer, err := auth.NewCasbinEnforcer(db)
		if err != nil {
			fmt.Printf("⚠️  Failed to create enforcer: %v (role bound in DB but not in Casbin)\n", err)
			return
		}

		if err := enforcer.AddGroupingPolicy(context.Background(), user.ID.String(), role.ID.String()); err != nil {
			fmt.Printf("⚠️  Failed to add Casbin policy: %v (role bound in DB but not in Casbin)\n", err)
			return
		}

		fmt.Printf("✅ Role bound successfully: %s\n", createRole)
		fmt.Printf("   Please restart the server to reload permissions.\n")
	}
}

func runBindRole(cmd *cobra.Command, args []string) {
	// Load admin config
	cfg, err := config.LoadAdminConfig(adminCfgFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load admin config: %v\n", err)
		os.Exit(1)
	}

	// Init logger
	logger.Init("info", "")
	defer logger.Sync()

	ctx := context.Background()

	// Init database
	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger: logger.NewGormLogger(false, 200*time.Millisecond),
	})
	if err != nil {
		logger.Fatal(context.Background(), "admin.database_connection_failed", zap.Error(err))
	}

	// Find user
	var user model.User
	if err := db.Where("email = ?", bindRoleEmail).First(&user).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			logger.Error(context.Background(), "admin.user_not_found", zap.String("email", bindRoleEmail))
			fmt.Fprintf(os.Stderr, "❌ User not found: %s\n", bindRoleEmail)
			os.Exit(1)
		}
		logger.Error(context.Background(), "admin.find_user_failed", zap.Error(err))
		fmt.Fprintf(os.Stderr, "❌ Failed to find user: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Found user: %s (ID: %s)\n", user.Email, user.ID)

	// Find role
	roleRepo := repo.NewRoleRepo(db)
	role, err := roleRepo.GetByName(ctx, bindRoleName)
	if err != nil {
		logger.Error(context.Background(), "admin.role_not_found", zap.String("role", bindRoleName))
		fmt.Fprintf(os.Stderr, "❌ Role not found: %s\n", bindRoleName)
		os.Exit(1)
	}
	fmt.Printf("✅ Found role: %s (ID: %s)\n", role.DisplayName, role.ID)

	// Check if already assigned
	var count int64
	db.Model(&model.UserRole{}).Where("user_id = ? AND role_id = ?", user.ID, role.ID).Count(&count)
	if count > 0 {
		fmt.Printf("⚠️  User already has role: %s\n", bindRoleName)
		return
	}

	// Create user_role record
	userRole := &model.UserRole{
		UserID:     user.ID,
		RoleID:     role.ID,
		AssignedAt: time.Now(),
	}
	if err := db.Create(userRole).Error; err != nil {
		logger.Error(context.Background(), "admin.create_user_role_failed", zap.Error(err))
		fmt.Fprintf(os.Stderr, "❌ Failed to create user_role: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Created user_role record\n")

	// Add Casbin grouping policy
	enforcer, err := auth.NewCasbinEnforcer(db)
	if err != nil {
		logger.Error(context.Background(), "admin.create_enforcer_failed", zap.Error(err))
		fmt.Fprintf(os.Stderr, "❌ Failed to create enforcer: %v\n", err)
		os.Exit(1)
	}

	if err := enforcer.AddGroupingPolicy(ctx, user.ID.String(), role.ID.String()); err != nil {
		logger.Error(context.Background(), "admin.add_casbin_policy_failed", zap.Error(err))
		fmt.Fprintf(os.Stderr, "❌ Failed to add Casbin policy: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Added Casbin grouping policy\n")

	fmt.Printf("\n🎉 Success! User %s now has role: %s\n", user.Email, bindRoleName)
	fmt.Printf("   Please restart the server to reload permissions.\n")
}
