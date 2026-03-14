package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

// Service file paths and identifiers.
const (
	serviceName        = "runscaler"
	serviceDescription = "GitHub Actions Runner Auto-Scaler"

	defaultConfigPath = "/etc/runscaler/config.toml"

	// systemd
	systemdSystemDir = "/etc/systemd/system"
	systemdUnitFile  = "runscaler.service"

	// launchd
	launchdSystemDir = "/Library/LaunchDaemons"
	launchdLabel     = "com.runscaler.agent"
	launchdPlistFile = "com.runscaler.agent.plist"
)

// serviceManager abstracts platform-specific service management.
type serviceManager interface {
	install(opts installOpts) error
	uninstall(user bool) error
	start(user bool) error
	stop(user bool) error
	restart(user bool) error
	status(user bool) error
	logs(user bool, follow bool, lines int) error
}

type installOpts struct {
	user       bool
	configPath string
	binaryPath string
	noStart    bool
	backend    string
}

func newServiceManager() (serviceManager, error) {
	switch runtime.GOOS {
	case "linux":
		return &systemdManager{}, nil
	case "darwin":
		return &launchdManager{}, nil
	default:
		return nil, fmt.Errorf("unsupported platform: %s (only linux and darwin are supported)", runtime.GOOS)
	}
}

// ── Cobra commands ──────────────────────────────────────────────────────

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage runscaler as a system service",
	Long: `Install, start, stop, and manage runscaler as a system service.

On Linux, this uses systemd. On macOS, this uses launchd.
By default, services are installed at the system level (requires root).
Use --user for user-level services that don't require root.`,
	Example: `  sudo runscaler service install              # Install as system service
  runscaler service install --user             # Install as user service
  runscaler service status                     # Show service status
  runscaler service logs -f                    # Follow service logs
  sudo runscaler service uninstall             # Remove system service`,
}

var serviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and start runscaler as a system service",
	RunE:  runServiceInstall,
}

var serviceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop and remove the runscaler service",
	RunE:  runServiceUninstall,
}

var serviceStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the runscaler service",
	RunE:  runServiceStart,
}

var serviceStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the runscaler service",
	RunE:  runServiceStop,
}

var serviceRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the runscaler service",
	RunE:  runServiceRestart,
}

var serviceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show runscaler service status",
	RunE:  runServiceStatus,
}

var serviceLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show runscaler service logs",
	RunE:  runServiceLogs,
}

func init() {
	serviceCmd.AddCommand(
		serviceInstallCmd,
		serviceUninstallCmd,
		serviceStartCmd,
		serviceStopCmd,
		serviceRestartCmd,
		serviceStatusCmd,
		serviceLogsCmd,
	)

	// install flags
	f := serviceInstallCmd.Flags()
	f.Bool("user", false, "Install as user-level service (no root required)")
	f.String("config-path", "", "Config file path for the service (default: auto-detect)")
	f.String("binary-path", "", "Path to runscaler binary (default: auto-detect)")
	f.Bool("no-start", false, "Install and enable without starting")

	// uninstall / start / stop / restart share --user
	for _, c := range []*cobra.Command{serviceUninstallCmd, serviceStartCmd, serviceStopCmd, serviceRestartCmd} {
		c.Flags().Bool("user", false, "Manage user-level service")
	}

	// logs flags
	serviceLogsCmd.Flags().BoolP("follow", "f", false, "Follow log output")
	serviceLogsCmd.Flags().IntP("lines", "n", 100, "Number of lines to show")
	serviceLogsCmd.Flags().Bool("user", false, "Show user-level service logs")

	// status --user
	serviceStatusCmd.Flags().Bool("user", false, "Show user-level service status")
}

// ── Handler functions ───────────────────────────────────────────────────

