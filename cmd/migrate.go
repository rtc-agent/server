package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"

	"github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database schema migrations",
	Long:  `Run database auto-migration separately from the serve command. This is the recommended way to apply schema changes in production.`,
	RunE:  runMigrate,
}

func init() {
	rootCmd.AddCommand(migrateCmd)
}

func runMigrate(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger.Init(cfg.Log.Level, cfg.Log.ServerLogFile)
	defer logger.Sync()

	logger.Info(context.Background(), "Running database migration...")

	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger: logger.NewGormLogger(
			false, // do not ignore ErrRecordNotFound during migration
			200*time.Millisecond,
		),
	})
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}

	// Ensure pgvector extension is created (pgvector/pgvector:pg17 image has it installed, but it must be explicitly enabled).
	if err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector").Error; err != nil {
		logger.Warn(context.Background(), "Failed to create pgvector extension (vector search may not work)", zap.Error(err))
	}

	if err := model.AutoMigrate(db); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	// Migrate admin-server tables
	if err := db.AutoMigrate(
		&model.AdminUser{},
		&model.AdminRefreshToken{},
		&model.AdminRole{},
		&model.AdminUserRole{},
		&model.AuditLog{},
		&model.ServerConfig{},        // 动态配置表
		&model.ServerConfigHistory{}, // 配置变更历史
	); err != nil {
		return fmt.Errorf("admin auto migrate: %w", err)
	}

	// ServerConfig needs special handling: migrate from composite PK to surrogate PK
	if err := model.MigrateServerConfigTable(db); err != nil {
		return fmt.Errorf("migrate server_configs table: %w", err)
	}

	// Create partial unique indexes for server_configs to enforce (key, user_id) uniqueness.
	// Two indexes are needed because PostgreSQL NULL != NULL, so a single unique index
	// would allow multiple NULL user_id rows for the same key.
	// Index 1: Ensures at most one system config (user_id IS NULL) per key.
	if err := db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS uniq_server_configs_key_system
		ON server_configs(key)
		WHERE user_id IS NULL
	`).Error; err != nil {
		return fmt.Errorf("create unique index for system configs: %w", err)
	}
	// Index 2: Ensures at most one user override per (key, user_id) combination.
	if err := db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS uniq_server_configs_key_user
		ON server_configs(key, user_id)
		WHERE user_id IS NOT NULL
	`).Error; err != nil {
		return fmt.Errorf("create unique index for user configs: %w", err)
	}

	// Composite index for admin session listing and token aggregation stats.
	if err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_sessions_owner_created
		ON sessions(owner_ref_id, created_at)
	`).Error; err != nil {
		return fmt.Errorf("create index for sessions: %w", err)
	}

	// Bootstrap default roles and Casbin policies (idempotent).
	// This ensures default roles exist even when only `migrate` is run without `serve`.
	// If Casbin enforcer initialization fails (e.g., table not yet created), we skip
	// bootstrap here — the serve command will retry on startup.
	ctx := context.Background()
	enforcer, err := auth.NewCasbinEnforcer(db)
	if err != nil {
		logger.Warn(ctx, "casbin_enforcer_init_failed_skipping_bootstrap", zap.Error(err))
	} else {
		roleRepo := repo.NewAdminRoleRepo(db)
		if err := usecase.BootstrapAdmin(ctx, db, roleRepo, enforcer); err != nil {
			logger.Warn(ctx, "bootstrap_admin_failed_will_retry_on_serve", zap.Error(err))
		}
	}

	// Bootstrap dynamic configs from registry (idempotent).
	// This ensures "out-of-box" experience: new deployments have all configs ready for editing.
	configRepo := repo.NewConfigRepo(db)
	auditLogRepo := repo.NewAuditLogRepo(db)
	oauth2UserRepo := repo.NewOAuth2UserRepo(db)
	serverConfigUsecase := usecase.NewServerConfigUsecase(configRepo, auditLogRepo, oauth2UserRepo, db)
	if err := serverConfigUsecase.BootstrapDynamicConfigs(ctx, uuid.Nil); err != nil {
		logger.Warn(ctx, "bootstrap_dynamic_configs_failed", zap.Error(err))
	}

	// Run owner migration stages
	if err := model.MigrateOwnerStages1And2(db); err != nil {
		return fmt.Errorf("migrate owner stages 1+2: %w", err)
	}

	if err := model.MigrateOwnerStage3(db); err != nil {
		return fmt.Errorf("migrate owner stage 3: %w", err)
	}

	if err := model.MigrateOwnerStage4(db); err != nil {
		return fmt.Errorf("migrate owner stage 4: %w", err)
	}

	logger.Info(context.Background(), "Database migration completed successfully")
	return nil
}
