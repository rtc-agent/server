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

	// Disable statement_timeout for migration operations.
	// Production DBAs often set statement_timeout (e.g., 30s) to prevent long-running queries,
	// but CREATE INDEX CONCURRENTLY on large tables can legitimately take longer.
	// Setting it to 0 (unlimited) ensures migration completes without artificial timeout.
	// This only affects the current session/connection.
	if err := db.Exec("SET statement_timeout = 0").Error; err != nil {
		logger.Warn(context.Background(), "migrate.set_statement_timeout_failed", zap.Error(err))
	}

	// Ensure pgvector extension is created (pgvector/pgvector:pg17 image has it installed, but it must be explicitly enabled).
	// This is a hard error because subsequent migrations may depend on vector types.
	if err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector").Error; err != nil {
		return fmt.Errorf("create pgvector extension: %w (hint: install pgvector or use pgvector/pgvector Docker image)", err)
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

	ctx := context.Background()

	if err := createMigrationIndexes(db); err != nil {
		return err
	}

	// Bootstrap default roles and Casbin policies (idempotent).
	// This ensures default roles exist even when only `migrate` is run without `serve`.
	// If Casbin enforcer initialization fails (e.g., table not yet created), we skip
	// bootstrap here — the serve command will retry on startup.
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
	configRepo := repo.NewServerConfigRepo(db)
	auditLogRepo := repo.NewAuditLogRepo(db)
	oauth2UserRepo := repo.NewOAuth2UserRepo(db)
	serverConfigUsecase := usecase.NewServerConfigUsecase(configRepo, auditLogRepo, oauth2UserRepo, db)
	if err := serverConfigUsecase.BootstrapDynamicConfigs(ctx, uuid.Nil); err != nil {
		return fmt.Errorf("bootstrap dynamic configs: %w", err)
	}

	if err := runOwnerMigrations(ctx, db); err != nil {
		return err
	}

	logger.Info(context.Background(), "Database migration completed successfully")
	return nil
}

// createMigrationIndexes creates partial unique indexes and composite indexes
// needed for the application. All indexes use CONCURRENTLY to avoid table locks.
func createMigrationIndexes(db *gorm.DB) error {
	// Create partial unique indexes for server_configs to enforce (key, user_id) uniqueness.
	// Two indexes are needed because PostgreSQL NULL != NULL, so a single unique index
	// would allow multiple NULL user_id rows for the same key.
	// Index 1: Ensures at most one system config (user_id IS NULL) per key.
	// CONCURRENTLY avoids table lock during index creation.
	if err := db.Exec(`
		CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uniq_server_configs_key_system
		ON server_configs(key)
		WHERE user_id IS NULL
	`).Error; err != nil {
		return fmt.Errorf("create unique index for system configs: %w", err)
	}
	// Index 2: Ensures at most one user override per (key, user_id) combination.
	if err := db.Exec(`
		CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uniq_server_configs_key_user
		ON server_configs(key, user_id)
		WHERE user_id IS NOT NULL
	`).Error; err != nil {
		return fmt.Errorf("create unique index for user configs: %w", err)
	}

	// Composite index for admin session listing, token aggregation stats, and queries
	// filtering by owner_kind + owner_ref_id with ordering/limit by created_at.
	// This single 3-column index replaces the previous 2-column idx_sessions_owner
	// and idx_sessions_owner_created indexes, reducing index maintenance overhead.
	if err := db.Exec(`
		CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_sessions_owner_created
		ON sessions(owner_kind, owner_ref_id, created_at)
	`).Error; err != nil {
		return fmt.Errorf("create index for sessions: %w", err)
	}

	return nil
}

