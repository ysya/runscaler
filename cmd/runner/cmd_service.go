package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	"github.com/ysya/runscaler/internal/shell"
)

// Service file paths and identifiers.
const (
	serviceName        = "runner"
	serviceDescription = "GitHub Actions Runner Manager"

	defaultConfigPath = layout.SystemConfigFile

	// rootBinaryPath is where printed fixes install a copy of runner that only
	// root can modify.
	rootBinaryPath = "/usr/local/bin/runner"

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
	// uninstall removes the definition discovery found at definition ("" if
	// unknown), but only when it is the file runner itself writes.
	uninstall(scope serviceScope, definition string) error
	start(scope serviceScope) error
	stop(scope serviceScope) error
	restart(scope serviceScope) error
	status(scope serviceScope) error
	logs(scope serviceScope, follow bool, lines int) error
}

type installOpts struct {
	runtimeUser         string
	runtimeGroup        string
	supplementaryGroups []string
	environmentFiles    []string
	// scope is scopeUser for the current user's service manager; anything
	// else, including the zero value, is a system installation.
	scope         serviceScope
	configPath    string
	binaryPath    string
	noStart       bool
	force         bool
	provider      string
	scaleSetNames []string
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
macOS supports only a LaunchAgent for the logged-in user (no root required).
Linux defaults to a system installation with a dedicated non-root runtime;
use --scope user for a user installation. Management commands select the
unique installed service automatically; pass --scope user|system to
disambiguate. --user and --user=false remain aliases for the two scopes.`,
	Example: `  sudo runner service install                   # Linux: install a non-root system service
  runner service install --scope user           # Install as user service
  runner service status                         # Show service status
  runner service logs -f                        # Follow service logs
  sudo runner service uninstall --scope system  # Remove the system service`,
}

var serviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and start runner as a system service",
	Long: `Install runner as a service and start it.

On Linux, the default is a system service that runs as the dedicated
non-root "runscaler" account. It needs sudo, and before the unit is written
'runner validate' runs as that account (--no-start skips this check).
--scope user installs a systemd user service for the current account
instead, without sudo. On macOS, runner installs a LaunchAgent for the
logged-in user.

A host has one runner installation: install refuses a second one, and a
fresh install refuses while another runner is running.`,
	Example: `  sudo runner service install             # Linux: system service (the default)
  runner service install --scope user     # User service for this account, no sudo
  runner service install --no-start       # Install and enable without starting`,
	RunE: runServiceInstall,
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

	// install flags; --scope and --user are added for every installation-aware
	// command in installation.go.
	f := serviceInstallCmd.Flags()
	f.String("config-path", "", "Config file path for the service (default: auto-detect)")
	f.String("binary-path", "", "Path to runner binary (default: auto-detect)")
	f.Bool("no-start", false, "Install and enable without starting")
	f.Bool("force", false, "Regenerate an existing service definition after validating the new one")
	f.String("environment-file", "", "Protected systemd environment file for service credential references")

	// logs flags
	serviceLogsCmd.Flags().BoolP("follow", "f", false, "Follow log output")
	serviceLogsCmd.Flags().IntP("lines", "n", 100, "Number of lines to show")
}

// ── Handler functions ───────────────────────────────────────────────────

var errServiceNotInstalled = errors.New("service not installed")

// serviceConfigFacts are the parts of a config a service definition encodes.
type serviceConfigFacts struct {
	found         bool
	provider      string
	scaleSetNames []string
	drainTimeout  *time.Duration
	logFile       *string
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
			if ss.ScaleSetName != "" {
				facts.scaleSetNames = append(facts.scaleSetNames, ss.ScaleSetName)
			}
			if !ss.IsTart() {
				facts.provider = config.DefaultProvider
			}
		}
	}
	return facts, nil
}

