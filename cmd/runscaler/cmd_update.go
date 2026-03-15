package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/versioncheck"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update runscaler to the latest release",
	Long: `Check GitHub for the latest release and, if newer, download and replace
the current binary in-place. The checksum is verified before installation.`,
	Example: `  runscaler update            # Update to latest release
  runscaler update --check    # Check for updates without installing`,
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().Bool("check", false, "Check for updates without installing")
	cmd.AddCommand(updateCmd)
}

func runUpdate(cmd *cobra.Command, _ []string) error {
	checkOnly, _ := cmd.Flags().GetBool("check")

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

	fmt.Fprintf(cmd.OutOrStdout(), "Updated to %s. Restart runscaler to use the new version.\n", release.TagName)
	return nil
}
