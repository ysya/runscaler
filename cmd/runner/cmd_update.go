package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/health"
	"github.com/ysya/runscaler/internal/versioncheck"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update runner to the latest release",
	Long: `Check GitHub for the latest release and, if newer, download and replace
the current binary in-place. The checksum is verified before installation.`,
	Example: `  runner update            # Update to latest release
  runner update --check    # Check for updates without installing`,
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().Bool("check", false, "Check for updates without installing")
	cmd.AddCommand(updateCmd)
}

func runUpdate(cmd *cobra.Command, _ []string) error {
	checkOnly, _ := cmd.Flags().GetBool("check")

	if version == "dev" {
		fmt.Fprintln(cmd.ErrOrStderr(), "Warning: running a dev build — version comparison may be inaccurate.")
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Checking for updates...")

	release, err := versioncheck.Latest(cmd.Context())
	if err != nil {
		return fmt.Errorf("could not check for updates: %w", err)
	}

	if !versioncheck.IsNewer(version, release.TagName) {
		fmt.Fprintf(cmd.OutOrStdout(), "Already up to date (%s).\n", version)
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "New version available: %s (current: %s)\n", release.TagName, version)

	if checkOnly {
		fmt.Fprintf(cmd.OutOrStdout(), "Run without --check to install.\n")
		return nil
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine executable path: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Downloading %s...\n", release.TagName)

	if err := versioncheck.Update(cmd.Context(), release.TagName, execPath); err != nil {
		return fmt.Errorf("update failed: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Updated to %s.\n", release.TagName)

	// A replaced binary does not change an already-running process. Query the
	// same local health endpoint as `runner status` and make that mismatch
	// explicit when it is observable; failure is best-effort because health may
	// be disabled or no service may be running.
	address, port := config.DefaultHealthAddress, config.DefaultHealthPort
	if cfg, cfgErr := loadConfig(cmd); cfgErr == nil {
		address, port = cfg.HealthAddress, cfg.HealthPort
	}
	running := fetchRunningVersion(cmd.Context(), address, port, &http.Client{Timeout: 2 * time.Second})
	if notice := updateRestartNotice(release.TagName, running); notice != "" {
		fmt.Fprintln(cmd.OutOrStdout(), notice)
	}
	return nil
}

// updateRestartNotice returns the actionable message shown after a successful
// update. It is empty when no running instance can be observed or when that
// instance already matches the installed binary.
func updateRestartNotice(installed, running string) string {
	if running == "" || normalizeVersion(installed) == normalizeVersion(running) {
		return ""
	}
	return fmt.Sprintf("Runner service is still running %s while the installed binary is %s. Run 'runner service restart' to apply the update safely (add sudo for a system-level service).", running, installed)
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
