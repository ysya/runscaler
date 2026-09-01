package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/health"
	"github.com/ysya/runscaler/internal/provider"
)

var statusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Show current runner status",
	SilenceErrors: true,
	SilenceUsage:  true,
	Long: `Query the local health endpoint and show runner capacity, scale-set
connections, job activity, queue depth, and filesystem headroom.`,
	Example: `  runner status
  runner status --health-port 9090
  runner status --no-color
  runner status --json`,
	RunE: runStatus,
}

func init() {
	flags := statusCmd.Flags()
	flags.Int("health-port", config.DefaultHealthPort, "Health check server port to connect to")
	flags.String("health-address", config.DefaultHealthAddress, "Health check server address to connect to")
	flags.Bool("json", false, "Output raw JSON")
	flags.Bool("no-color", false, "Disable color output")
}

func runStatus(cmd *cobra.Command, _ []string) error {
	port, _ := cmd.Flags().GetInt("health-port")
	address, _ := cmd.Flags().GetString("health-address")
	jsonOutput, _ := cmd.Flags().GetBool("json")
	noColor, _ := cmd.Flags().GetBool("no-color")
	if cfg, err := loadConfig(cmd); err == nil {
		if !cmd.Flags().Changed("health-port") {
			port = cfg.HealthPort
		}
		if !cmd.Flags().Changed("health-address") {
			address = cfg.HealthAddress
		}
	} else {
		return err
	}
	if port <= 0 {
		return fmt.Errorf("health endpoint is disabled (health-port = 0)")
	}

	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(address, fmt.Sprintf("%d", port)))
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create health request: %w", err)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
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
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runner health endpoint returned HTTP %d", resp.StatusCode)
	}

	out := cmd.OutOrStdout()
	if jsonOutput {
		if _, err := out.Write(body); err != nil {
			return fmt.Errorf("write status JSON: %w", err)
		}
		if len(body) == 0 || body[len(body)-1] != '\n' {
			fmt.Fprintln(out)
		}
		return nil
	}

	var h health.HealthResponse
	if err := json.Unmarshal(body, &h); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	color := statusColorEnabled(out, noColor)
	fmt.Fprint(out, formatStatus(h, url, time.Now(), color))
	return nil
}

// statusTotals is the dashboard-wide rollup of the per-scale-set health data.
// MetricsReported is tracked separately because an absent Metrics object means
// "not reported yet", not a real collection of zero values.
type statusTotals struct {
	Ready           int
	Idle            int
	Busy            int
	Desired         int
	AvailableJobs   int
	AssignedJobs    int
	RunningJobs     int
	JobsStarted     int64
	JobsCompleted   int64
	MetricsReported int
}

func aggregateStatus(scaleSets []health.ScaleSetStatus) statusTotals {
	var totals statusTotals
	for _, ss := range scaleSets {
		if ss.Ready {
			totals.Ready++
		}
		totals.Idle += ss.Idle
		totals.Busy += ss.Busy
		if ss.Metrics == nil {
			continue
		}
		totals.MetricsReported++
		totals.Desired += ss.Metrics.DesiredRunners
		totals.AvailableJobs += ss.Metrics.AvailableJobs
		totals.AssignedJobs += ss.Metrics.AssignedJobs
		totals.RunningJobs += ss.Metrics.RunningJobs
		totals.JobsStarted += ss.Metrics.JobsStarted
		totals.JobsCompleted += ss.Metrics.JobsCompleted
	}
	return totals
}

type statusStyles struct {
	enabled bool
	title   lipgloss.Style
	good    lipgloss.Style
	warn    lipgloss.Style
	bad     lipgloss.Style
	muted   lipgloss.Style
}

func newStatusStyles(enabled bool) statusStyles {
	return statusStyles{
		enabled: enabled,
		title:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
		good:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")),
		warn:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3")),
		bad:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")),
		muted:   lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
	}
}

func (s statusStyles) render(style lipgloss.Style, value string) string {
	if !s.enabled {
		return value
	}
	return style.Render(value)
}

func (s statusStyles) section(value string) string { return s.render(s.title, value) }
func (s statusStyles) label(value string) string   { return s.render(s.muted, value) }
func (s statusStyles) field(value string) string {
	return s.label(fmt.Sprintf("%-12s", value))
}

func (s statusStyles) overall(h health.HealthResponse) string {
	if len(h.ScaleSets) == 0 {
		return s.render(s.warn, "○ WAITING")
	}
	if strings.EqualFold(h.Status, "ok") {
		return s.render(s.good, "● HEALTHY")
	}
	return s.render(s.bad, "● "+strings.ToUpper(h.Status))
}

func (s statusStyles) scaleSetState(ss health.ScaleSetStatus) string {
	if ss.Ready {
		return s.render(s.good, "● READY")
	}
	if ss.LastError != "" {
		return s.render(s.bad, "● ERROR")
	}
	return s.render(s.warn, "○ CONNECTING")
}

