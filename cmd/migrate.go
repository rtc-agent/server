package cmd

import (
	"fmt"

	"github.com/rtc-agent/server/internal/config"
	"github.com/rtc-agent/server/internal/dbmodel"
	"github.com/rtc-agent/server/pkg/logger"

	"github.com/spf13/cobra"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
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

	logger.Info("Running database migration...")

	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}

	if err := dbmodel.AutoMigrate(db); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	if err := dbmodel.MigrateOwnerStages1And2(db); err != nil {
		return fmt.Errorf("migrate owner stages 1+2: %w", err)
	}

	if err := dbmodel.MigrateOwnerStage3(db); err != nil {
		return fmt.Errorf("migrate owner stage 3: %w", err)
	}

	if err := dbmodel.MigrateOwnerStage4(db); err != nil {
		return fmt.Errorf("migrate owner stage 4: %w", err)
	}

	logger.Info("Database migration completed successfully")
	return nil
}
