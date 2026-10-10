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

// skipExtensions controls whether CREATE EXTENSION statements are executed.
// When true, DBAs are expected to have pre-created required extensions
// (e.g. pgvector) manually. Useful in locked-down environments where the
// migration user lacks superuser privileges.
var skipExtensions bool

func init() {
	rootCmd.AddCommand(migrateCmd)
	migrateCmd.Flags().BoolVar(&skipExtensions, "skip-extensions", false,
		"skip CREATE EXTENSION statements (DBA must pre-create extensions like pgvector)")
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
	//
	// IMPORTANT: GORM uses connection pooling, and SET statement_timeout only affects the current
	// connection. To ensure all migrations run on the same connection with the timeout disabled,
	// we temporarily set MaxOpenConns to 1 (single connection mode) during migration.
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("get underlying sql.DB: %w", err)
	}

	// Store original MaxOpenConns to restore later
	originalMaxOpenConns := sqlDB.Stats().MaxOpenConnections

	// Force single connection mode to ensure statement_timeout applies to all migrations
	sqlDB.SetMaxOpenConns(1)
	defer func() {
		// Restore original MaxOpenConns after migration completes
		sqlDB.SetMaxOpenConns(originalMaxOpenConns)
	}()

	// Now set statement_timeout = 0 on the single connection
	if err := db.Exec("SET statement_timeout = 0").Error; err != nil {
		logger.Warn(context.Background(), "migrate.set_statement_timeout_failed", zap.Error(err))
	}

	// Ensure pgvector extension is created (pgvector/pgvector:pg17 image has it installed, but it must be explicitly enabled).
	// This is a hard error because subsequent migrations may depend on vector types.
	//
	// Note: CREATE EXTENSION requires superuser privileges or the CREATEROLE attribute.
	// If this fails with "permission denied", ensure the database user has the necessary
	// privileges, or have a DBA create the extension manually before running migrations.
	if skipExtensions {
		logger.Info(context.Background(), "migrate.skip_extensions",
			zap.String("reason", "skipping CREATE EXTENSION as requested via --skip-extensions flag; DBA must pre-create required extensions"))
	} else if err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector").Error; err != nil {
		return fmt.Errorf("failed to create pgvector extension.\n\n"+
			"This extension requires database superuser privileges.\n\n"+
			"Solutions:\n"+
			"  1. Run migration with a superuser account (recommended for initial setup).\n"+
			"  2. Have a DBA execute: CREATE EXTENSION IF NOT EXISTS vector;\n"+
			"  3. Or skip extension creation if it already exists: --skip-extensions\n\n"+
			"Original error: %v", err)
	}

	if err := model.AutoMigrate(db); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	// Migrate admin-server tables
	// Migration order is important due to foreign key dependencies:
	// 1. AdminUser - base user table (no dependencies)
	// 2. AdminRole - role definitions (no dependencies)
	// 3. AdminRefreshToken - references AdminUser
	// 4. AdminUserRole - references both AdminUser and AdminRole
	// 5. AuditLog - standalone audit table
	// 6. ServerConfig/ServerConfigHistory - dynamic configuration (standalone)
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

	// ServerConfig needs special handling: migrate from composite PK to surrogate PK.
	// WARNING: This migration performs DDL operations (ALTER COLUMN SET NOT NULL, ADD PRIMARY KEY)
	// that require ACCESS EXCLUSIVE locks on the table. On large tables, this can block all reads
	// and writes during migration. For tables with >1M rows, consider breaking this into multiple
	// smaller migrations or using pg_repack for online table reorganization.
	logger.Info(context.Background(), "migrate.server_config_table.starting",
		zap.String("warning", "This operation may block reads/writes on large tables due to ACCESS EXCLUSIVE locks"))
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
	logger.Info(context.Background(), "migrate.creating_index", zap.String("index", "uniq_server_configs_key_system"))
	if err := db.Exec(`
		CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uniq_server_configs_key_system
		ON server_configs(key)
		WHERE user_id IS NULL
	`).Error; err != nil {
		return fmt.Errorf("create unique index for system configs: %w (hint: check for duplicate system config keys)", err)
	}
	logger.Info(context.Background(), "migrate.index_created", zap.String("index", "uniq_server_configs_key_system"))
	// Index 2: Ensures at most one user override per (key, user_id) combination.
	logger.Info(context.Background(), "migrate.creating_index", zap.String("index", "uniq_server_configs_key_user"))
	if err := db.Exec(`
		CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uniq_server_configs_key_user
		ON server_configs(key, user_id)
		WHERE user_id IS NOT NULL
	`).Error; err != nil {
		return fmt.Errorf("create unique index for user configs: %w (hint: check for duplicate user config keys)", err)
	}
	logger.Info(context.Background(), "migrate.index_created", zap.String("index", "uniq_server_configs_key_user"))

	// Composite index for admin session listing, token aggregation stats, and queries
	// filtering by owner_kind + owner_ref_id with ordering/limit by created_at.
	// This single 3-column index replaces the previous 2-column idx_sessions_owner
	// and idx_sessions_owner_created indexes, reducing index maintenance overhead.
	logger.Info(context.Background(), "migrate.creating_index", zap.String("index", "idx_sessions_owner_created"))
	if err := db.Exec(`
		CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_sessions_owner_created
		ON sessions(owner_kind, owner_ref_id, created_at)
	`).Error; err != nil {
		return fmt.Errorf("create index for sessions: %w", err)
	}
	logger.Info(context.Background(), "migrate.index_created", zap.String("index", "idx_sessions_owner_created"))

	// Composite index for server_config_history to optimize queries by (key, user_id, version).
	// Used by GetHistoryByVersion and other history queries.
	// This index enables efficient lookups for specific config versions and audit trails.
	logger.Info(context.Background(), "migrate.creating_index", zap.String("index", "idx_server_config_history_key_user_version"))
	if err := db.Exec(`
		CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_server_config_history_key_user_version
		ON server_config_history(key, user_id, version DESC)
	`).Error; err != nil {
		return fmt.Errorf("create index for server_config_history: %w", err)
	}
	logger.Info(context.Background(), "migrate.index_created", zap.String("index", "idx_server_config_history_key_user_version"))

	return nil
}

// runOwnerMigrations executes owner migration stages (1-4) with checkpoint checks
// to avoid re-running completed work.
func runOwnerMigrations(ctx context.Context, db *gorm.DB) error {
	// Check if stages 1+2 are needed by looking for rows with NULL owner fields.
	// This is more reliable than checking for backfilled data, because it directly
	// answers "are there rows that still need migration?" rather than "did we ever
	// migrate anything?" (which could be true even if migration was incomplete).
	var incompleteCount int64
	if err := db.Raw(`
		SELECT COUNT(*) FROM sessions
		WHERE owner_kind IS NULL OR owner_ref_id IS NULL
	`).Scan(&incompleteCount).Error; err != nil {
		return fmt.Errorf("check owner migration checkpoint: %w", err)
	}
	if incompleteCount > 0 {
		logger.Info(ctx, "owner.migration.running_stages_1_and_2",
			zap.Int64("incomplete_rows", incompleteCount))
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