// buildInstallOpts gathers everything a service definition depends on from
// one read of the config. A missing config is only a warning for a fresh
// install; --force requires one.
func buildInstallOpts(scope serviceScope, configPath, binaryPath string, requireConfig bool, getenv func(string) string) (installOpts, []string, error) {
	facts, err := readServiceConfig(configPath, requireConfig)
	if err != nil {
		return installOpts{}, nil, err
	}
	var warnings []string
	if !facts.found {
		warnings = append(warnings, fmt.Sprintf("Config file not found at %s; build one at %s first", configPath, configBuilderURL))
	}
	if scope != scopeUser && runtime.GOOS == "linux" {
		if _, warning := systemdLogsDirectory(facts.logFile); warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return installOpts{
		scope:         scope,
		configPath:    configPath,
		binaryPath:    binaryPath,
		provider:      facts.provider,
		scaleSetNames: facts.scaleSetNames,
		drainTimeout:  facts.drainTimeout,
		logFile:       facts.logFile,
		xdgConfigHome: getenv("XDG_CONFIG_HOME"),
		xdgStateHome:  getenv("XDG_STATE_HOME"),
	}, warnings, nil
}

func runServiceInstall(cmd *cobra.Command, _ []string) error {
	scope, target, err := serviceCommandInstallation(cmd, true)
	if err != nil {
		return err
	}
	noStart, _ := cmd.Flags().GetBool("no-start")
	force, _ := cmd.Flags().GetBool("force")
	if target != nil && !force {
		return errors.New("service already installed; use --force to regenerate its definition")
	}

	if err := refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "install the runner service", pathExists); err != nil {
		return err
	}
	if err := refuseDarwinSystemScope(runtime.GOOS, scope, "install the runner service"); err != nil {
		return err
	}
	if scope == scopeUser && os.Geteuid() == 0 {
		return errors.New("refusing to install a root user's service; run as the actual user without sudo, or select --scope system on Linux")
	}
	if err := checkCommandPrivileges(cmd, scope); err != nil {
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
	if target != nil && !cmd.Flags().Changed("binary-path") {
		binaryPath = target.BinaryPath
	}
	configPath, err := resolveConfigPath(cmd, scope)
	if err != nil {
		return err
	}
	if target != nil {
		if cmd.Flags().Changed("config-path") || cmd.Flags().Changed("config") {
			if err := explicitConfigConflict(configPath, target); err != nil {
				return err
			}
		} else {
			configPath = target.ConfigPath
		}
	}
	opts, warnings, err := buildInstallOpts(scope, configPath, binaryPath, force || (scope != scopeUser && runtime.GOOS == "linux" && target == nil), os.Getenv)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "  ⚠ %s\n", w)
	}
	opts.noStart = noStart
	opts.force = force
	if runtime.GOOS == "linux" && scope != scopeUser {
		if target == nil {
			opts.runtimeUser = serviceAccountName
			opts.runtimeGroup = serviceAccountName
		} else {
			applyInstalledIdentity(&opts, target)
			if target.RuntimeUser == "root" || target.RuntimeUser == "0" {
				fmt.Fprintln(cmd.ErrOrStderr(), "Existing system service retains its root runtime.")
			}
		}
	}
	if target != nil && scope == scopeUser {
		opts.xdgConfigHome = target.Environment["XDG_CONFIG_HOME"]
		opts.xdgStateHome = target.Environment["XDG_STATE_HOME"]
	}
	if envFile, _ := cmd.Flags().GetString("environment-file"); envFile != "" {
		if scope == scopeUser || runtime.GOOS != "linux" {
			return errors.New("--environment-file is supported only for Linux system services")
		}
		path, err := filepath.Abs(envFile)
		if err != nil {
			return err
		}
		opts.environmentFiles = []string{path}
	}
	if target != nil && scope != scopeUser && !cmd.Flags().Changed("environment-file") {
		for key := range target.Environment {
			if !slices.Contains(serviceManagedEnvKeys, key) {
				return errors.New("existing unit has custom inline environment; move it to a protected --environment-file before regeneration")
			}
		}
	}
	if err := installService(mgr, opts, validateServiceBinaryFor); err != nil {
		var ub *untrustedBinaryError
		if errors.As(err, &ub) {
			return fmt.Errorf("%w%s", err, untrustedBinaryHint(ub.Path, installRetryCommand(opts)))
		}
		return err
	}
	printWorkflowLabels(cmd.OutOrStdout(), opts.scaleSetNames)
	return nil
}

func printWorkflowLabels(out io.Writer, names []string) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintln(out, "\n  Workflow runs-on values:")
	for _, name := range names {
		fmt.Fprintf(out, "    runs-on: %s\n", strconv.Quote(name))
	}
}

