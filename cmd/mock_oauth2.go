package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/rtc-agent/server/internal/oauth"
)

// mockOAuth2Cmd is the Mock OAuth2 server command.
var mockOAuth2Cmd = &cobra.Command{
	Use:   "mock-oauth2",
	Short: "Start a mock OAuth2 server for development",
	Long: `Start a simplified mock OAuth2 server for development and testing.

Features:
- HTML authorization page (enter User ID, click authorize)
- Authorization code generation and verification
- Client ID / client secret validation for token exchange

All data is stored in memory and cleared on restart. Development environment only.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Override parent command's PersistentPreRunE: this command uses an independent
		// viper instance to load config, avoiding coupling with the main server's global viper.
		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		// Config file path: defaults to mock-oauth2-specific config; uses explicit path when --config is set.
		configPath := "etc/mock-oauth2.yaml"
		if f := cmd.Flags().Lookup("config"); f != nil && f.Changed {
			configPath = cfgFile
		}
		runMockOAuth2(configPath)
	},
}

func init() {
	rootCmd.AddCommand(mockOAuth2Cmd)
}

// runMockOAuth2 starts the Mock OAuth2 server.
func runMockOAuth2(configPath string) {
	// Load config (independent viper instance).
	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetDefault("server.port", "10060")
	v.SetDefault("client_id", "test-client")
	v.SetDefault("client_secret", "test-client-secret")

	if err := v.ReadInConfig(); err != nil {
		log.Fatalf("failed to read config: %v", err)
	}

	// Create Mock OAuth2 Provider.
	provider := oauth.NewProvider(oauth.Config{
		ClientID:     v.GetString("client_id"),
		ClientSecret: v.GetString("client_secret"),
	})

	// Register routes.
	mux := http.NewServeMux()
	provider.RegisterRoutes(mux)

	// Health check endpoint.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "OK")
	})

	// CORS middleware: allow all origins (Mock environment only).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		// Preflight requests return 204 directly.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})

	// Create HTTP Server.
	port := v.GetString("server.port")
	srv := &http.Server{
		Addr:    "0.0.0.0:" + port,
		Handler: handler,
	}

	// Start HTTP Server.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Mock OAuth2 Server panic: %v\n%s", r, debug.Stack())
			}
		}()
		log.Printf("Mock OAuth2 Server listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen failed: %v", err)
		}
	}()

	// Wait for shutdown signal.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Mock OAuth2 Server shutting down...")

	// Graceful shutdown.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown failed: %v", err)
		return
	}

	log.Println("Mock OAuth2 Server exited")
}
