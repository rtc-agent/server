// Package admin provides the admin-server cobra command and initialization.
package admin

import (
	"time"

	"github.com/spf13/cobra"
)

var (
	adminCfgFile string
)

// shutdownTimeout is the maximum duration to wait for in-flight requests
// to complete before forcing the server to shut down.
const shutdownTimeout = 5 * time.Second

// adminCmd represents the admin command
var adminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Admin server management commands",
	Long:  `RTC Agent admin server management commands (serve, account, etc.)`,
}

// init initializes the admin command
func init() {
	adminCmd.PersistentFlags().StringVar(&adminCfgFile, "config", "etc/admin.yaml", "config file (default is etc/admin.yaml)")

	// Add subcommands
	adminCmd.AddCommand(serveCmd)
	adminCmd.AddCommand(accountCmd)
	adminCmd.AddCommand(keygenCmd)
}

// GetCommand returns the admin cobra command
func GetCommand() *cobra.Command {
	return adminCmd
}