// installRetryCommand repeats a system service install with the root-only
// copy of the binary, keeping the config and flags of the failed attempt.
// Only a system install can fail the binary trust check: a user service
// installed without root is never checked, and installing one as root is
// refused before the check runs.
func installRetryCommand(opts installOpts) string {
	return serviceInstallCommand(rootBinaryPath, scopeSystem, opts.configPath, opts.force, opts.noStart, opts.environmentFiles)
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
func installService(mgr serviceManager, opts installOpts, validateBinary func(scope serviceScope, path string) (string, error)) error {
	binaryPath, err := validateBinary(opts.scope, opts.binaryPath)
	if err != nil {
		return err
	}
	opts.binaryPath = binaryPath
	if opts.scope != scopeUser {
		for _, path := range opts.environmentFiles {
			if err := validateServiceFile(path); err != nil {
				return err
			}
			if err := checkRootOnlyChain(path, lstatFile); err != nil {
				return fmt.Errorf("system environment file must be root-protected: %w", err)
			}
		}
	}
	if _, err := renderServiceDefinition(opts); err != nil {
		return err
	}
	if opts.runtimeUser != "" {
		if err := prepareSystemInstallation(&opts); err != nil {
			return err
		}
		if _, err := renderServiceDefinition(opts); err != nil {
			return err
		}
	}
	return mgr.install(opts)
}

// untrustedBinaryError reports a service binary that someone other than
// root could replace.
type untrustedBinaryError struct {
	Path string // resolved binary
	Err  error  // the failing path and reason from checkRootOnlyChain
}

func (e *untrustedBinaryError) Error() string {
	return "a system service must run a binary only root can modify: " + e.Err.Error()
}
func (e *untrustedBinaryError) Unwrap() error { return e.Err }

// untrustedBinaryHint shows how to give a root service a binary only root
// can modify and repeat the command with it. When the binary already is
// rootBinaryPath, a copy cannot replace it, so the reported path is what
// needs fixing.
func untrustedBinaryHint(binary, retry string) string {
	if binary == rootBinaryPath {
		return "\n\n  Make the path reported above changeable only by root, then retry:\n    " + retry
	}
	return "\n\n  Install a copy only root can modify, then retry:\n" +
		"    sudo install -m 0755 " + shell.Word(binary) + " " + rootBinaryPath + "\n" +
		"    " + retry
}

// validateServiceBinary resolves the binary a service will run. On Linux, a
// system service must run a binary only root can modify: systemd starts it
// from a root-managed unit, so a user-writable file would let that user run
// code under the service's identity (root, for existing root services). A
// user service is exempt unless root is installing it, which is refused
// earlier anyway.
func validateServiceBinary(goos string, scope serviceScope, euid int, path string, evalSymlinks func(string) (string, error), stat statFunc) (string, error) {
	resolved, err := evalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve binary %s: %w", path, err)
	}
	if goos != "linux" || (scope == scopeUser && euid != 0) {
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
		return "", &untrustedBinaryError{Path: resolved, Err: err}
	}
	return resolved, nil
}

func validateServiceBinaryFor(scope serviceScope, path string) (string, error) {
	return validateServiceBinary(runtime.GOOS, scope, os.Geteuid(), path, filepath.EvalSymlinks, lstatFile)
}

func runServiceUninstall(cmd *cobra.Command, _ []string) error {
	scope, target, err := serviceCommandInstallation(cmd, false)
	if err != nil {
		return err
	}
	if err := checkCommandPrivileges(cmd, scope); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	definition := ""
	if target != nil {
		definition = target.DefinitionPath
	}
	return mgr.uninstall(scope, definition)
}

func runServiceStart(cmd *cobra.Command, _ []string) error {
	scope, _, err := serviceCommandInstallation(cmd, false)
	if err != nil {
		return err
	}
	if err := refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "start the runner service", pathExists); err != nil {
		return err
	}
	if err := refuseDarwinSystemScope(runtime.GOOS, scope, "start the runner service"); err != nil {
		return err
	}
	if err := checkCommandPrivileges(cmd, scope); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.start(scope)
}

func runServiceStop(cmd *cobra.Command, _ []string) error {
	scope, _, err := serviceCommandInstallation(cmd, false)
	if err != nil {
		return err
	}
	if err := checkCommandPrivileges(cmd, scope); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.stop(scope)
}

