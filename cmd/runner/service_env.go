package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/spf13/viper"
	"github.com/ysya/runscaler/internal/layout"
	"github.com/ysya/runscaler/internal/shell"
)

const (
	serviceVersionEnv     = "RUNNER_SERVICE_VERSION"
	serviceStopTimeoutEnv = "RUNNER_SERVICE_STOP_TIMEOUT"
	serviceScopeEnv       = layout.ServiceScopeEnv
	// currentServiceVersion is the template generation that records the
	// variables above; bump it whenever generated definitions change.
	currentServiceVersion = 3
)

// serviceManagedEnvKeys are the only inline Environment= entries runner
// writes into a definition. Anything else in an installed unit is operator
// data that regeneration and maintenance must not copy around.
var serviceManagedEnvKeys = []string{"HOME", serviceScopeEnv, serviceVersionEnv, serviceStopTimeoutEnv, "XDG_CONFIG_HOME", "XDG_STATE_HOME"}

// startedByServiceManager reports whether systemd, or launchd under runner's
// current label, started this process. A macOS terminal sets
// XPC_SERVICE_NAME=0, so the label must match exactly.
func startedByServiceManager(getenv func(string) string) bool {
	if getenv("INVOCATION_ID") != "" {
		return true
	}
	return getenv("XPC_SERVICE_NAME") == launchdLabel
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

// serviceFixCommand regenerates a current service with its exact config and
// binary. On Linux a system service may only run a root-protected binary, so
// an untrusted one is first copied to rootBinaryPath.
func serviceFixCommand(goos string, scope serviceScope, configPath, binaryPath string, stat statFunc) string {
	if goos == "linux" && scope == scopeSystem && binaryPath != rootBinaryPath && checkRootOnlyChain(binaryPath, stat) != nil {
		// One line, so it works in a log attribute and a terminal alike.
		return "sudo install -m 0755 " + shell.Word(binaryPath) + " " + rootBinaryPath + " && " +
			serviceInstallCommand(rootBinaryPath, scopeSystem, configPath, true, false, nil)
	}
	return serviceInstallCommand(binaryPath, scope, configPath, true, false, nil)
}

// currentServiceFixCommand is serviceFixCommand for this process.
func currentServiceFixCommand() string {
	return serviceFixCommand(runtime.GOOS, scopeFromIdentity(layout.CurrentIdentity()), absConfigPath(viper.ConfigFileUsed()), currentBinaryPath(), lstatFile)
}

// scopeFromIdentity maps the running identity onto an installation scope: a
// system layout regenerates the system service, anything else the user's.
func scopeFromIdentity(id layout.Identity) serviceScope {
	if id.System() {
		return scopeSystem
	}
	return scopeUser
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
		logger.Warn(w, slog.String("fix", fix), slog.String("upgrade_requirements", upgradeRequirementsURL))
	}
}
