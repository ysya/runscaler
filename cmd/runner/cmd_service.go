package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"

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
	force      bool
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
	f.Bool("force", false, "Regenerate an existing service definition after validating the new one")

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

var errServiceNotInstalled = errors.New("service not installed")

// serviceConfigFacts are the parts of a config a service definition encodes.
type serviceConfigFacts struct {
	found        bool
	provider     string
	drainTimeout *time.Duration
	logFile      *string
}

// readServiceConfig loads the facts a service definition depends on. A config
// that exists must read and load; a missing one is allowed only when not
// required.
func readServiceConfig(configPath string, required bool) (serviceConfigFacts, error) {
	facts := serviceConfigFacts{provider: platformDefaultProvider()}
	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		if required {
			return facts, fmt.Errorf("config %s does not exist", configPath)
		}
		return facts, nil
	} else if err != nil {
		return facts, fmt.Errorf("inspect config %s: %w", configPath, err)
	}
	v := viper.New()
	v.SetConfigFile(configPath)
	if err := v.ReadInConfig(); err != nil {
		return facts, fmt.Errorf("read config %s: %w", configPath, err)
	}
	cfg, err := config.Load(v)
	if err != nil {
		return facts, fmt.Errorf("load config %s: %w", configPath, err)
	}
	facts.found = true
	facts.drainTimeout = cfg.DrainTimeout
	facts.logFile = cfg.LogFile
	if sets := cfg.ResolveScaleSets(); len(sets) > 0 {
		facts.provider = "tart"
		for _, ss := range sets {
			if !ss.IsTart() {
				facts.provider = config.DefaultProvider
				break
			}
		}
	}
	return facts, nil
}

// buildInstallOpts gathers everything a service definition depends on from
// one read of the config. A missing config is only a warning for a fresh
// install; --force and migration require one.
func buildInstallOpts(user bool, configPath, binaryPath string, requireConfig bool, getenv func(string) string) (installOpts, []string, error) {
	facts, err := readServiceConfig(configPath, requireConfig)
	if err != nil {
		return installOpts{}, nil, err
	}
	var warnings []string
	if !facts.found {
		warnings = append(warnings, fmt.Sprintf("Config file not found at %s; run 'runner init' to generate one first", configPath))
	}
	if !user && runtime.GOOS == "linux" {
		if _, warning := systemdLogsDirectory(facts.logFile); warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return installOpts{
		user:          user,
		configPath:    configPath,
		binaryPath:    binaryPath,
		provider:      facts.provider,
		drainTimeout:  facts.drainTimeout,
		logFile:       facts.logFile,
		xdgConfigHome: getenv("XDG_CONFIG_HOME"),
		xdgStateHome:  getenv("XDG_STATE_HOME"),
	}, warnings, nil
}

func runServiceInstall(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	noStart, _ := cmd.Flags().GetBool("no-start")
	force, _ := cmd.Flags().GetBool("force")

	if err := refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "install the runner service", pathExists); err != nil {
		return err
	}
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
	opts, warnings, err := buildInstallOpts(user, configPath, binaryPath, force, os.Getenv)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "  ⚠ %s\n", w)
	}
	opts.noStart = noStart
	opts.force = force
	return installService(mgr, opts, validateServiceBinaryFor)
}

// renderServiceDefinition renders what install would write, so every render
// failure surfaces before an existing service is touched.
var renderServiceDefinition = func(opts installOpts) (string, error) {
	if runtime.GOOS == "darwin" {
		return renderLaunchdPlist(opts)
	}
	return renderSystemdUnit(opts)
}

// installService validates the binary and renders the definition before the
// manager writes anything, so a failed --force leaves the installed service
// as it was. Every entry point that installs a service goes through here.
func installService(mgr serviceManager, opts installOpts, validateBinary func(user bool, path string) (string, error)) error {
	binaryPath, err := validateBinary(opts.user, opts.binaryPath)
	if err != nil {
		return err
	}
	opts.binaryPath = binaryPath
	if _, err := renderServiceDefinition(opts); err != nil {
		return err
	}
	return mgr.install(opts)
}

// validateServiceBinary resolves the binary a service will run and, for a
// system service on Linux, requires that only root can change it: a root
// service executing a user-writable file hands that user root.
func validateServiceBinary(goos string, user bool, path string, evalSymlinks func(string) (string, error), stat statFunc) (string, error) {
	resolved, err := evalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve binary %s: %w", path, err)
	}
	if user || goos != "linux" {
		return resolved, nil
	}
	st, err := stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect binary %s: %w", resolved, err)
	}
	if !st.Mode.IsRegular() {
		return "", fmt.Errorf("binary %s is not a regular file", resolved)
	}
	if err := checkRootOnlyChain(resolved, stat); err != nil {
		return "", fmt.Errorf("a system service must run a binary only root can modify: %w\n\n  Install it to a root-owned directory first:\n    sudo install -m 0755 %s /usr/local/bin/runner\n    sudo /usr/local/bin/runner service install --user=false --binary-path /usr/local/bin/runner", err, shellQuotePath(resolved))
	}
	return resolved, nil
}

func validateServiceBinaryFor(user bool, path string) (string, error) {
	return validateServiceBinary(runtime.GOOS, user, path, filepath.EvalSymlinks, lstatFile)
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
	removedLegacy, err := uninstallServices(user, mgr.uninstall, legacyServiceInstalled, removeLegacyServiceFile)
	if removedLegacy {
		fmt.Printf("  ✓ Legacy runscaler service removed\n")
	}
	return err
}