func runServiceRestart(cmd *cobra.Command, _ []string) error {
	scope, _, err := serviceCommandInstallation(cmd, false)
	if err != nil {
		return err
	}
	if err := refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "restart the runner service", pathExists); err != nil {
		return err
	}
	if err := refuseDarwinSystemScope(runtime.GOOS, scope, "restart the runner service"); err != nil {
		return err
	}
	if err := checkCommandPrivileges(cmd, scope); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.restart(scope)
}

func runServiceStatus(cmd *cobra.Command, _ []string) error {
	scope, _, err := serviceCommandInstallation(cmd, false)
	if err != nil {
		return err
	}
	if err := statusPrivilegeError(runtime.GOOS, scope, os.Geteuid(), hintBinary(cmd)); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.status(scope)
}

// statusPrivilegeError explains that launchctl lists system services only
// to root.
func statusPrivilegeError(goos string, scope serviceScope, euid int, binary string) error {
	if goos == "darwin" && scope == scopeSystem && euid != 0 {
		return errors.New("launchctl lists system services only to root; run: " + serviceCommand(binary, scopeSystem, "status"))
	}
	return nil
}

// launchdServiceLogFile returns the service's stderr log when it exists.
func launchdServiceLogFile(scope serviceScope, exists func(string) bool) string {
	if current := launchdStderrPath(scope); exists(current) {
		return current
	}
	return ""
}

