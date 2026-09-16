package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const (
	serviceVersionEnv     = "RUNNER_SERVICE_VERSION"
	serviceStopTimeoutEnv = "RUNNER_SERVICE_STOP_TIMEOUT"
	// currentServiceVersion is the template generation that records the two
	// variables above; bump it whenever generated definitions change.
	currentServiceVersion = 2
)

// startedByServiceManager reports whether systemd, or launchd under runner's
// current or legacy label, started this process. A macOS terminal sets
// XPC_SERVICE_NAME=0, so the label must match exactly.
func startedByServiceManager(getenv func(string) string) bool {
	if getenv("INVOCATION_ID") != "" {
		return true
	}
	switch getenv("XPC_SERVICE_NAME") {
	case launchdLabel, legacyLaunchdLabel:
		return true
	}
	return false
}

// outdatedServiceWarnings reads the template generation and stop timeout that
// generated service definitions record. It does not audit hand-edited
// definitions.
func outdatedServiceWarnings(getenv func(string) string, drainTimeout time.Duration) []string {
	if !startedByServiceManager(getenv) {
		return nil
	}
	var warnings []string
	version, err := strconv.Atoi(getenv(serviceVersionEnv))
	if err != nil {
		version = 1
	}
	if version < currentServiceVersion {
		warnings = append(warnings, "Service definition predates this runner's template (log directory, lock access, stop timeout)")
	}
	if seconds, err := strconv.Atoi(getenv(serviceStopTimeoutEnv)); err == nil {
		need := serviceStopTimeout(&drainTimeout)
		if time.Duration(seconds)*time.Second < need {
			warnings = append(warnings, fmt.Sprintf("Service stop timeout %ds is shorter than drain-timeout plus one minute (%s); a restart could kill in-flight jobs", seconds, need))
		}
	}
	return warnings
}

// serviceReinstallCommand regenerates the service in place with the config
// and binary this process actually uses.
func serviceReinstallCommand(root bool, configPath, binaryPath string) string {
	var parts []string
	if root {
		parts = append(parts, "sudo")
	}
	parts = append(parts, shellQuotePath(binaryPath), "service", "install")
	if root {
		parts = append(parts, "--user=false")
	} else {
		parts = append(parts, "--user")
	}
	parts = append(parts, "--force")
	if configPath != "" {
		parts = append(parts, "--config-path", shellQuotePath(configPath))
	}
	parts = append(parts, "--binary-path", shellQuotePath(binaryPath))
	return strings.Join(parts, " ")
}

// currentBinaryPath is the resolved executable, or "runner" when unknown.
func currentBinaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "runner"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

// absConfigPath makes a config path usable from any working directory.
func absConfigPath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// serviceFixCommand is the command that regenerates the definition of the
// service this process runs under. A pre-rename definition is migrated
// rather than reinstalled beside it, and a root service whose binary others
// could replace is first given a root-only copy.
func serviceFixCommand(goos string, root bool, configPath, binaryPath string, legacyInstalled bool, stat statFunc) string {
	switch {
	case legacyInstalled && root:
		return "sudo " + shellQuotePath(binaryPath) + " migrate --user=false"
	case legacyInstalled:
		return shellQuotePath(binaryPath) + " migrate --user"
	case goos == "linux" && root && checkRootOnlyChain(binaryPath, stat) != nil:
		// One line, so it works in a log attribute and a terminal alike.
		return "sudo install -m 0755 " + shellQuotePath(binaryPath) + " " + rootBinaryPath + " && " +
			serviceReinstallCommand(true, configPath, rootBinaryPath)
	default:
		return serviceReinstallCommand(root, configPath, binaryPath)
	}
}

// currentServiceFixCommand is serviceFixCommand for this process.
func currentServiceFixCommand() string {
	return serviceFixCommand(runtime.GOOS, os.Geteuid() == 0, absConfigPath(viper.ConfigFileUsed()), currentBinaryPath(), legacyServiceInstalled(os.Geteuid() != 0), lstatFile)
}

// warnOutdatedService logs each reason to regenerate the service together
// with the single command that does it.
func warnOutdatedService(logger *slog.Logger, drainTimeout time.Duration) {
	warnings := outdatedServiceWarnings(os.Getenv, drainTimeout)
	if len(warnings) == 0 {
		return
	}
	fix := currentServiceFixCommand()
	for _, w := range warnings {
		logger.Warn(w, slog.String("fix", fix))
	}
}