func runServiceInstall(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	noStart, _ := cmd.Flags().GetBool("no-start")

	if err := checkPrivileges(user); err != nil {
		return err
	}

	mgr, err := newServiceManager()
	if err != nil {
		return err
	}

	binaryPath, err := resolveBinaryPath(cmd)
	if err != nil {
		return err
	}

	configPath := resolveConfigPath(cmd)

	// Verify binary exists
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("binary not found at %s: %w", binaryPath, err)
	}

	// Warn if config doesn't exist
	if _, err := os.Stat(configPath); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠ Config file not found at %s\n", configPath)
		fmt.Fprintf(os.Stderr, "    Run 'runscaler init' to generate one first.\n\n")
	}

	backend := detectBackend(configPath)

	return mgr.install(installOpts{
		user:       user,
		configPath: configPath,
		binaryPath: binaryPath,
		noStart:    noStart,
		backend:    backend,
	})
}

func runServiceUninstall(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	if err := checkPrivileges(user); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.uninstall(user)
}

func runServiceStart(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	if err := checkPrivileges(user); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.start(user)
}

func runServiceStop(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	if err := checkPrivileges(user); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.stop(user)
}

func runServiceRestart(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	if err := checkPrivileges(user); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.restart(user)
}

func runServiceStatus(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.status(user)
}

func runServiceLogs(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	follow, _ := cmd.Flags().GetBool("follow")
	lines, _ := cmd.Flags().GetInt("lines")
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.logs(user, follow, lines)
}

// ── systemd implementation (Linux) ──────────────────────────────────────

var systemdTmpl = template.Must(template.New("systemd").Parse(`[Unit]
Description={{.Description}}
{{- if .AfterDocker}}
After=docker.service
Requires=docker.service
{{- end}}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart={{.BinaryPath}} --config {{.ConfigPath}}
Restart=on-failure
RestartSec=10s
{{- if not .User}}
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths={{.ReadWritePaths}}
{{- end}}

[Install]
WantedBy={{- if .User}}default.target{{- else}}multi-user.target{{- end}}
`))

type systemdData struct {
	Description    string
	BinaryPath     string
	ConfigPath     string
	AfterDocker    bool
	User           bool
	ReadWritePaths string
}

type systemdManager struct{}

func (m *systemdManager) install(opts installOpts) error {
	unitDir := systemdSystemDir
	if opts.user {
		home, _ := os.UserHomeDir()
		unitDir = filepath.Join(home, ".config", "systemd", "user")
		if err := os.MkdirAll(unitDir, 0755); err != nil {
			return fmt.Errorf("failed to create systemd user directory: %w", err)
		}
	}

	unitPath := filepath.Join(unitDir, systemdUnitFile)

	// Check if already installed
	if _, err := os.Stat(unitPath); err == nil {
		return fmt.Errorf("service already installed at %s\n\n  Run 'runscaler service uninstall' first", unitPath)
	}

	rwPaths := filepath.Dir(opts.configPath)
	if opts.backend == "docker" {
		rwPaths += " /var/run/docker.sock"
	}

	data := systemdData{
		Description:    serviceDescription,
		BinaryPath:     opts.binaryPath,
		ConfigPath:     opts.configPath,
		AfterDocker:    opts.backend == "docker",
		User:           opts.user,
		ReadWritePaths: rwPaths,
	}

	f, err := os.Create(unitPath)
	if err != nil {
		return fmt.Errorf("failed to write unit file: %w", err)
	}
	defer f.Close()

	if err := systemdTmpl.Execute(f, data); err != nil {
		return fmt.Errorf("failed to render unit template: %w", err)
	}
	fmt.Printf("  ✓ Service file installed at %s\n", unitPath)

	// daemon-reload, enable, start
	userFlag := systemdUserFlag(opts.user)

	if err := runCmd("systemctl", append(userFlag, "daemon-reload")...); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed: %w", err)
	}

	if err := runCmd("systemctl", append(userFlag, "enable", serviceName)...); err != nil {
		return fmt.Errorf("systemctl enable failed: %w", err)
	}
	fmt.Printf("  ✓ Service enabled\n")

	if !opts.noStart {
		if err := runCmd("systemctl", append(userFlag, "start", serviceName)...); err != nil {
			return fmt.Errorf("systemctl start failed: %w", err)
		}
		fmt.Printf("  ✓ Service started\n")
	}

	fmt.Printf("\n  Next steps:\n")
	fmt.Printf("    runscaler service status    # Check service status\n")
	fmt.Printf("    runscaler service logs -f   # Follow logs\n")
	return nil
}

