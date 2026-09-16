package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/layout"
	runnerlock "github.com/ysya/runscaler/internal/lock"
)

// Service file paths and identifiers.
const (
	serviceName        = "runner"
	serviceDescription = "GitHub Actions Runner Manager"

	defaultConfigPath = layout.SystemConfigFile

	// systemd
	systemdSystemDir = "/etc/systemd/system"
	systemdUnitFile  = "runner.service"

	// launchd
	launchdSystemDir = "/Library/LaunchDaemons"
	launchdLabel     = "io.github.ysya.runner"
	launchdPlistFile = "io.github.ysya.runner.plist"
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
	provider   string
	// nil means the config omitted drain-timeout and therefore inherits the
	// compiled-in default; a non-nil zero explicitly disables draining.
	drainTimeout *time.Duration
	// logFile is the config's explicit log-file (nil when unset); a system
	// unit can only grant it a directory under /var/log.
	logFile *string
	// XDG base directories of the installing shell, recorded in user
	// services so the service and the CLI resolve the same paths.
	xdgConfigHome string
	xdgStateHome  string
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
	Short: "Manage runner as a system service",
	Long: `Install, start, stop, and manage runner as a system service.

On Linux, this uses systemd. On macOS, this uses launchd.
macOS defaults to user-level LaunchAgents (no root required).
Linux defaults to system-level services. Use --user for user-level services
or --user=false to explicitly manage a system-level service.`,
	Example: `  sudo runner service install --user=false # Install as system service
  runner service install --user             # Install as user service
  runner service status                     # Show service status
  runner service logs -f                    # Follow service logs
  sudo runner service uninstall --user=false # Remove system service`,
}

var serviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and start runner as a system service",
	RunE:  runServiceInstall,
}

var serviceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop and remove the runner service",
	RunE:  runServiceUninstall,
}

var serviceStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the runner service",
	RunE:  runServiceStart,
}

var serviceStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the runner service",
	RunE:  runServiceStop,
}

var serviceRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the runner service",
	RunE:  runServiceRestart,
}

var serviceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show runner service status",
	RunE:  runServiceStatus,
}

var serviceLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show runner service logs",
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
	f.Bool("user", defaultUserService(), "Install as user-level service (no root required)")
	f.String("config-path", "", "Config file path for the service (default: auto-detect)")
	f.String("binary-path", "", "Path to runner binary (default: auto-detect)")
	f.Bool("no-start", false, "Install and enable without starting")

	// uninstall / start / stop / restart share --user
	for _, c := range []*cobra.Command{serviceUninstallCmd, serviceStartCmd, serviceStopCmd, serviceRestartCmd} {
		c.Flags().Bool("user", defaultUserService(), "Manage user-level service")
	}

	// logs flags
	serviceLogsCmd.Flags().BoolP("follow", "f", false, "Follow log output")
	serviceLogsCmd.Flags().IntP("lines", "n", 100, "Number of lines to show")
	serviceLogsCmd.Flags().Bool("user", defaultUserService(), "Show user-level service logs")

	// status --user
	serviceStatusCmd.Flags().Bool("user", defaultUserService(), "Show user-level service status")
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

	configPath, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}

	// Verify binary exists
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("binary not found at %s: %w", binaryPath, err)
	}

	// Warn if config doesn't exist
	if _, err := os.Stat(configPath); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠ Config file not found at %s\n", configPath)
		fmt.Fprintf(os.Stderr, "    Run 'runner init' to generate one first.\n\n")
	}

	providerName := detectProvider(configPath)
	drainTimeout, err := detectDrainTimeout(configPath)
	if err != nil {
		return err
	}

	return mgr.install(installOpts{
		user:         user,
		configPath:   configPath,
		binaryPath:   binaryPath,
		noStart:      noStart,
		provider:     providerName,
		drainTimeout: drainTimeout,
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
{{- /* User units cannot depend on system units like docker.service —
       systemd would fail with "Unit docker.service not found". */}}
{{- if and .AfterDocker (not .User)}}
After=docker.service
Requires=docker.service
{{- end}}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart={{.ExecStart}}
Restart=on-failure
RestartSec=10s
# Must exceed runner's drain budget so systemd does not SIGKILL an in-flight job.
TimeoutStopSec={{.StopTimeoutSeconds}}
{{- range .Environment}}
Environment={{.}}
{{- end}}
{{- if not .User}}
NoNewPrivileges=true
ProtectSystem=strict
LogsDirectory={{.LogsDirectory}}
ReadWritePaths={{.ReadWritePaths}}
{{- end}}

[Install]
WantedBy={{- if .User}}default.target{{- else}}multi-user.target{{- end}}
`))

type systemdData struct {
	Description        string
	ExecStart          string
	AfterDocker        bool
	User               bool
	LogsDirectory      string
	ReadWritePaths     string
	StopTimeoutSeconds int
	Environment        []string
}

type systemdManager struct{}

// renderSystemdUnit renders the complete unit used by install. Keeping
// rendering separate from filesystem writes makes the sandbox directly testable.
func renderSystemdUnit(opts installOpts) (string, error) {
	binary, err := systemdExecArg(opts.binaryPath)
	if err != nil {
		return "", fmt.Errorf("binary path: %w", err)
	}
	configPath, err := systemdExecArg(opts.configPath)
	if err != nil {
		return "", fmt.Errorf("config path: %w", err)
	}
	logsDir, _ := systemdLogsDirectory(opts.logFile)
	data := systemdData{
		Description:   serviceDescription,
		ExecStart:     binary + " run --config " + configPath,
		AfterDocker:   opts.provider == "docker",
		User:          opts.user,
		LogsDirectory: logsDir,
		// ProtectSystem=strict leaves /tmp read-only, where runner run creates
		// its machine-wide lock. PrivateTmp would not help: a private /tmp
		// hides the lock from runners started outside the service. Unix
		// sockets such as Docker's stay connectable on read-only paths.
		ReadWritePaths:     filepath.Dir(runnerlock.DefaultPath),
		StopTimeoutSeconds: int(serviceStopTimeout(opts.drainTimeout).Seconds()),
	}
	for _, v := range serviceEnvironment(opts) {
		assignment, err := systemdValue(v.Key + "=" + v.Value)
		if err != nil {
			return "", fmt.Errorf("environment %s: %w", v.Key, err)
		}
		data.Environment = append(data.Environment, assignment)
	}
	var out strings.Builder
	if err := systemdTmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render systemd unit: %w", err)
	}
	return out.String(), nil
}

type envVar struct{ Key, Value string }

// serviceEnvironment is what every generated definition records: the template
// generation and stop timeout runner checks at startup, and for user services
// the installing shell's absolute XDG directories.
func serviceEnvironment(opts installOpts) []envVar {
	env := []envVar{
		{Key: serviceVersionEnv, Value: strconv.Itoa(currentServiceVersion)},
		{Key: serviceStopTimeoutEnv, Value: strconv.Itoa(int(serviceStopTimeout(opts.drainTimeout).Seconds()))},
	}
	if opts.user {
		if filepath.IsAbs(opts.xdgConfigHome) {
			env = append(env, envVar{Key: "XDG_CONFIG_HOME", Value: opts.xdgConfigHome})
		}
		if filepath.IsAbs(opts.xdgStateHome) {
			env = append(env, envVar{Key: "XDG_STATE_HOME", Value: opts.xdgStateHome})
		}
	}
	return env
}

// systemdLogsDirectory maps an explicit log-file onto LogsDirectory=, the only
// log location a system unit's sandbox can write. It applies the same rule as
// root's runtime log decision, so a rejected path is rejected in both places.
func systemdLogsDirectory(logFile *string) (dir, warning string) {
	const fallback = "runner"
	if logFile == nil || *logFile == "" {
		return fallback, ""
	}
	if rel, ok := layout.SystemLogsDirectory(*logFile); ok {
		return rel, ""
	}
	return fallback, fmt.Sprintf("log-file %s is not in a directory under %s named with letters, digits, '.', '_' or '-'; the service will write to %s instead", *logFile, layout.SystemLogRoot, layout.SystemLogFile)
}

// systemdExecArg quotes one ExecStart word. systemd removes the quotes and
// C-style escapes and expands %specifiers and $VARIABLES, so a literal % or $
// is doubled.
func systemdExecArg(s string) (string, error) {
	if err := rejectControlChars(s); err != nil {
		return "", err
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(s) + `"`, nil
}

// systemdValue quotes a directive value such as an Environment= assignment,
// where specifiers expand but $VARIABLES do not.
func systemdValue(s string) (string, error) {
	if err := rejectControlChars(s); err != nil {
		return "", err
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(s) + `"`, nil
}

// rejectControlChars refuses values that would break line-based unit files.
func rejectControlChars(s string) error {
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return fmt.Errorf("%q contains a control character", s)
	}
	return nil
}

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
		return fmt.Errorf("service already installed at %s\n\n  Run 'runner service uninstall' first", unitPath)
	}

	unit, err := renderSystemdUnit(opts)
	if err != nil {
		return fmt.Errorf("failed to render unit template: %w", err)
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		return fmt.Errorf("failed to write unit file: %w", err)
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
	fmt.Printf("    runner service status    # Check service status\n")
	fmt.Printf("    runner service logs -f   # Follow logs\n")
	return nil
}

