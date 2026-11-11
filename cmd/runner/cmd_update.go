package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/health"
	"github.com/ysya/runscaler/internal/versioncheck"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update runner to the latest release",
	Long: `Check GitHub for the latest release and replace the installed service's
binary, or the current binary when no service is installed. Checksums are
verified before installation. Service definitions are never rewritten, and
the existing service scope and runtime user are kept.

By default nothing is restarted: update ends with the steps that finish the
upgrade. With --restart, it validates the config with the new binary as the
service will run it and, if that passes, restarts the selected service, which
waits for running jobs up to drain-timeout. Use --binary-path for an explicit
binary-only update. Prepare older settings and installations first:
` + upgradeRequirementsURL,
	Example: `  runner update              # Update to the latest release
  runner update --restart    # Update, validate the config, restart the service
  runner update --check      # Check for updates without installing`,
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().Bool("check", false, "Check for updates without installing")
	updateCmd.Flags().String("binary-path", "", "Update only this explicit binary, without selecting or restarting a service")
	updateCmd.Flags().Bool("restart", false, "Validate the config with the new binary, then restart the selected service")
	cmd.AddCommand(updateCmd)
}

func runUpdate(cmd *cobra.Command, _ []string) error {
	checkOnly, _ := cmd.Flags().GetBool("check")
	invoking, err := updateExecutablePath()
	if err != nil {
		return fmt.Errorf("cannot determine updater path: %w", err)
	}
	execPath, target, err := resolveUpdateDestination(cmd, invoking)
	if err != nil {
		return err
	}
	restart, _ := cmd.Flags().GetBool("restart")
	if restart {
		if err := updateRestartAllowed(cmd, checkOnly, target); err != nil {
			return err
		}
	}
	if !checkOnly {
		if err := checkUpdateAccess(execPath, invoking); err != nil {
			return err
		}
	}
	current := version
	if !sameFilePath(execPath, invoking) {
		current, err = installedBinaryVersion(cmd.Context(), execPath)
		if err != nil {
			return err
		}
	}
	if target != nil {
		announceInstallation(cmd, target)
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "Update target: %s\n", execPath)
	}

	if current == "dev" {
		fmt.Fprintln(cmd.ErrOrStderr(), "Warning: running a dev build — version comparison may be inaccurate.")
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Checking for updates...")

	release, err := latestUpdateRelease(cmd.Context())
	if err != nil {
		return fmt.Errorf("could not check for updates: %w", err)
	}
	printUpdateConfigNotices(cmd, target)

	if !versioncheck.IsNewer(current, release.TagName) {
		fmt.Fprintf(cmd.OutOrStdout(), "Installed binary is up to date (%s).\n", normalizeVersion(current))
		if target != nil {
			if restart {
				return finishUpgrade(cmd, current, target, false)
			}
			printRestartSteps(cmd, current, target, false)
		}
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "New version available: %s (installed: %s)\n", normalizeVersion(release.TagName), normalizeVersion(current))
	printUpgradeNotice(cmd.OutOrStdout(), release.TagName)

	if checkOnly {
		fmt.Fprintln(cmd.OutOrStdout(), "Check only: no files changed. After reviewing the upgrade requirements, run this command without --check to install.")
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Downloading %s...\n", normalizeVersion(release.TagName))

	if err := installUpdateRelease(cmd.Context(), release.TagName, execPath); err != nil {
		return updateFailure(err, execPath, invoking)
	}

	if restart {
		fmt.Fprintf(cmd.OutOrStdout(), "Installed %s at %s (checksum verified).\n", normalizeVersion(release.TagName), execPath)
		return finishUpgrade(cmd, release.TagName, target, true)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Installed %s at %s (checksum verified); services were not restarted.\n", normalizeVersion(release.TagName), execPath)
	if target == nil {
		fmt.Fprintln(cmd.OutOrStdout(), updateRestartNotice(release.TagName, "", nil, "", nil))
		return nil
	}
	printRestartSteps(cmd, release.TagName, target, true)
	return nil
}

// printUpdateConfigNotices reports advisory migrations from the selected
// service's config before any binary replacement. Binary-only updates have no
// selected service and must not inspect an unrelated configuration.
func printUpdateConfigNotices(cmd *cobra.Command, target *installationTarget) {
	if target == nil || target.ConfigPath == "" {
		return
	}
	cfg, err := loadConfigAtPath(cmd, target.ConfigPath)
	if err != nil {
		fmt.Fprintln(cmd.OutOrStdout(), "Could not inspect the selected service config for migration notices; run runner validate before restarting it.")
		return
	}
	if len(cfg.Deprecations) == 0 {
		return
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Configuration sunset notices:")
	for _, notice := range cfg.Deprecations {
		fmt.Fprintf(cmd.OutOrStdout(), "  ⚠ %s\n", notice)
	}
}

// updateRestartAllowed checks, before anything is downloaded, that --restart
// can act: it needs a selected service and an update that installs.
func updateRestartAllowed(cmd *cobra.Command, checkOnly bool, target *installationTarget) error {
	switch {
	case checkOnly:
		return errors.New("--restart cannot be combined with --check")
	case cmd.Flags().Changed("binary-path"):
		return errors.New("--restart restarts the selected service; --binary-path updates only a binary")
	case target == nil:
		return errors.New("--restart needs an installed service, and none was found")
	}
	if err := refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "restart the runner service", pathExists); err != nil {
		return err
	}
	return refuseDarwinSystemScope(runtime.GOOS, target.Scope, "restart the runner service")
}

// finishUpgrade validates the config with the installed binary as the
// service will run it and, if that passes, restarts the service. It leaves
// alone a service whose health reports the installed version already, and,
// unless the binary just changed, one whose version it cannot observe.
func finishUpgrade(cmd *cobra.Command, installed string, target *installationTarget, changed bool) error {
	out := cmd.OutOrStdout()
	version := normalizeVersion(installed)
	cfg, err := loadConfigAtPath(cmd, target.ConfigPath)
	if err != nil {
		return fmt.Errorf("cannot load the selected config, so the service was not restarted: %w", err)
	}
	running := fetchRunningVersion(cmd.Context(), cfg.HealthAddress, cfg.HealthPort, &http.Client{Timeout: 2 * time.Second})
	switch {
	case running != "" && normalizeVersion(running) == version:
		fmt.Fprintf(out, "The service already runs %s; nothing to restart.\n", version)
		return nil
	case running == "" && !changed:
		fmt.Fprintf(out, "The running service version could not be verified (health is disabled or unavailable), so the service was not restarted. Restart it with:\n  %s\n", targetCommand(target, "restart"))
		return nil
	}
	fmt.Fprintf(out, "\nValidating the config with %s as the service will run it...\n", version)
	if err := validateAsService(cmd, target); err != nil {
		return fmt.Errorf("the config did not validate with %s, so the service was not restarted: %w", version, err)
	}
	drain := cfg.EffectiveDrainTimeout()
	fmt.Fprintf(out, "Restarting the %s service...\n%s\n", target.Scope, drainNotice(&drain))
	if err := restartService(target.Scope); err != nil {
		return fmt.Errorf("restart the %s service: %w", target.Scope, err)
	}
	fmt.Fprintf(out, "Restarted the %s service. Check it with: %s\n", target.Scope, runnerCommand{sudo: target.Scope == scopeSystem, binary: target.BinaryPath, args: []string{"status"}})
	return nil
}

// validateAsService runs `runner validate` with the installed binary as the
// service will run it. A non-root system service runs it through systemd-run
// with its identity, groups and environment file, as doctor does; any other
// service runs it as the invoking user with the service's environment.
var validateAsService = func(cmd *cobra.Command, target *installationTarget) error {
	ctx := cmd.Context()
	name, args := target.BinaryPath, []string{"validate", "--config", target.ConfigPath}
	identity := target.Scope == scopeSystem && target.RuntimeUser != "" && target.RuntimeUser != "root" && target.RuntimeUser != "0"
	if identity {
		noExpand, err := maintenanceCapabilities(ctx)
		if err != nil {
			return err
		}
		maintenance, err := buildMaintenanceArgs(target, []string{"validate"}, noExpand)
		if err != nil {
			return err
		}
		name, args = maintenance[0], maintenance[1:]
	}
	path, err := trustedExecutable(name)
	if err != nil {
		return err
	}
	c := exec.CommandContext(ctx, path, args...)
	c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
	if !identity {
		c.Env = os.Environ()
		for key, value := range target.Environment {
			c.Env = append(c.Env, key+"="+value)
		}
	}
	return c.Run()
}

// trustedExecutable resolves name for execution. As root it must resolve to
// a file only root can modify, as service maintenance tools must.
func trustedExecutable(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	if os.Geteuid() != 0 {
		return path, nil
	}
	if path, err = filepath.EvalSymlinks(path); err != nil {
		return "", err
	}
	if err := checkRootOnlyChain(path, lstatFile); err != nil {
		return "", fmt.Errorf("untrusted executable %s: %w", name, err)
	}
	return path, nil
}

// restartService restarts the selected service through its service manager,
// as `runner service restart` does.
var restartService = func(scope serviceScope) error {
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.restart(scope)
}

// printRestartSteps tells how to finish an upgrade the selected service does
// not run yet. A replaced binary does not change an already-running process,
// so this asks the same local health endpoint as `runner status` which
// version runs. That is best-effort: health may be disabled, or no service
// may be running. unknownMatters says whether to speak up when the running
// version cannot be observed.
func printRestartSteps(cmd *cobra.Command, installed string, target *installationTarget, unknownMatters bool) {
	out := cmd.OutOrStdout()
	running := ""
	var drain *time.Duration
	if cfg, err := loadConfigAtPath(cmd, target.ConfigPath); err == nil {
		running = fetchRunningVersion(cmd.Context(), cfg.HealthAddress, cfg.HealthPort, &http.Client{Timeout: 2 * time.Second})
		budget := cfg.EffectiveDrainTimeout()
		drain = &budget
	} else {
		fmt.Fprintln(out, "The selected config could not be loaded by this updater; validate it with the new binary before restarting.")
	}
	if running == "" && !unknownMatters {
		return
	}
	if notice := updateRestartNotice(installed, running, target, validateCommand(cmd.Context(), target), drain); notice != "" {
		fmt.Fprintln(out)
		fmt.Fprintln(out, notice)
	}
}

// validateCommand renders how to validate target's config with the binary
// just installed, as the service will run it. A non-root system service
// needs its own identity, groups and environment file, which systemd-run
// reproduces as it does for doctor; a root shell would check other
// credentials and access.
func validateCommand(ctx context.Context, target *installationTarget) string {
	if target.Scope == scopeSystem && target.RuntimeUser != "" && target.RuntimeUser != "root" && target.RuntimeUser != "0" {
		if hint, err := maintenanceHint(ctx, target, []string{"validate"}); err == nil {
			return hint
		}
	}
	return runnerCommand{sudo: target.Scope == scopeSystem, binary: target.BinaryPath, args: []string{"validate", "--config", target.ConfigPath}}.String()
}

// updateFailure adds a sudo hint when the binary lives in a directory only
// root can write, as a system service's binary must.
func updateFailure(err error, execPath, updater string) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("update failed: %w\n\n  %s is not writable by this user; %s", err, filepath.Dir(execPath), updatePermissionHint(updater, execPath, lstatFile))
	}
	return fmt.Errorf("update failed: %w", err)
}

