package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/ysya/runscaler/internal/shell"
	"github.com/ysya/runscaler/internal/versioncheck"
	"golang.org/x/sys/unix"
)

var latestUpdateRelease = versioncheck.Latest
var installUpdateRelease = versioncheck.Update
var updateExecutablePath = os.Executable

var installedBinaryVersion = func(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "version", "--json")
	// Version inspection must not forward credentials to another executable.
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	data, err := command.Output()
	if err != nil {
		return "", errors.New("cannot inspect the installed runner version; verify the selected binary")
	}
	var info versionInfo
	if json.Unmarshal(data, &info) != nil || strings.TrimSpace(info.Version) == "" {
		return "", errors.New("installed runner returned invalid version metadata")
	}
	return info.Version, nil
}

func resolveUpdateDestination(c *cobra.Command, invoking string) (string, *installationTarget, error) {
	explicit, _ := c.Flags().GetString("binary-path")
	var target *installationTarget
	path := invoking
	if explicit != "" {
		scope, err := requestedScope(c)
		if err != nil {
			return "", nil, err
		}
		if scope != scopeAuto || (c.Flags().Lookup("scope") != nil && c.Flags().Changed("scope")) {
			return "", nil, errors.New("--binary-path cannot be combined with a scope override")
		}
		path = explicit
	} else {
		var err error
		target, err = commandInstallation(c)
		if err != nil {
			return "", nil, err
		}
		if target != nil {
			path = target.BinaryPath
		}
		configPath, _ := c.Flags().GetString("config")
		if err := explicitConfigConflict(configPath, target); err != nil {
			return "", nil, err
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, fmt.Errorf("resolve update target: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, errors.New("update target is not a regular executable")
	}
	if os.Geteuid() == 0 {
		updater, err := filepath.EvalSymlinks(invoking)
		if err != nil {
			return "", nil, err
		}
		if err := checkRootOnlyChain(updater, lstatFile); err != nil {
			return "", nil, fmt.Errorf("run a root-protected current updater before updating system binaries: %w", err)
		}
		if err := checkRootOnlyChain(resolved, lstatFile); err != nil {
			return "", nil, fmt.Errorf("root updates require a root-only target: %w", err)
		}
	}
	return resolved, target, nil
}

func checkUpdateAccess(path, updater string) error {
	if err := unix.Access(filepath.Dir(path), unix.W_OK|unix.X_OK); err != nil {
		return fmt.Errorf("update target %s is not writable: %w\n\n  %s", path, os.ErrPermission, updatePermissionHint(updater, path, lstatFile))
	}
	return nil
}

func updatePermissionHint(updater, target string, stat statFunc) string {
	guidance := "\n  Review upgrade requirements first: " + upgradeRequirementsURL + "\n  --binary-path updates only the named binary; validate its configuration before manually restarting any process that uses it."
	if err := checkRootOnlyChain(updater, stat); err != nil {
		return fmt.Sprintf("Current updater: %s\n  Selected binary: %s\n  Install the current updater at a root-protected absolute path, then run that updater with sudo and --binary-path %s", updater, target, shell.Word(target)) + guidance
	}
	return "Run: " + runnerCommand{sudo: true, binary: updater, args: []string{"update", "--binary-path", target}}.String() + guidance
}
