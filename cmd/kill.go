package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

var (
	killServeFlag      bool
	killMockOAuth2Flag bool
	killAllFlag        bool
)

var killCmd = &cobra.Command{
	Use:   "kill",
	Short: "Stop running rtc-agent processes",
	Long: `Stop running rtc-agent processes by sending SIGTERM (graceful shutdown).

Examples:
  rtc-agent kill --serve           # stop the serve process
  rtc-agent kill --mock-oauth2     # stop the mock-oauth2 process
  rtc-agent kill --all             # stop both`,
	RunE: runKill,
}

func init() {
	killCmd.Flags().BoolVar(&killServeFlag, "serve", false, "Stop the serve process")
	killCmd.Flags().BoolVar(&killMockOAuth2Flag, "mock-oauth2", false, "Stop the mock-oauth2 process")
	killCmd.Flags().BoolVar(&killAllFlag, "all", false, "Stop all rtc-agent processes")
	rootCmd.AddCommand(killCmd)
}

func runKill(cmd *cobra.Command, args []string) error {
	if killAllFlag {
		killServeFlag = true
		killMockOAuth2Flag = true
	}
	if !killServeFlag && !killMockOAuth2Flag {
		return cmd.Help()
	}

	type target struct {
		name    string
		subcmd  string
		enabled bool
	}
	targets := []target{
		{name: "serve", subcmd: "serve", enabled: killServeFlag},
		{name: "mock-oauth2", subcmd: "mock-oauth2", enabled: killMockOAuth2Flag},
	}

	for _, t := range targets {
		if !t.enabled {
			continue
		}
		pids, err := findProcesses(t.subcmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lookup %s: %v\n", t.name, err)
			continue
		}
		if len(pids) == 0 {
			fmt.Printf("No running %s process found\n", t.name)
			continue
		}
		if err := signalProcesses(pids); err != nil {
			fmt.Fprintf(os.Stderr, "kill %s: %v\n", t.name, err)
			continue
		}
		fmt.Printf("Sent SIGTERM to %s process (pid %s)\n", t.name, joinInts(pids))
	}
	return nil
}

// findProcesses locates rtc-agent processes running the given subcommand by
// matching the current binary name + subcommand against the full cmdline via
// pgrep(1). The current process is excluded.
func findProcesses(subcmd string) ([]int, error) {
	bin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("executable: %w", err)
	}
	binName := strings.TrimSuffix(filepath.Base(bin), ".exe")
	pattern := regexp.QuoteMeta(binName) + `[[:space:]]+` + regexp.QuoteMeta(subcmd) + `([[:space:]]|$)`

	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// pgrep returns 1 when nothing matched.
			return nil, nil
		}
		return nil, fmt.Errorf("pgrep: %w", err)
	}

	self := os.Getpid()
	var pids []int
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil || pid == self {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// signalProcesses sends SIGTERM to each pid so the existing graceful-shutdown
// handlers in serve / mock-oauth2 can run. "process already finished" is
// treated as success.
func signalProcesses(pids []int) error {
	var firstErr error
	for _, pid := range pids {
		p, err := os.FindProcess(pid)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := p.Signal(syscall.SIGTERM); err != nil {
			if strings.Contains(err.Error(), "already finished") {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func joinInts(xs []int) string {
	ss := make([]string, len(xs))
	for i, x := range xs {
		ss[i] = strconv.Itoa(x)
	}
	return strings.Join(ss, ", ")
}
