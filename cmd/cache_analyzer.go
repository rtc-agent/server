package cmd

import (
	"fmt"
	"os"

	"github.com/rtc-agent/server/internal/cache_analyzer"
	"github.com/spf13/cobra"
)

var (
	analyzerPort     int
	analyzerLogFiles []string
)

var cacheAnalyzerCmd = &cobra.Command{
	Use:   "cache-analyzer",
	Short: "Start cache analyzer server",
	Long:  `Start HTTP server for analyzing LLM cache hit rates from log files`,
	Run:   runCacheAnalyzer,
}

func init() {
	rootCmd.AddCommand(cacheAnalyzerCmd)

	cacheAnalyzerCmd.Flags().IntVarP(&analyzerPort, "port", "p", 1000, "HTTP server port")
	cacheAnalyzerCmd.Flags().StringArrayVar(&analyzerLogFiles, "log", []string{}, "Log file paths (can be specified multiple times)")
}

func runCacheAnalyzer(cmd *cobra.Command, args []string) {
	if len(analyzerLogFiles) == 0 {
		fmt.Println("⚠️  Warning: No log files specified. You can add them via the web UI.")
		fmt.Println("   Or use: --log /path/to/log1.log --log /path/to/log2.log")
		fmt.Println()
	}

	// Verify log files exist
	for _, logFile := range analyzerLogFiles {
		if _, err := os.Stat(logFile); os.IsNotExist(err) {
			fmt.Printf("⚠️  Warning: Log file does not exist: %s\n", logFile)
		}
	}

	server := cache_analyzer.NewServer(analyzerPort, analyzerLogFiles)

	fmt.Printf("🚀 Cache Analyzer Server starting on http://localhost:%d\n", analyzerPort)
	fmt.Printf("📁 Log files: %v\n", analyzerLogFiles)
	fmt.Println()
	fmt.Println("Press Ctrl+C to stop")

	if err := server.Run(); err != nil {
		fmt.Printf("❌ Error starting server: %v\n", err)
		os.Exit(1)
	}
}