// removeSystemdUnit stops, disables, and removes a systemd unit by name.
func removeSystemdUnit(user bool, unitFile, svcName string) error {
	unitDir := systemdSystemDir
	if user {
		home, _ := os.UserHomeDir()
		unitDir = filepath.Join(home, ".config", "systemd", "user")
	}
	unitPath := filepath.Join(unitDir, unitFile)
	if _, err := os.Stat(unitPath); os.IsNotExist(err) {
		return fmt.Errorf("service not installed (no unit file at %s)", unitPath)
	}
	userFlag := systemdUserFlag(user)
	_ = runCmd("systemctl", append(userFlag, "stop", svcName)...)
	_ = runCmd("systemctl", append(userFlag, "disable", svcName)...)
	if err := os.Remove(unitPath); err != nil {
		return fmt.Errorf("failed to remove unit file: %w", err)
	}
	_ = runCmd("systemctl", append(userFlag, "daemon-reload")...)
	return nil
}

func (m *systemdManager) uninstall(user bool) error {
	if err := removeSystemdUnit(user, systemdUnitFile, serviceName); err != nil {
		return err
	}
	fmt.Printf("  ✓ Service stopped, disabled, and removed\n")
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

var launchdTmpl = template.Must(template.New("launchd").Funcs(template.FuncMap{"xml": xmlEscape}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{xml .Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{xml .BinaryPath}}</string>
        <string>run</string>
        <string>--config</string>
        <string>{{xml .ConfigPath}}</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
{{- range .Environment}}
        <key>{{xml .Key}}</key>
        <string>{{xml .Value}}</string>
{{- end}}
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <!-- Must exceed runner's drain budget so launchd does not SIGKILL an in-flight job. -->
    <key>ExitTimeOut</key>
    <integer>{{.StopTimeoutSeconds}}</integer>
    <!-- runner writes its own rotated log; stdout would only duplicate it. -->
    <key>StandardOutPath</key>
    <string>/dev/null</string>
    <key>StandardErrorPath</key>
    <string>{{xml .StderrPath}}</string>
</dict>
</plist>
`))

type launchdData struct {
	Label              string
	BinaryPath         string
	ConfigPath         string
	StderrPath         string
	StopTimeoutSeconds int
	Environment        []envVar
}

type launchdManager struct{}

// renderLaunchdPlist renders the complete plist used by install.
func renderLaunchdPlist(opts installOpts) (string, error) {
	data := launchdData{
		Label:              launchdLabel,
		BinaryPath:         opts.binaryPath,
		ConfigPath:         opts.configPath,
		StderrPath:         launchdStderrPath(opts.user),
		StopTimeoutSeconds: int(serviceStopTimeout(opts.drainTimeout).Seconds()),
		Environment:        serviceEnvironment(opts),
	}
	var out strings.Builder
	if err := launchdTmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render launchd plist: %w", err)
	}
	return out.String(), nil
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// launchdStderrPath keeps what runner cannot log itself: startup failures,
// panics and child process stderr. It is not rotated.
func launchdStderrPath(user bool) string {
	if user {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Logs", "runner", "stderr.log")
	}
	return "/var/log/runner/stderr.log"
}

func (m *launchdManager) plistPath(user bool) string {
	if user {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "LaunchAgents", launchdPlistFile)
	}
	return filepath.Join(launchdSystemDir, launchdPlistFile)
}

func (m *launchdManager) install(opts installOpts) error {
	plist := m.plistPath(opts.user)

	// Ensure parent directory exists (for user LaunchAgents)
	if opts.user {
		if err := os.MkdirAll(filepath.Dir(plist), 0755); err != nil {
			return fmt.Errorf("failed to create LaunchAgents directory: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(launchdStderrPath(opts.user)), 0o700); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	// Check if already installed
	if _, err := os.Stat(plist); err == nil {
		return fmt.Errorf("service already installed at %s\n\n  Run 'runner service uninstall' first", plist)
	}

	rendered, err := renderLaunchdPlist(opts)
	if err != nil {
		return fmt.Errorf("failed to render plist template: %w", err)
	}
	if err := os.WriteFile(plist, []byte(rendered), 0644); err != nil {
		return fmt.Errorf("failed to write plist file: %w", err)
	}
	fmt.Printf("  ✓ Service file installed at %s\n", plist)

	if !opts.noStart {
		if err := runCmd("launchctl", "load", "-w", plist); err != nil {
			return fmt.Errorf("launchctl load failed: %w", err)
		}
		fmt.Printf("  ✓ Service loaded and started\n")
	}

	fmt.Printf("\n  Next steps:\n")
	fmt.Printf("    runner service status    # Check service status\n")
	fmt.Printf("    runner service logs -f   # Follow logs\n")
	return nil
}

// removeLaunchdPlist unloads and removes a launchd plist at the given path.
func removeLaunchdPlist(plistPath string) error {
	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		return fmt.Errorf("service not installed (no plist at %s)", plistPath)
	}
	_ = runCmd("launchctl", "unload", plistPath)
	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("failed to remove plist: %w", err)
	}
	return nil
}

func (m *launchdManager) uninstall(user bool) error {
	if err := removeLaunchdPlist(m.plistPath(user)); err != nil {
		return err
	}
	fmt.Printf("  ✓ Service unloaded and removed\n")
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
	logFile := launchdStderrPath(user)
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
			"  Run with sudo:  sudo runner service install --user=false\n" +
			"  Or use --user:  runner service install --user")
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

func resolveConfigPath(cmd *cobra.Command) (string, error) {
	if p, _ := cmd.Flags().GetString("config-path"); p != "" {
		return filepath.Abs(p)
	}
	if p, _ := cmd.Flags().GetString("config"); p != "" {
		return filepath.Abs(p)
	}
	if p := cmd.Root().PersistentFlags().Lookup("config"); p != nil && p.Value.String() != "" {
		return filepath.Abs(p.Value.String())
	}
	user, _ := cmd.Flags().GetBool("user")
	if !user {
		return defaultConfigPath, nil
	}
	path, err := userConfigPath("runner")
	if err != nil {
		return "", err
	}
	// Match the CLI's local-file precedence, without silently installing a user
	// service against a system-owned config (whose adjacent log is not writable).
	if _, err := os.Stat("config.toml"); err == nil || !os.IsNotExist(err) {
		return filepath.Abs("config.toml")
	}
	return path, nil
}

func detectProvider(configPath string) string {
	if configPath == "" {
		return platformDefaultProvider()
	}
	v := viper.New()
	v.SetConfigFile(configPath)
	if err := v.ReadInConfig(); err != nil {
		return platformDefaultProvider()
	}
	cfg, err := config.Load(v)
	if err != nil {
		return platformDefaultProvider()
	}
	sets := cfg.ResolveScaleSets()
	for _, ss := range sets {
		if !ss.IsTart() {
			return config.DefaultProvider
		}
	}
	if len(sets) > 0 {
		return "tart"
	}
	return platformDefaultProvider()
}

// detectDrainTimeout reads only enough configuration to preserve the
// distinction between an omitted value (nil, inherit the default) and an
// explicit zero (disable drain). Missing config keeps the existing install
// behavior: runServiceInstall has already warned and the generated service
// uses the compiled-in default.
func detectDrainTimeout(configPath string) (*time.Duration, error) {
	if configPath == "" {
		return nil, nil
	}
	v := viper.New()
	v.SetConfigFile(configPath)
	if err := v.ReadInConfig(); err != nil {
		return nil, nil
	}
	cfg, err := config.Load(v)
	if err != nil {
		return nil, fmt.Errorf("read drain-timeout from %s: %w", configPath, err)
	}
	return cfg.DrainTimeout, nil
}

// serviceStopTimeout gives the service manager one minute beyond runner's
// own drain deadline. When drain is disabled, that minute still covers the
// normal 30-second forced cleanup and scale-set deletion.
func serviceStopTimeout(configured *time.Duration) time.Duration {
	drainTimeout := config.DefaultDrainTimeout
	if configured != nil {
		drainTimeout = *configured
	}
	if drainTimeout < 0 {
		drainTimeout = 0
	}
	return drainTimeout + time.Minute
}

func platformDefaultProvider() string {
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

// serviceFilePresent reports whether a service file for the CURRENT OS exists
// at the given level — a systemd unit on Linux, a launchd plist on macOS.
// Checking only the current platform avoids false positives from stale files
// left by a different OS.
func serviceFilePresent(user bool, systemdUnit, launchdPlist string) bool {
	var p string
	switch runtime.GOOS {
	case "linux":
		if user {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, ".config", "systemd", "user", systemdUnit)
		} else {
			p = filepath.Join(systemdSystemDir, systemdUnit)
		}
	case "darwin":
		if user {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, "Library", "LaunchAgents", launchdPlist)
		} else {
			p = filepath.Join(launchdSystemDir, launchdPlist)
		}
	default:
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// newServiceInstalled reports whether the new runner service is installed at
// the given level (current OS only).
func newServiceInstalled(user bool) bool {
	return serviceFilePresent(user, systemdUnitFile, launchdPlistFile)
}
