package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"

	"github.com/ysya/runscaler/internal/config"
)

// checkTartSoftnet diagnoses an opted-in dependency without executing it or
// changing host permissions. Tart also supports passwordless sudo, so an
// executable without setuid-root produces guidance rather than a false failure.
func checkTartSoftnet(sets []config.ScaleSetConfig, out io.Writer) error {
	warning, err := tartSoftnetWarning(sets)
	if err != nil {
		fmt.Fprintf(out, "  ✗ %s\n", err)
		return err
	}
	if warning != "" {
		fmt.Fprintf(out, "  ⚠ %s\n", warning)
	}
	return nil
}

func checkTartSoftnetForRun(sets []config.ScaleSetConfig, logger *slog.Logger) error {
	warning, err := tartSoftnetWarning(sets)
	if err != nil {
		return err
	}
	if warning != "" {
		logger.Warn(warning)
	}
	return nil
}

func tartSoftnetWarning(sets []config.ScaleSetConfig) (string, error) {
	enabled := false
	for _, ss := range sets {
		enabled = enabled || ss.IsTart() && ss.Tart.NetSoftnet
	}
	if !enabled {
		return "", nil
	}
	path, err := exec.LookPath("softnet")
	if err != nil {
		return "", fmt.Errorf("tart Softnet is enabled but softnet is not executable on PATH; install openai/tools/softnet")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect Softnet executable: %w", err)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || info.Mode()&os.ModeSetuid == 0 {
		return "Softnet requires setuid-root or passwordless sudo; verify privileges before starting jobs", nil
	}
	return "", nil
}