func (m *systemdManager) uninstall(user bool) error {
	unitDir := systemdSystemDir
	if user {
		home, _ := os.UserHomeDir()
		unitDir = filepath.Join(home, ".config", "systemd", "user")
	}
	unitPath := filepath.Join(unitDir, systemdUnitFile)

	if _, err := os.Stat(unitPath); os.IsNotExist(err) {
		return fmt.Errorf("service not installed (no unit file at %s)", unitPath)
	}

	userFlag := systemdUserFlag(user)

	_ = runCmd("systemctl", append(userFlag, "stop", serviceName)...)
	fmt.Printf("  ✓ Service stopped\n")

	_ = runCmd("systemctl", append(userFlag, "disable", serviceName)...)
	fmt.Printf("  ✓ Service disabled\n")

	if err := os.Remove(unitPath); err != nil {
		return fmt.Errorf("failed to remove unit file: %w", err)
	}
	fmt.Printf("  ✓ Removed %s\n", unitPath)

	_ = runCmd("systemctl", append(userFlag, "daemon-reload")...)
	return nil
}

func (m *systemdManager) start(user bool) error {
	return runCmdPassthrough("systemctl", append(systemdUserFlag(user), "start", serviceName)...)
}

func (m *systemdManager) stop(user bool) error {
	return runCmdPassthrough("systemctl", append(systemdUserFlag(user), "stop", serviceName)...)
}

func (m *systemdManager) restart(user bool) error {
	return runCmdPassthrough("systemctl", append(systemdUserFlag(user), "restart", serviceName)...)
}

func (m *systemdManager) status(user bool) error {
	return runCmdPassthrough("systemctl", append(systemdUserFlag(user), "status", serviceName)...)
}

func (m *systemdManager) logs(user bool, follow bool, lines int) error {
	args := systemdUserFlag(user)
	args = append(args, "-u", serviceName, "-n", fmt.Sprintf("%d", lines))
	if follow {
		args = append(args, "-f")
	}
	return runCmdPassthrough("journalctl", args...)
}

func systemdUserFlag(user bool) []string {
	if user {
		return []string{"--user"}
	}
	return nil
}

// ── launchd implementation (macOS) ──────────────────────────────────────