// runOwnerMigrations executes owner migration stages (1-4) with checkpoint checks
// to avoid re-running completed work.
func runOwnerMigrations(ctx context.Context, db *gorm.DB) error {
	// Check if stages 1+2 are already done by looking for backfilled data.
	var hasBackfilledData bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1 FROM sessions
			WHERE owner_kind IS NOT NULL AND owner_ref_id IS NOT NULL
			LIMIT 1
		)
	`).Scan(&hasBackfilledData).Error; err != nil {
		return fmt.Errorf("check owner migration checkpoint: %w", err)
	}
	if !hasBackfilledData {
		logger.Info(ctx, "owner.migration.running_stages_1_and_2")
		if err := model.MigrateOwnerStages1And2(db); err != nil {
			return fmt.Errorf("migrate owner stages 1+2: %w", err)
		}
	} else {
		logger.Info(ctx, "owner.migration.stages_1_and_2_already_completed")
	}

	// Verify backfill completeness and run stages 3-4.
	if err := verifyAndRunOwnerStages3And4(ctx, db); err != nil {
		return err
	}

	return nil
}

// verifyAndRunOwnerStages3And4 verifies backfill completeness and runs owner
// migration stages 3 (NOT NULL constraints) and 4 (legacy column removal).
func verifyAndRunOwnerStages3And4(ctx context.Context, db *gorm.DB) error {
	// Verify backfill completeness before Stage 3 (setting NOT NULL constraints).
	// If Stage 2 partially succeeded then crashed, we must ensure all rows are backfilled
	// before attempting to add NOT NULL constraints, otherwise Stage 3 will fail.
	var sessionNullCount int64
	if err := db.Raw(`
		SELECT COUNT(*) FROM sessions
		WHERE owner_kind IS NULL OR owner_ref_id IS NULL
	`).Scan(&sessionNullCount).Error; err != nil {
		return fmt.Errorf("verify session backfill completeness: %w", err)
	}

	var messageNullCount int64
	if err := db.Raw(`
		SELECT COUNT(*) FROM messages
		WHERE creator_kind IS NULL OR creator_ref_id IS NULL
	`).Scan(&messageNullCount).Error; err != nil {
		return fmt.Errorf("verify message backfill completeness: %w", err)
	}

	if sessionNullCount > 0 || messageNullCount > 0 {
		// Backfill is incomplete, run Stages 1+2 again to complete it.
		// This is safe because the backfill uses WHERE ... IS NULL conditions,
		// so it only affects rows that haven't been backfilled yet.
		logger.Info(ctx, "owner.migration.backfill_incomplete_rerunning",
			zap.Int64("sessions_null_count", sessionNullCount),
			zap.Int64("messages_null_count", messageNullCount))
		if err := model.MigrateOwnerStages1And2(db); err != nil {
			return fmt.Errorf("complete owner backfill: %w", err)
		}
	}

	// Run stage 3 (NOT NULL constraints) if not already done.
	if err := runOwnerStage3IfNeeded(ctx, db); err != nil {
		return err
	}

	// Run stage 4 (legacy column removal) if not already done.
	if err := runOwnerStage4IfNeeded(ctx, db); err != nil {
		return err
	}

	return nil
}

// runOwnerStage3IfNeeded checks if stage 3 is done and runs it if needed.
func runOwnerStage3IfNeeded(ctx context.Context, db *gorm.DB) error {
	var hasNotNullConstraint bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'sessions' AND column_name = 'owner_kind' AND is_nullable = 'NO'
		)
	`).Scan(&hasNotNullConstraint).Error; err != nil {
		return fmt.Errorf("check owner migration stage 3 checkpoint: %w", err)
	}
	if !hasNotNullConstraint {
		logger.Info(ctx, "owner.migration.running_stage_3")
		if err := model.MigrateOwnerStage3(db); err != nil {
			return fmt.Errorf("migrate owner stage 3: %w", err)
		}
	} else {
		logger.Info(ctx, "owner.migration.stage_3_already_completed")
	}
	return nil
}

// runOwnerStage4IfNeeded checks if stage 4 is done and runs it if needed.
func runOwnerStage4IfNeeded(ctx context.Context, db *gorm.DB) error {
	var hasLegacyColumn bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'sessions' AND column_name = 'user_id'
		)
	`).Scan(&hasLegacyColumn).Error; err != nil {
		return fmt.Errorf("check owner migration stage 4 checkpoint: %w", err)
	}
	if hasLegacyColumn {
		logger.Info(ctx, "owner.migration.running_stage_4")
		if err := model.MigrateOwnerStage4(db); err != nil {
			return fmt.Errorf("migrate owner stage 4: %w", err)
		}
	} else {
		logger.Info(ctx, "owner.migration.stage_4_already_completed")
	}
	return nil
}
