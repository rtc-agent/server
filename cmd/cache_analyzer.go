package cmd

import (
	"fmt"
	"os"

	"github.com/rtc-agent/server/internal/cache_analyzer"
	"github.com/spf13/cobra"
)

var analyzerPort int

var cacheAnalyzerCmd = &cobra.Command{
	Use:   "cache-analyzer",
	Short: "Start cache analyzer server",
	Long:  `Start HTTP server for analyzing LLM cache hit rates from log files`,
	Run:   runCacheAnalyzer,
}

func init() {
	rootCmd.AddCommand(cacheAnalyzerCmd)

	cacheAnalyzerCmd.Flags().IntVarP(&analyzerPort, "port", "p", 1000, "HTTP server port")
}

func runCacheAnalyzer(cmd *cobra.Command, args []string) {
	server := cache_analyzer.NewServer(analyzerPort, nil)

	fmt.Printf("🚀 Cache Analyzer Server starting on http://localhost:%d\n", analyzerPort)
	fmt.Println()
	fmt.Println("Press Ctrl+C to stop")

	if err := server.Run(); err != nil {
		fmt.Printf("❌ Error starting server: %v\n", err)
		os.Exit(1)
	}
}