// formatStatus turns the cheap /healthz snapshot into an operator-oriented
// dashboard. It deliberately consumes only HealthResponse so the renderer is
// deterministic, independently testable, and keeps --json as the stable raw
// data surface for scripts.
func formatStatus(h health.HealthResponse, endpoint string, now time.Time, color bool) string {
	styles := newStatusStyles(color)
	totals := aggregateStatus(h.ScaleSets)
	version := h.Version
	if version == "" {
		version = "unknown"
	}
	uptime := h.Uptime
	if uptime == "" {
		uptime = "unknown"
	}

	var b strings.Builder
	fmt.Fprintln(&b, styles.section("RUNNER STATUS"))
	fmt.Fprintf(&b, "  %s  runner %s · uptime %s\n", styles.overall(h), version, uptime)
	fmt.Fprintf(&b, "  %s%s\n", styles.field("Endpoint"), endpoint)
	fmt.Fprintf(&b, "  %s%d ready / %d total\n", styles.field("Scale sets"), totals.Ready, len(h.ScaleSets))

	runnerSummary := fmt.Sprintf("%d idle · %d busy · %d total", totals.Idle, totals.Busy, totals.Idle+totals.Busy)
	if totals.MetricsReported > 0 {
		runnerSummary += fmt.Sprintf(" · %d desired%s", totals.Desired, metricsCoverage(totals.MetricsReported, len(h.ScaleSets)))
	}
	fmt.Fprintf(&b, "  %s%s\n", styles.field("Runners"), runnerSummary)
	if totals.MetricsReported > 0 {
		coverage := metricsCoverage(totals.MetricsReported, len(h.ScaleSets))
		fmt.Fprintf(&b, "  %s%d available · %d assigned · %d running%s\n",
			styles.field("Queue"), totals.AvailableJobs, totals.AssignedJobs, totals.RunningJobs, coverage)
		fmt.Fprintf(&b, "  %s%d started · %d completed%s\n",
			styles.field("Jobs"), totals.JobsStarted, totals.JobsCompleted, coverage)
	}

	fmt.Fprintln(&b)
	fmt.Fprintln(&b, styles.section("SCALE SETS"))
	if len(h.ScaleSets) == 0 {
		fmt.Fprintln(&b, "  No scale sets registered yet. The runner may still be starting.")
	} else {
		for i, ss := range h.ScaleSets {
			if i > 0 {
				fmt.Fprintln(&b)
			}
			fmt.Fprintf(&b, "  %s  %s\n", styles.scaleSetState(ss), ss.Name)
			fmt.Fprintf(&b, "    %s%d idle · %d busy · %d total", styles.field("Runners"), ss.Idle, ss.Busy, ss.Idle+ss.Busy)
			if ss.Metrics != nil {
				fmt.Fprintf(&b, " · %d desired", ss.Metrics.DesiredRunners)
			}
			fmt.Fprintln(&b)

			if ss.Metrics == nil {
				fmt.Fprintf(&b, "    %snot reported yet\n", styles.field("Metrics"))
			} else {
				fmt.Fprintf(&b, "    %s%d available · %d assigned · %d running\n",
					styles.field("Queue"), ss.Metrics.AvailableJobs, ss.Metrics.AssignedJobs, ss.Metrics.RunningJobs)
				fmt.Fprintf(&b, "    %s%d started · %d completed\n",
					styles.field("Jobs"), ss.Metrics.JobsStarted, ss.Metrics.JobsCompleted)
			}
			fmt.Fprintf(&b, "    %s%s\n", styles.field("Connection"), formatLastConnected(ss.LastConnected, now))
			if ss.LastError != "" {
				fmt.Fprintf(&b, "    %s%s\n", styles.field("Error"), indentContinuation(ss.LastError, 16))
			}
		}
	}

	if len(h.Disk) > 0 {
		disks := append([]health.DiskStatus(nil), h.Disk...)
		sort.Slice(disks, func(i, j int) bool { return disks[i].Filesystem < disks[j].Filesystem })
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, styles.section("DISK"))
		for _, disk := range disks {
			fmt.Fprintf(&b, "  %s\n", disk.Filesystem)
			fmt.Fprintf(&b, "    %s  %.1f%% free · %s available / %s total\n",
				diskBar(disk.FreePercent, 16), disk.FreePercent,
				provider.FormatBytes(disk.FreeBytes), provider.FormatBytes(disk.TotalBytes))
		}
	}

	return b.String()
}

func metricsCoverage(reported, total int) string {
	if reported == total {
		return ""
	}
	return fmt.Sprintf(" · %d/%d scale sets reporting", reported, total)
}

func formatLastConnected(value string, now time.Time) string {
	if value == "" {
		return "not connected yet"
	}
	connected, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	age := now.Sub(connected)
	var relative string
	switch {
	case age < time.Minute:
		relative = "just now"
	case age < time.Hour:
		relative = fmt.Sprintf("%dm ago", int(age/time.Minute))
	case age < 24*time.Hour:
		relative = fmt.Sprintf("%dh ago", int(age/time.Hour))
	default:
		relative = fmt.Sprintf("%dd ago", int(age/(24*time.Hour)))
	}
	return relative + " · " + value
}

func diskBar(freePercent float64, width int) string {
	if freePercent < 0 {
		freePercent = 0
	}
	if freePercent > 100 {
		freePercent = 100
	}
	filled := int(freePercent/100*float64(width) + 0.5)
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

func indentContinuation(value string, spaces int) string {
	indent := "\n" + strings.Repeat(" ", spaces)
	return strings.ReplaceAll(strings.TrimSpace(value), "\n", indent)
}

func statusColorEnabled(w io.Writer, disabled bool) bool {
	if disabled || os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