func runServiceLogs(cmd *cobra.Command, _ []string) error {
	scope, _, err := serviceCommandInstallation(cmd, false)
	if err != nil {
		return err
	}
	follow, _ := cmd.Flags().GetBool("follow")
	lines, _ := cmd.Flags().GetInt("lines")
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.logs(scope, follow, lines)
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
{{- if .RuntimeUser}}
User={{.RuntimeUser}}
Group={{.RuntimeGroup}}
WorkingDirectory=/var/lib/runner
{{- if .SupplementaryGroups}}
SupplementaryGroups={{.SupplementaryGroups}}
{{- end}}
{{- end}}
ExecStart={{.ExecStart}}
Restart=on-failure
RestartSec=10s
# Must exceed runner's drain budget so systemd does not SIGKILL an in-flight job.
TimeoutStopSec={{.StopTimeoutSeconds}}
{{- range .Environment}}
Environment={{.}}
{{- end}}
{{- range .EnvironmentFiles}}
EnvironmentFile={{.}}
{{- end}}
{{- if not .User}}
NoNewPrivileges=true
ProtectSystem=strict
{{- if not .RuntimeUser}}
LogsDirectory={{.LogsDirectory}}
{{- end}}
ReadWritePaths={{.ReadWritePaths}}
{{- end}}

[Install]
WantedBy={{- if .User}}default.target{{- else}}multi-user.target{{- end}}
`))

type systemdData struct {
	RuntimeUser         string
	RuntimeGroup        string
	SupplementaryGroups string
	EnvironmentFiles    []string
	Description         string
	ExecStart           string
	AfterDocker         bool
	User                bool
	LogsDirectory       string
	ReadWritePaths      string
	StopTimeoutSeconds  int
	Environment         []string
}

type systemdManager struct{}

// renderSystemdUnit renders the complete unit used by install. Keeping
// rendering separate from filesystem writes makes the sandbox directly testable.
func renderSystemdUnit(opts installOpts) (string, error) {
	if opts.runtimeUser != "" {
		if opts.scope == scopeUser || !validServicePrincipal(opts.runtimeUser) || !validServicePrincipal(opts.runtimeGroup) || opts.runtimeUser == "root" || opts.runtimeUser == "0" || opts.runtimeGroup == "root" || opts.runtimeGroup == "0" {
			return "", errors.New("invalid non-root system service identity")
		}
		for _, group := range opts.supplementaryGroups {
			if !validServicePrincipal(group) || group == "root" || group == "0" {
				return "", errors.New("invalid service supplementary group")
			}
		}
	}
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
		RuntimeUser:         opts.runtimeUser,
		RuntimeGroup:        opts.runtimeGroup,
		SupplementaryGroups: strings.Join(opts.supplementaryGroups, " "),
		Description:         serviceDescription,
		ExecStart:           binary + " run --config " + configPath,
		AfterDocker:         opts.provider == "docker",
		User:                opts.scope == scopeUser,
		LogsDirectory:       logsDir,
		// ProtectSystem=strict leaves /tmp read-only, where runner run creates
		// its machine-wide lock. PrivateTmp would not help: a private /tmp
		// hides the lock from runners started outside the service. Unix
		// sockets such as Docker's stay connectable on read-only paths.
		ReadWritePaths:     filepath.Dir(runnerlock.DefaultPath),
		StopTimeoutSeconds: int(serviceStopTimeout(opts.drainTimeout).Seconds()),
	}
	if opts.runtimeUser != "" {
		data.ReadWritePaths += " /var/lib/runner " + filepath.Join(layout.SystemLogRoot, logsDir)
	}
	for _, path := range opts.environmentFiles {
		if !filepath.IsAbs(path) {
			return "", errors.New("service environment file must be absolute")
		}
		literal, err := systemdEnvironmentFile(path)
		if err != nil {
			return "", err
		}
		data.EnvironmentFiles = append(data.EnvironmentFiles, literal)
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
	scope := scopeSystem
	if opts.scope == scopeUser {
		scope = scopeUser
	}
	env = append(env, envVar{Key: serviceScopeEnv, Value: string(scope)})
	if opts.runtimeUser != "" {
		env = append(env, envVar{Key: "HOME", Value: "/var/lib/runner"})
	}
	if scope == scopeUser {
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

// EnvironmentFile takes one literal absolute path, not a quoted word list.
// systemd's config_parse_unit_env_file expands specifiers but does not unquote
// or C-unescape the value. Refuse glob/continuation/trim ambiguity instead.
func systemdEnvironmentFile(path string) (string, error) {
	if !filepath.IsAbs(path) || rejectControlChars(path) != nil || strings.TrimSpace(path) != path || strings.ContainsAny(path, `\*?[`) {
		return "", errors.New("systemd environment file must be an absolute literal path without control, glob, backslash or surrounding whitespace characters")
	}
	return strings.ReplaceAll(path, "%", "%%"), nil
}

// rejectControlChars refuses values that would break line-based unit files.
func rejectControlChars(s string) error {
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return fmt.Errorf("%q contains a control character", s)
	}
	return nil
}

// systemdVendorUnitPaths are where a distribution package installs
// runner.service, in the order systemd loads them. Discovery manages a unit
// found there, but runner never writes or deletes these files: they belong
// to the package manager.
var systemdVendorUnitPaths = []string{"/usr/lib/systemd/system/" + systemdUnitFile, "/lib/systemd/system/" + systemdUnitFile}

// systemdDefinitionPaths lists where scope's unit may be defined, in the
// order systemd loads them: the unit runner writes, then, for a system
// service, package units.
func systemdDefinitionPaths(scope serviceScope) []string {
	if scope == scopeUser {
		return []string{systemdUnitPath(scopeUser)}
	}
	return append([]string{systemdUnitPath(scopeSystem)}, systemdVendorUnitPaths...)
}

// packageUnit returns the package-provided unit systemd would load for
// scope, or "" when there is none. Package units are system units only.
func packageUnit(scope serviceScope, exists func(string) bool) string {
	if scope == scopeUser {
		return ""
	}
	for _, path := range systemdVendorUnitPaths {
		if exists(path) {
			return path
		}
	}
	return ""
}

// ownedDefinition refuses to act on a definition runner did not write. A
// unit a package or an administrator installed is managed like any other,
// but deleting it is the job of whatever installed it. An unknown
// definition ("") is taken to be runner's own.
func ownedDefinition(definition, owned string) error {
	if definition == "" || definition == owned {
		return nil
	}
	return fmt.Errorf("the runner service is defined by %s, which runner did not write; disable it with 'sudo systemctl disable --now %s' or remove the package that provides it", definition, serviceName)
}

// systemdUnitPath is where runner writes the unit file for the given scope.
func systemdUnitPath(scope serviceScope) string {
	if scope == scopeUser {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "systemd", "user", systemdUnitFile)
	}
	return filepath.Join(systemdSystemDir, systemdUnitFile)
}

func (m *systemdManager) install(opts installOpts) error {
	unitPath := systemdUnitPath(opts.scope)
	if opts.scope == scopeUser {
		if err := os.MkdirAll(filepath.Dir(unitPath), 0755); err != nil {
			return fmt.Errorf("failed to create systemd user directory: %w", err)
		}
	}

	_, statErr := os.Stat(unitPath)
	existed := statErr == nil
	if existed && !opts.force {
		return fmt.Errorf("service already installed at %s\n\n  Regenerate it in place with --force", unitPath)
	}
	// A package unit may already be running the service. Runner's own unit
	// takes precedence over it, and only a restart applies the new one.
	packaged := packageUnit(opts.scope, pathExists)

	unit, err := renderSystemdUnit(opts)
	if err != nil {
		return fmt.Errorf("failed to render unit template: %w", err)
	}
	if err := writeFileAtomic(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("failed to write unit file: %w", err)
	}
	fmt.Printf("  ✓ Service file installed at %s\n", unitPath)
	if packaged != "" {
		fmt.Printf("    It takes precedence over the package unit at %s\n", packaged)
	}

	userFlag := systemdUserFlag(opts.scope)
	if err := runCmd("systemctl", append(userFlag, "daemon-reload")...); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed: %w", err)
	}
	if err := runCmd("systemctl", append(userFlag, "enable", serviceName)...); err != nil {
		return fmt.Errorf("systemctl enable failed: %w", err)
	}
	fmt.Printf("  ✓ Service enabled\n")

	if !opts.noStart {
		action := "start"
		if existed || packaged != "" {
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

func (m *systemdManager) uninstall(scope serviceScope, definition string) error {
	unitPath := systemdUnitPath(scope)
	if err := ownedDefinition(definition, unitPath); err != nil {
		return err
	}
	if _, err := os.Stat(unitPath); os.IsNotExist(err) {
		return fmt.Errorf("%w (no unit file at %s)", errServiceNotInstalled, unitPath)
	}
	userFlag := systemdUserFlag(scope)
	_ = runCmd("systemctl", append(userFlag, "stop", serviceName)...)
	_ = runCmd("systemctl", append(userFlag, "disable", serviceName)...)
	if err := os.Remove(unitPath); err != nil {
		return fmt.Errorf("failed to remove unit file: %w", err)
	}
	_ = runCmd("systemctl", append(userFlag, "daemon-reload")...)
	fmt.Printf("  ✓ Service stopped, disabled, and removed\n")
	if packaged := packageUnit(scope, pathExists); packaged != "" {
		fmt.Printf("  ⚠ systemd now loads the package unit at %s instead; it is disabled and stopped\n", packaged)
	}
	return nil
}

func (m *systemdManager) start(scope serviceScope) error {
	return runCmdPassthrough("systemctl", append(systemdUserFlag(scope), "start", serviceName)...)
}

func (m *systemdManager) stop(scope serviceScope) error {
	return runCmdPassthrough("systemctl", append(systemdUserFlag(scope), "stop", serviceName)...)
}

func (m *systemdManager) restart(scope serviceScope) error {
	return runCmdPassthrough("systemctl", append(systemdUserFlag(scope), "restart", serviceName)...)
}

func (m *systemdManager) status(scope serviceScope) error {
	return runCmdPassthrough("systemctl", append(systemdUserFlag(scope), "status", serviceName)...)
}

func (m *systemdManager) logs(scope serviceScope, follow bool, lines int) error {
	args := systemdUserFlag(scope)
	args = append(args, "-u", serviceName, "-n", fmt.Sprintf("%d", lines))
	if follow {
		args = append(args, "-f")
	}
	return runCmdPassthrough("journalctl", args...)
}

func systemdUserFlag(scope serviceScope) []string {
	if scope == scopeUser {
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
		StderrPath:         launchdStderrPath(opts.scope),
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
func launchdStderrPath(scope serviceScope) string {
	if scope == scopeUser {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Logs", "runner", "stderr.log")
	}
	return "/var/log/runner/stderr.log"
}

// launchdPlistPath is where runner writes, and discovery looks for, scope's
// launchd definition.
func launchdPlistPath(scope serviceScope) string {
	if scope == scopeUser {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "LaunchAgents", launchdPlistFile)
	}
	return filepath.Join(launchdSystemDir, launchdPlistFile)
}

func (m *launchdManager) install(opts installOpts) error {
	plist := launchdPlistPath(opts.scope)

	// Ensure parent directory exists (for user LaunchAgents)
	if opts.scope == scopeUser {
		if err := os.MkdirAll(filepath.Dir(plist), 0755); err != nil {
			return fmt.Errorf("failed to create LaunchAgents directory: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(launchdStderrPath(opts.scope)), 0o700); err != nil {
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

func (m *launchdManager) uninstall(scope serviceScope, definition string) error {
	plist := launchdPlistPath(scope)
	if err := ownedDefinition(definition, plist); err != nil {
		return err
	}
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return fmt.Errorf("%w (no plist at %s)", errServiceNotInstalled, plist)
	}
	_ = runCmd("launchctl", "unload", plist)
	if err := os.Remove(plist); err != nil {
		return fmt.Errorf("failed to remove plist: %w", err)
	}
	fmt.Printf("  ✓ Service unloaded and removed\n")
	return nil
}

func (m *launchdManager) start(scope serviceScope) error {
	plist := launchdPlistPath(scope)
	return runCmdPassthrough("launchctl", "load", "-w", plist)
}

func (m *launchdManager) stop(scope serviceScope) error {
	plist := launchdPlistPath(scope)
	return runCmdPassthrough("launchctl", "unload", plist)
}

func (m *launchdManager) restart(scope serviceScope) error {
	plist := launchdPlistPath(scope)
	_ = runCmd("launchctl", "unload", plist)
	return runCmdPassthrough("launchctl", "load", "-w", plist)
}

func (m *launchdManager) status(_ serviceScope) error {
	return runCmdPassthrough("launchctl", "list", launchdLabel)
}

func (m *launchdManager) logs(scope serviceScope, follow bool, lines int) error {
	logFile := launchdServiceLogFile(scope, pathExists)
	if logFile == "" {
		return fmt.Errorf("service log not found at %s", launchdStderrPath(scope))
	}
	args := []string{"-n", fmt.Sprintf("%d", lines)}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, logFile)
	return runCmdPassthrough("tail", args...)
}

// ── Helper functions ────────────────────────────────────────────────────

// checkCommandPrivileges enforces who may manage each scope: a user service
// belongs to the invoking user, never root, and a system service needs root.
// Without root it prints the exact command to rerun, forwarding the flags
// that were given, and for an install the user-scope alternative.
func checkCommandPrivileges(cmd *cobra.Command, scope serviceScope) error {
	if scope == scopeUser {
		if os.Geteuid() == 0 {
			return errors.New("user service operations must run as the actual user without sudo")
		}
		return nil
	}
	if os.Geteuid() == 0 {
		return nil
	}
	var args []string
	for _, name := range []string{"config", "config-path", "binary-path", "environment-file", "force", "no-start"} {
		f := cmd.Flags().Lookup(name)
		if f == nil || !f.Changed {
			continue
		}
		switch {
		case f.Value.Type() != "bool":
			args = append(args, "--"+name, f.Value.String())
		case f.Value.String() == "true":
			args = append(args, "--"+name)
		default:
			args = append(args, "--"+name+"=false")
		}
	}
	binary := hintBinary(cmd)
	msg := "system service management requires root; run:\n\n  " + serviceCommand(binary, scopeSystem, cmd.Name(), args...)
	if cmd.Name() == "install" {
		msg += "\n\n  Or install a user service instead:\n\n  " + serviceCommand(binary, scopeUser, "install")
	}
	return errors.New(msg)
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

// resolveConfigPath picks the config a new service definition records: an
// explicit flag, else the scope's standard location, with a local
// config.toml taking precedence for a user service as it does for the CLI.
func resolveConfigPath(cmd *cobra.Command, scope serviceScope) (string, error) {
	if p, _ := cmd.Flags().GetString("config-path"); p != "" {
		return filepath.Abs(p)
	}
	if p, _ := cmd.Flags().GetString("config"); p != "" {
		return filepath.Abs(p)
	}
	if p := cmd.Root().PersistentFlags().Lookup("config"); p != nil && p.Value.String() != "" {
		return filepath.Abs(p.Value.String())
	}
	if scope != scopeUser {
		return defaultConfigPath, nil
	}
	path, err := userConfigPath()
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

// serviceStopTimeout gives the service manager one minute beyond runner's
// own drain deadline. When drain is disabled, that minute still covers the
// normal 30-second forced cleanup and scale-set deletion.
func serviceStopTimeout(configured *time.Duration) time.Duration {
	return max(config.DrainTimeoutOrDefault(configured), 0) + time.Minute
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
