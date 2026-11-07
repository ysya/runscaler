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
		fmt.Fprintf(cmd.OutOrStdout(), "Installed binary is up to date (%s). Running service version, readiness and configuration compatibility were not checked.\n", current)
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "New version available: %s (current: %s)\n", release.TagName, current)
	printUpgradeNotice(cmd.OutOrStdout(), release.TagName)

	if checkOnly {
		fmt.Fprintln(cmd.OutOrStdout(), "Check only: no files changed. After reviewing the upgrade requirements, run this command without --check to install.")
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Downloading %s...\n", release.TagName)

	if err := installUpdateRelease(cmd.Context(), release.TagName, execPath); err != nil {
		return updateFailure(err, execPath, invoking)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Binary installed: %s at %s. Configuration/provider compatibility has not been validated; no services were restarted.\n", release.TagName, execPath)
	if target == nil {
		fmt.Fprintln(cmd.OutOrStdout(), updateRestartNotice(release.TagName, "", nil))
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Before restarting, validate the configuration with the new binary in the service's runtime environment (credentials, provider access and groups):\n  %s validate --config %s\n", shellQuotePath(execPath), shellQuotePath(target.ConfigPath))

	// A replaced binary does not change an already-running process. Query the
	// same local health endpoint as `runner status` and make that mismatch
	// explicit when it is observable; failure is best-effort because health may
	// be disabled or no service may be running.
	running := ""
	if cfg, cfgErr := loadConfigAtPath(cmd, target.ConfigPath); cfgErr == nil {
		running = fetchRunningVersion(cmd.Context(), cfg.HealthAddress, cfg.HealthPort, &http.Client{Timeout: 2 * time.Second})
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "The selected config could not be loaded by this updater; validate it with the newly installed binary before restarting.")
	}
	if notice := updateRestartNotice(release.TagName, running, target); notice != "" {
		fmt.Fprintln(cmd.OutOrStdout(), notice)
	}
	return nil
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
func updateRestartNotice(installed, running string, target *installationTarget) string {
	if target == nil {
		return "No service was selected. After validating its configuration, restart any process using this binary manually; service state was not checked."
	}
	if normalizeVersion(installed) == normalizeVersion(running) {
		return ""
	}
	command := shellQuotePath(target.BinaryPath) + " service restart --scope " + string(target.Scope)
	prefix := fmt.Sprintf("The selected service still reports %s while the installed binary is %s. After preparing and validating the upgrade, restart it with:", running, installed)
	if running == "" {
		prefix = "The running service version could not be verified (health is disabled or unavailable). If the selected service is running, restart it only after preparing and validating the upgrade:"
	}
	if target.Scope == scopeSystem {
		prefix += "\nUse an administrative shell for this system service."
	}
	return prefix + "\n  " + command + "\nReview drain-timeout and the service stop timeout first; jobs may be terminated if drain is disabled or a deadline expires."
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