// uninstallServices removes runner's service and any pre-rename definition at
// the same level; finding only the legacy one is not an error.
func uninstallServices(user bool, uninstallCurrent func(bool) error, legacyInstalled func(bool) bool, removeLegacy func(bool) error) (removedLegacy bool, err error) {
	currentErr := uninstallCurrent(user)
	if legacyInstalled(user) {
		if err := removeLegacy(user); err != nil {
			return false, errors.Join(currentErr, fmt.Errorf("remove legacy service: %w", err))
		}
		removedLegacy = true
	}
	if removedLegacy && errors.Is(currentErr, errServiceNotInstalled) {
		return true, nil
	}
	return removedLegacy, currentErr
}

func runServiceStart(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	if err := refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "start the runner service", pathExists); err != nil {
		return err
	}
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
	if err := refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "restart the runner service", pathExists); err != nil {
		return err
	}
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
	if err := statusPrivilegeError(runtime.GOOS, user, os.Geteuid()); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.status(user)
}

// statusPrivilegeError explains that launchctl's legacy list only shows
// system services to root.
func statusPrivilegeError(goos string, user bool, euid int) error {
	if goos == "darwin" && !user && euid != 0 {
		return errors.New("launchctl lists system services only to root; run: sudo runner service status --user=false")
	}
	return nil
}

// launchdServiceLogFile prefers the current stderr log and falls back to the
// combined log of plists generated by older releases.
func launchdServiceLogFile(user bool, exists func(string) bool) (path, note string) {
	if current := launchdStderrPath(user); exists(current) {
		return current, ""
	}
	if legacy := launchdLegacyLogPath(user); exists(legacy) {
		return legacy, fmt.Sprintf("  ⚠ Showing %s from a service generated by an older runner; regenerate it with 'runner service install --force' to log to %s", legacy, launchdStderrPath(user))
	}
	return "", ""
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
	binary, err := systemdExecutable(opts.binaryPath)
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

// systemdExecutable quotes and encodes the ExecStart executable (word 0).
// Unlike every later ExecStart word, systemd expands %specifiers in the
// executable path but never $VARIABLES there, so a literal $ must not be
// doubled. systemd also rejects an executable path that, after undoing the
// surrounding quoting, contains a quote, a backslash, a control character,
// invalid UTF-8, or a glob character (*?[) — quoting or backslash-escaping
// those does not help, since the check runs on the unescaped result, so such
// a path is rejected here instead of being written into a unit systemd
// cannot load.
func systemdExecutable(s string) (string, error) {
	if rejectControlChars(s) != nil || !utf8.ValidString(s) || strings.ContainsAny(s, `"'\*?[`) {
		return "", fmt.Errorf("systemd does not allow control, non-UTF-8, quote, backslash, or glob (*?[) characters in an executable path: %q", s)
	}
	return `"` + strings.ReplaceAll(s, `%`, `%%`) + `"`, nil
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

	_, statErr := os.Stat(unitPath)
	existed := statErr == nil
	if existed && !opts.force {
		return fmt.Errorf("service already installed at %s\n\n  Regenerate it in place with --force", unitPath)
	}

	unit, err := renderSystemdUnit(opts)
	if err != nil {
		return fmt.Errorf("failed to render unit template: %w", err)
	}
	if err := writeFileAtomic(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("failed to write unit file: %w", err)
	}
	fmt.Printf("  ✓ Service file installed at %s\n", unitPath)

	userFlag := systemdUserFlag(opts.user)
	if err := runCmd("systemctl", append(userFlag, "daemon-reload")...); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed: %w", err)
	}
	if err := runCmd("systemctl", append(userFlag, "enable", serviceName)...); err != nil {
		return fmt.Errorf("systemctl enable failed: %w", err)
	}
	fmt.Printf("  ✓ Service enabled\n")

	if !opts.noStart {
		action := "start"
		if existed {
			action = "restart"
		}
		if err := runCmd("systemctl", append(userFlag, action, serviceName)...); err != nil {
			return fmt.Errorf("systemctl %s failed: %w", action, err)
		}
		fmt.Printf("  ✓ Service %sed\n", action)
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
		return fmt.Errorf("%w (no unit file at %s)", errServiceNotInstalled, unitPath)
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

// launchdLegacyLogPath is the combined stdout and stderr file of plists
// generated before stdout was discarded.
func launchdLegacyLogPath(user bool) string {
	if user {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Logs", "runner.log")
	}
	return "/var/log/runner.log"
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

	_, statErr := os.Stat(plist)
	existed := statErr == nil
	if existed && !opts.force {
		return fmt.Errorf("service already installed at %s\n\n  Regenerate it in place with --force", plist)
	}

	rendered, err := renderLaunchdPlist(opts)
	if err != nil {
		return fmt.Errorf("failed to render plist template: %w", err)
	}
	if err := writeFileAtomic(plist, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("failed to write plist file: %w", err)
	}
	fmt.Printf("  ✓ Service file installed at %s\n", plist)

	if !opts.noStart {
		if existed {
			_ = runCmd("launchctl", "unload", plist)
		}
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
		return fmt.Errorf("%w (no plist at %s)", errServiceNotInstalled, plistPath)
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
	logFile, note := launchdServiceLogFile(user, pathExists)
	if logFile == "" {
		return fmt.Errorf("service log not found at %s", launchdStderrPath(user))
	}
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
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

// resolveBinaryPath returns the real file the service will execute; the
// security check must see the same file the definition runs.
func resolveBinaryPath(cmd *cobra.Command) (string, error) {
	path, _ := cmd.Flags().GetString("binary-path")
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("cannot detect binary path: %w (use --binary-path)", err)
		}
		path = exe
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve binary %s: %w", abs, err)
	}
	return resolved, nil
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
