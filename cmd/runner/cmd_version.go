package main

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Long:  `Display the version, commit hash, build date, and runtime info.`,
	Example: `  runscaler version            # Show version info
  runscaler version --short    # Print version number only
  runscaler version --json     # Output as JSON`,
	RunE: runVersion,
}

func init() {
	versionCmd.Flags().Bool("json", false, "Output as JSON")
	versionCmd.Flags().Bool("short", false, "Print version number only")
}

type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Date    string `json:"date,omitempty"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func runVersion(cmd *cobra.Command, _ []string) error {
	jsonOutput, _ := cmd.Flags().GetBool("json")
	short, _ := cmd.Flags().GetBool("short")
	if short {
		fmt.Fprintln(cmd.OutOrStdout(), version)
		return nil
	}

	info := versionInfo{
		Version: version,
		Commit:  commit,
		Date:    date,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}

	if jsonOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "runscaler %s\n", info.Version)
	if info.Commit != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "  commit: %s\n", info.Commit)
	}
	if info.Date != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "  built:  %s\n", info.Date)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "  go:     %s\n", info.Go)
	fmt.Fprintf(cmd.OutOrStdout(), "  os:     %s/%s\n", info.OS, info.Arch)

	return nil
}
