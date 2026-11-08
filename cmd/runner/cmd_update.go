package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
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
binary, or the current binary when no service is installed. Use --binary-path
for an explicit binary-only update. Checksums are verified before installation.
Configuration/provider compatibility is not validated, service definitions are
not rewritten, and services are not restarted. Existing service scope and
runtime user are retained. Prepare older settings and installations first:
` + upgradeRequirementsURL,
	Example: `  runner update            # Update to latest release
  runner update --check    # Check for updates without installing`,
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().Bool("check", false, "Check for updates without installing")
	updateCmd.Flags().String("binary-path", "", "Update only this explicit binary, without selecting or restarting a service")
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

	if !versioncheck.IsNewer(current, release.TagName) {
		fmt.Fprintf(cmd.OutOrStdout(), "Installed binary is up to date (%s).\n", normalizeVersion(current))
		if target != nil {
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

	fmt.Fprintf(cmd.OutOrStdout(), "Installed %s at %s (checksum verified); services were not restarted.\n", normalizeVersion(release.TagName), execPath)
	if target == nil {
		fmt.Fprintln(cmd.OutOrStdout(), updateRestartNotice(release.TagName, "", nil, "", nil))
		return nil
	}
	printRestartSteps(cmd, release.TagName, target, true)
	return nil
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
		"\n  2. Restart the service:\n       " + serviceCommand(target.BinaryPath, target.Scope, "restart") +
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
