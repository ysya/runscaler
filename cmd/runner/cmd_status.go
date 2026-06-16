package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/health"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show current runner status",
	Long:  "Query the local health check endpoint to display runner status.",
	Example: `  runner status
  runner status --health-port 9090
  runner status --json`,
	RunE: runStatus,
}

func init() {
	flags := statusCmd.Flags()
	flags.Int("health-port", config.DefaultHealthPort, "Health check server port to connect to")
	flags.Bool("json", false, "Output raw JSON")
}

func runStatus(cmd *cobra.Command, args []string) error {
	port, _ := cmd.Flags().GetInt("health-port")
	jsonOutput, _ := cmd.Flags().GetBool("json")

	url := fmt.Sprintf("http://localhost:%d/healthz", port)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("cannot connect to runner at port %d — is it running?\n\n"+
			"  Start runner first: runner run --config config.toml\n"+
			"  Or check the health port: runner run --health-port %d --config config.toml",
			port, port)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if jsonOutput {
		fmt.Println(string(body))
		return nil
	}

	var h health.HealthResponse
	if err := json.Unmarshal(body, &h); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	fmt.Printf("runner %s (uptime: %s)\n\n", h.Version, h.Uptime)

	if len(h.ScaleSets) == 0 {
		fmt.Println("No scale sets registered yet.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SCALE SET\tIDLE\tBUSY\tTOTAL")
	for _, ss := range h.ScaleSets {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\n", ss.Name, ss.Idle, ss.Busy, ss.Idle+ss.Busy)
	}
	w.Flush()

	return nil
}