// Unknown health is not proof that a service stopped. Explicit binary-only
// updates never print commands that control an unrelated installed service.
func updateRestartNotice(installed, running string, target *installationTarget, validate string, drain *time.Duration) string {
	if target == nil {
		return "No service was selected. After validating its configuration, restart any process using this binary manually; service state was not checked."
	}
	if normalizeVersion(installed) == normalizeVersion(running) {
		return ""
	}
	header := fmt.Sprintf("The service is still running %s. To finish upgrading to %s:", normalizeVersion(running), normalizeVersion(installed))
	if running == "" {
		header = fmt.Sprintf("The running service version could not be verified (health is disabled or unavailable). If the service is running, finish upgrading to %s:", normalizeVersion(installed))
	}
	return header +
		"\n  1. Validate the config with the new binary:\n       " + validate +
		"\n  2. Restart the service:\n       " + targetCommand(target, "restart") +
		"\n" + drainNotice(drain)
}

// drainNotice says how long a restart waits for running jobs; drain is nil
// when the config could not be read.
func drainNotice(drain *time.Duration) string {
	switch {
	case drain == nil:
		return "Restarting waits for running jobs up to drain-timeout, unless the service manager's stop timeout is shorter."
	case *drain <= 0:
		return "drain-timeout is disabled, so restarting stops running jobs at once."
	}
	return fmt.Sprintf("Restarting waits up to %s for running jobs to finish (drain-timeout), unless the service manager's stop timeout is shorter.", shortDuration(*drain))
}

// shortDuration drops a duration's zero minutes and seconds: 2h, 1h30m, 45s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func normalizeVersion(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "v")
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// fetchRunningVersion returns the version exposed by the local health
// endpoint, or an empty string when no instance can be observed. Update is
// already complete by the time this runs, so every failure is deliberately
// non-fatal.
func fetchRunningVersion(ctx context.Context, address string, port int, client httpDoer) string {
	if port <= 0 {
		return ""
	}
	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(address, fmt.Sprintf("%d", port)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var status health.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return ""
	}
	return strings.TrimSpace(status.Version)
}