var launchdTmpl = template.Must(template.New("launchd").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.BinaryPath}}</string>
        <string>--config</string>
        <string>{{.ConfigPath}}</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>StandardOutPath</key>
    <string>{{.LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{.LogPath}}</string>
</dict>
</plist>
`))

type launchdData struct {
	Label      string
	BinaryPath string
	ConfigPath string
	LogPath    string
}

type launchdManager struct{}

func (m *launchdManager) plistPath(user bool) string {
	if user {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "LaunchAgents", launchdPlistFile)
	}
	return filepath.Join(launchdSystemDir, launchdPlistFile)
}

func (m *launchdManager) logPath(user bool) string {
	if user {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Logs", "runscaler.log")
	}
	return "/var/log/runscaler.log"
}

func (m *launchdManager) install(opts installOpts) error {
	plist := m.plistPath(opts.user)

	// Ensure parent directory exists (for user LaunchAgents)
	if opts.user {
		if err := os.MkdirAll(filepath.Dir(plist), 0755); err != nil {
			return fmt.Errorf("failed to create LaunchAgents directory: %w", err)
		}
	}

	// Check if already installed
	if _, err := os.Stat(plist); err == nil {
		return fmt.Errorf("service already installed at %s\n\n  Run 'runscaler service uninstall' first", plist)
	}

	data := launchdData{
		Label:      launchdLabel,
		BinaryPath: opts.binaryPath,
		ConfigPath: opts.configPath,
		LogPath:    m.logPath(opts.user),
	}

	f, err := os.Create(plist)
	if err != nil {
		return fmt.Errorf("failed to write plist file: %w", err)
	}
	defer f.Close()

	if err := launchdTmpl.Execute(f, data); err != nil {
		return fmt.Errorf("failed to render plist template: %w", err)
	}
	fmt.Printf("  ✓ Service file installed at %s\n", plist)

	if !opts.noStart {
		if err := runCmd("launchctl", "load", "-w", plist); err != nil {
			return fmt.Errorf("launchctl load failed: %w", err)
		}
		fmt.Printf("  ✓ Service loaded and started\n")
	}

	fmt.Printf("\n  Next steps:\n")
	fmt.Printf("    runscaler service status    # Check service status\n")
	fmt.Printf("    runscaler service logs -f   # Follow logs\n")
	return nil
}

func (m *launchdManager) uninstall(user bool) error {
	plist := m.plistPath(user)

	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return fmt.Errorf("service not installed (no plist at %s)", plist)
	}

	_ = runCmd("launchctl", "unload", plist)
	fmt.Printf("  ✓ Service unloaded\n")

	if err := os.Remove(plist); err != nil {
		return fmt.Errorf("failed to remove plist: %w", err)
	}
	fmt.Printf("  ✓ Removed %s\n", plist)
	return nil
}

func (m *launchdManager) start(user bool) error {
	plist := m.plistPath(user)
	return runCmdPassthrough("launchctl", "load", "-w", plist)
}

func (m *launchdManager) stop(user bool) error {
	plist := m.plistPath(user)
	return runCmdPassthrough("launchctl", "unload", plist)
}

func (m *launchdManager) restart(user bool) error {
	plist := m.plistPath(user)
	_ = runCmd("launchctl", "unload", plist)
	return runCmdPassthrough("launchctl", "load", "-w", plist)
}

func (m *launchdManager) status(_ bool) error {
	return runCmdPassthrough("launchctl", "list", launchdLabel)
}

func (m *launchdManager) logs(user bool, follow bool, lines int) error {
	logFile := m.logPath(user)
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		return fmt.Errorf("log file not found at %s", logFile)
	}
	args := []string{"-n", fmt.Sprintf("%d", lines)}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, logFile)
	return runCmdPassthrough("tail", args...)
}

// ── Helper functions ────────────────────────────────────────────────────

func checkPrivileges(user bool) error {
	if user {
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("system-level service management requires root privileges\n\n" +
			"  Run with sudo:  sudo runscaler service install\n" +
			"  Or use --user:  runscaler service install --user")
	}
	return nil
}

func resolveBinaryPath(cmd *cobra.Command) (string, error) {
	if p, _ := cmd.Flags().GetString("binary-path"); p != "" {
		return filepath.Abs(p)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot detect binary path: %w (use --binary-path)", err)
	}
	return filepath.EvalSymlinks(exe)
}

func resolveConfigPath(cmd *cobra.Command) string {
	if p, _ := cmd.Flags().GetString("config-path"); p != "" {
		abs, err := filepath.Abs(p)
		if err == nil {
			return abs
		}
		return p
	}
	// Try the persistent --config flag
	if p := cmd.Root().PersistentFlags().Lookup("config"); p != nil && p.Value.String() != "" {
		abs, err := filepath.Abs(p.Value.String())
		if err == nil {
			return abs
		}
		return p.Value.String()
	}
	return defaultConfigPath
}

func detectBackend(configPath string) string {
	if configPath == "" {
		return platformDefaultBackend()
	}
	v := viper.New()
	v.SetConfigFile(configPath)
	if err := v.ReadInConfig(); err != nil {
		return platformDefaultBackend()
	}
	b := v.GetString("backend")
	if b == "" {
		return config.DefaultBackend
	}
	return b
}

func platformDefaultBackend() string {
	if runtime.GOOS == "darwin" {
		return "tart"
	}
	return "docker"
}

// runCmd runs a command silently, returning any error.
func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = nil
	c.Stderr = nil
	return c.Run()
}

// runCmdPassthrough runs a command with stdout/stderr connected to the terminal.
func runCmdPassthrough(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s %s failed: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
