package cmd

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
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

	logger.Init(cfg.Log.Level)
	defer logger.Sync()

	logger.Info(context.Background(), "Running database migration...")

	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger: logger.NewGormLogger(
			false,        // 迁移时不忽略 ErrRecordNotFound
			200*time.Millisecond,
		),
	})
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}

	// 确保 pgvector 扩展已创建（pgvector/pgvector:pg17 镜像已安装扩展，但需显式启用）
	if err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector").Error; err != nil {
		logger.Warn(context.Background(), "Failed to create pgvector extension (vector search may not work)", zap.Error(err))
	}

	if err := model.AutoMigrate(db); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

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
