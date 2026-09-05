package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
)

var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Development utilities",
	Long:  `Development utilities for rtc-agent`,
}

var devDependenciesCmd = &cobra.Command{
	Use:   "dependencies",
	Short: "Manage development dependencies",
	Long:  `Manage development dependencies (Redis, PostgreSQL) via Docker Compose`,
}

var devDependenciesStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start development dependencies",
	Long:  `Start Redis and PostgreSQL containers via Docker Compose`,
	RunE:  runDevDependenciesStart,
}

var devDependenciesFlushallCmd = &cobra.Command{
	Use:   "flushall",
	Short: "Clear all data in development dependencies",
	Long:  `Clear all data in Redis and PostgreSQL by removing Docker volumes and restarting`,
	RunE:  runDevDependenciesFlushall,
}

func init() {
	devDependenciesCmd.AddCommand(devDependenciesStartCmd)
	devDependenciesCmd.AddCommand(devDependenciesFlushallCmd)
	devCmd.AddCommand(devDependenciesCmd)
	rootCmd.AddCommand(devCmd)
}

func getDevComposePath() (string, error) {
	// 优先使用当前工作目录（兼容从仓库根目录执行）
	cwd, err := os.Getwd()
	if err == nil {
		composePath := filepath.Join(cwd, "etc", "dev", "docker-compose.yml")
		if _, err := os.Stat(composePath); err == nil {
			return composePath, nil
		}
	}

	// 回退：使用源码文件位置定位（开发时有效）
	_, currentFile, _, ok := runtime.Caller(0)
	if ok {
		// cmd/dev.go → 项目根目录
		projectRoot := filepath.Dir(filepath.Dir(currentFile))
		composePath := filepath.Join(projectRoot, "etc", "dev", "docker-compose.yml")
		if _, err := os.Stat(composePath); err == nil {
			return composePath, nil
		}
	}

	return "", fmt.Errorf("docker-compose.yml not found (searched cwd and source location)")
}

func runDevDependenciesStart(cmd *cobra.Command, args []string) error {
	composePath, err := getDevComposePath()
	if err != nil {
		return err
	}

	fmt.Println("Starting development dependencies...")
	dc := exec.Command("docker", "compose", "-f", composePath, "up", "-d")
	dc.Stdout = os.Stdout
	dc.Stderr = os.Stderr
	if err := dc.Run(); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}

	fmt.Println("Development dependencies started successfully")
	return nil
}

func runDevDependenciesFlushall(cmd *cobra.Command, args []string) error {
	composePath, err := getDevComposePath()
	if err != nil {
		return err
	}

	fmt.Println("Stopping containers and removing volumes...")
	dc := exec.Command("docker", "compose", "-f", composePath, "down", "-v")
	dc.Stdout = os.Stdout
	dc.Stderr = os.Stderr
	if err := dc.Run(); err != nil {
		return fmt.Errorf("docker compose down: %w", err)
	}

	fmt.Println("Restarting with clean data...")
	dc = exec.Command("docker", "compose", "-f", composePath, "up", "-d")
	dc.Stdout = os.Stdout
	dc.Stderr = os.Stderr
	if err := dc.Run(); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}

	fmt.Println("All data cleared successfully")
	return nil
}
