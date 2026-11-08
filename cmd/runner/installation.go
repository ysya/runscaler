package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/ysya/runscaler/internal/layout"
)

type serviceScope string

// systemdUnitObject is runner.service's D-Bus object path.
const systemdUnitObject = "/org/freedesktop/systemd1/unit/runner_2eservice"

const (
	scopeAuto   serviceScope = "auto"
	scopeUser   serviceScope = "user"
	scopeSystem serviceScope = "system"
)

func (s serviceScope) String() string { return string(s) }

// Installation scope is independent of the service's execution identity.
// Environment is private metadata: never print it or copy it into errors.
type installationTarget struct {
	Scope serviceScope
	// Unique is set when this is the only installation discovery finds, so
	// auto detection selects it without --scope.
	Unique bool
	// DefinitionPath is the unit file or plist the service manager loads.
	// Uninstall removes it only when it is the file runner writes.
	DefinitionPath      string
	BinaryPath          string
	ConfigPath          string
	RuntimeUser         string
	RuntimeGroup        string
	SupplementaryGroups []string
	Environment         map[string]string
	EnvironmentFiles    []string
	WorkingDirectory    string
	ReadWritePaths      []string
}

type installationCandidate struct {
	Scope  serviceScope
	Target *installationTarget
	Err    error
}

// addInstallationFlags registers both spellings of the installation scope
// on every installation-aware command. --scope is canonical; --user and
// --user=false remain aliases for --scope user and --scope system, hidden
// from help.
func addInstallationFlags(c *cobra.Command) {
	c.Flags().String("scope", string(scopeAuto), "Installation scope: auto, user, or system")
	c.Flags().Bool("user", false, "Alias for --scope user; --user=false means --scope system")
	_ = c.Flags().MarkHidden("user")
}

func requestedScope(c *cobra.Command) (serviceScope, error) {
	value := "auto"
	if f := c.Flags().Lookup("scope"); f != nil {
		value = f.Value.String()
	}
	scope := serviceScope(value)
	if scope != scopeAuto && scope != scopeUser && scope != scopeSystem {
		return "", fmt.Errorf("invalid installation scope %q; use auto, user, or system", value)
	}
	if f := c.Flags().Lookup("user"); f != nil && f.Changed {
		user, err := c.Flags().GetBool("user")
		if err != nil {
			return "", err
		}
		aliasScope := scopeSystem
		if user {
			aliasScope = scopeUser
		}
		if scope != scopeAuto && scope != aliasScope {
			return "", errors.New("--scope conflicts with --user")
		}
		scope = aliasScope
	}
	return scope, nil
}

func selectInstallation(candidates []installationCandidate, scope serviceScope) (*installationTarget, error) {
	installed, verified := 0, true
	for _, candidate := range candidates {
		if candidate.Target != nil {
			installed++
		}
		if candidate.Err != nil {
			verified = false
		}
	}
	var selected *installationTarget
	for _, candidate := range candidates {
		if scope != scopeAuto && scope != candidate.Scope {
			continue
		}
		if candidate.Err != nil {
			return nil, fmt.Errorf("cannot verify %s installation; select a known scope explicitly or check its service manager", candidate.Scope)
		}
		if candidate.Target == nil {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("both system and user installations exist:\n  system/user targets include %s (%s) and %s (%s)\nSelect --scope user or --scope system; no installation was changed", selected.BinaryPath, selected.ConfigPath, candidate.Target.BinaryPath, candidate.Target.ConfigPath)
		}
		selected = candidate.Target
	}
	if selected != nil {
		selected.Unique = installed == 1 && verified
	}
	return selected, nil
}

func targetFromArgs(scope serviceScope, binary string, args []string) (*installationTarget, error) {
	if !filepath.IsAbs(binary) || len(args) < 2 || args[0] != binary {
		return nil, errors.New("service executable must have a verified absolute path and argv")
	}
	if args[1] != "run" {
		return nil, errors.New("service does not invoke the runner manager")
	}
	var configPath string
	for i := 1; i < len(args); i++ {
		value, found := strings.CutPrefix(args[i], "--config=")
		if args[i] == "--config" {
			i++
			if i >= len(args) {
				return nil, errors.New("service --config has no value")
			}
			value, found = args[i], true
		}
		if found {
			if configPath != "" || !filepath.IsAbs(value) {
				return nil, errors.New("service config path is missing, relative, or ambiguous")
			}
			configPath = filepath.Clean(value)
		}
	}
	if configPath == "" {
		return nil, errors.New("service has no explicit config path; regenerate its definition first")
	}
	return &installationTarget{Scope: scope, BinaryPath: binary, ConfigPath: configPath, Environment: map[string]string{}}, nil
}

var discoverInstallations = readInstalledTargets

func init() {
	for _, c := range []*cobra.Command{serviceInstallCmd, serviceUninstallCmd, serviceStartCmd, serviceStopCmd, serviceRestartCmd, serviceStatusCmd, serviceLogsCmd, statusCmd, logsCmd, updateCmd, doctorCmd} {
		addInstallationFlags(c)
	}
}

type installationContextKey struct{}

func commandContext(c *cobra.Command) context.Context {
	if ctx := c.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func targetInContext(c *cobra.Command) *installationTarget {
	target, _ := commandContext(c).Value(installationContextKey{}).(*installationTarget)
	return target
}

func commandIdentity(c *cobra.Command) layout.Identity {
	id := layout.CurrentIdentity()
	if target := targetInContext(c); target != nil {
		id.ServiceScope = string(target.Scope)
		if value := target.Environment["HOME"]; filepath.IsAbs(value) {
			id.Home = value
		}
		if value := target.Environment["XDG_CONFIG_HOME"]; filepath.IsAbs(value) {
			id.XDGConfigHome = value
		}
		if value := target.Environment["XDG_STATE_HOME"]; filepath.IsAbs(value) {
			id.XDGStateHome = value
		}
	}
	return id
}

func managedConfigPath(c *cobra.Command, explicit string) (string, error) {
	// Commands without installation flags, such as validate, use exactly the
	// config they were given; the install probe relies on this to run before
	// its unit exists.
	if c.Flags().Lookup("scope") == nil {
		return explicit, nil
	}
	target, err := commandInstallation(c)
	if err != nil {
		return "", err
	}
	c.SetContext(context.WithValue(commandContext(c), installationContextKey{}, target))
	if target == nil {
		return explicit, nil
	}
	if c.Name() == "doctor" {
		if err := requireDoctorIdentity(c, target); err != nil {
			return "", err
		}
	}
	if conflict := explicitConfigConflict(explicit, target); conflict != nil {
		if fix, _ := c.Flags().GetBool("fix"); c.Name() == "doctor" && fix {
			return "", fmt.Errorf("doctor --fix: %w", conflict)
		}
		fmt.Fprintf(c.ErrOrStderr(), "Using explicit config %s instead of installed config %s\n", explicit, target.ConfigPath)
		return explicit, nil
	}
	return target.ConfigPath, nil
}

// explicitConfigConflict reports an explicit --config that names a file
// other than the selected installation's. Read-only commands may go on with
// the explicit file; commands that change the service or its binary refuse,
// so no hint or restart ever targets a config the service does not use.
func explicitConfigConflict(explicit string, target *installationTarget) error {
	if target == nil || explicit == "" || sameFilePath(absConfigPath(explicit), target.ConfigPath) {
		return nil
	}
	return fmt.Errorf("explicit config %s conflicts with the selected installation's %s", explicit, target.ConfigPath)
}

// serviceCommandInstallation resolves the scope a service command acts on:
// the requested scope, narrowed to the one installation that exists, or
// the platform default for a fresh install. The result is returned, never
// written back into the flags.
func serviceCommandInstallation(c *cobra.Command, install bool) (serviceScope, *installationTarget, error) {
	scope, err := requestedScope(c)
	if err != nil {
		return "", nil, err
	}
	candidates := discoverInstallations(commandContext(c))
	if install {
		// Installation must verify every visible scope, including when an
		// explicit override was passed. --force never bypasses conflicts.
		for _, candidate := range candidates {
			if candidate.Err != nil {
				return "", nil, fmt.Errorf("cannot verify %s installation before installing", candidate.Scope)
			}
			if candidate.Target != nil && scope != scopeAuto && candidate.Scope != scope {
				return "", nil, fmt.Errorf("an installation already exists in %s scope; refusing a second installation", candidate.Scope)
			}
		}
		if _, err := selectInstallation(candidates, scopeAuto); err != nil {
			return "", nil, err
		}
	}
	target, err := selectInstallation(candidates, scope)
	if err != nil {
		return "", nil, err
	}
	if install && target == nil {
		if err := refuseWhileAnotherRunnerRuns(); err != nil {
			return "", nil, err
		}
	}
	switch {
	case target != nil:
		scope = target.Scope
		c.SetContext(context.WithValue(commandContext(c), installationContextKey{}, target))
		announceInstallation(c, target)
	case !install:
		return "", nil, notInstalledError(c, scope, candidates)
	case scope == scopeAuto:
		scope = defaultScope()
	}
	return scope, target, nil
}

// refuseWhileAnotherRunnerRuns stops a fresh installation while another
// runner holds the machine-wide lock. Discovery sees system definitions but
// only the invoking account's user definition, so another account's running
// user service, or a foreground `runner run`, shows up only here.
func refuseWhileAnotherRunnerRuns() error {
	running, err := probeRunningRunner()
	if err != nil {
		return fmt.Errorf("cannot check for a running runner before installing: %w", err)
	}
	if running != nil {
		return fmt.Errorf("%w\n\n  Only one runner may run per machine, so no service was installed.\n  If another account's service runs it, uninstall that service as that account;\n  otherwise stop that runner first", running)
	}
	return nil
}

// notInstalledError explains a missing installation: the installation in
// another scope that the requested scope excluded, or how to install one.
func notInstalledError(c *cobra.Command, scope serviceScope, candidates []installationCandidate) error {
	for _, candidate := range candidates {
		if candidate.Target != nil && candidate.Scope != scope {
			return fmt.Errorf("%w in %s scope; the installed service is in %s scope: select it with --scope %s", errServiceNotInstalled, scope, candidate.Scope, candidate.Scope)
		}
	}
	binary := hintBinary(c)
	hint := "\n\n  Install it with:\n    " + serviceCommand(binary, defaultScope(), "install")
	if defaultScope() == scopeSystem {
		hint += "\n  or, for this account only:\n    " + serviceCommand(binary, scopeUser, "install")
	}
	return fmt.Errorf("%w%s", errServiceNotInstalled, hint)
}

func announceInstallation(c *cobra.Command, target *installationTarget) {
	identity := target.RuntimeUser
	if identity == "" {
		identity = "current user"
	}
	fmt.Fprintf(c.ErrOrStderr(), "Installation: %s · runtime %s · binary %s · config %s\n", target.Scope, identity, target.BinaryPath, target.ConfigPath)
}

func commandInstallation(c *cobra.Command) (*installationTarget, error) {
	scope, err := requestedScope(c)
	if err != nil {
		return nil, err
	}
	target, err := selectInstallation(discoverInstallations(commandContext(c)), scope)
	if err == nil && target == nil && scope != scopeAuto {
		return nil, errServiceNotInstalled
	}
	return target, err
}

func readInstalledTargets(ctx context.Context) []installationCandidate {
	// The user-scope paths live under HOME; without one that scope cannot
	// be verified.
	if _, err := os.UserHomeDir(); err != nil {
		return []installationCandidate{{Scope: scopeUser, Err: err}}
	}
	var candidates []installationCandidate
	for _, scope := range []serviceScope{scopeSystem, scopeUser} {
		candidate := installationCandidate{Scope: scope}
		// The same paths the service managers write, so every definition
		// discovery selects is one they can act on.
		paths := systemdDefinitionPaths(scope)
		if runtime.GOOS == "darwin" {
			paths = []string{launchdPlistPath(scope)}
		}
		for _, path := range paths {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				candidate.Err = err
				break
			}
			if runtime.GOOS == "darwin" {
				var data []byte
				data, candidate.Err = installationOutput(ctx, "plutil", "-convert", "xml1", "-o", "-", path)
				if candidate.Err == nil {
					candidate.Target, candidate.Err = parseLaunchdTarget(scope, data)
				}
			} else {
				candidate.Target, candidate.Err = readSystemdTarget(ctx, scope)
			}
			if candidate.Target != nil {
				candidate.Target.DefinitionPath = path
			}
			break
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// Effective systemd properties include drop-ins; argv comes from D-Bus JSON,
// never from splitting the human-readable ExecStart display or shell code.
func readSystemdTarget(ctx context.Context, scope serviceScope) (*installationTarget, error) {
	bus := "--system"
	if scope == scopeUser {
		bus = "--user"
	}
	loaded, err := installationOutput(ctx, "busctl", bus, "--json=short", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "LoadUnit", "s", systemdUnitFile)
	if err != nil {
		return nil, errors.New("cannot load installed service metadata")
	}
	var object struct {
		Type string   `json:"type"`
		Data []string `json:"data"`
	}
	if json.Unmarshal(loaded, &object) != nil || object.Type != "o" || len(object.Data) != 1 || object.Data[0] != systemdUnitObject {
		return nil, errors.New("unexpected installed service object")
	}
	properties := map[string]json.RawMessage{}
	for _, name := range []string{"ExecStart", "ExecStartEx", "User", "Group", "Environment", "SupplementaryGroups", "WorkingDirectory", "EnvironmentFiles", "ReadWritePaths"} {
		args := []string{"--system", "--json=short", "get-property", "org.freedesktop.systemd1", systemdUnitObject, "org.freedesktop.systemd1.Service", name}
		if scope == scopeUser {
			args[0] = "--user"
		}
		data, err := installationOutput(ctx, "busctl", args...)
		if err != nil {
			return nil, fmt.Errorf("read service property %s: service manager unavailable", name)
		}
		if !json.Valid(data) {
			return nil, fmt.Errorf("invalid service property %s", name)
		}
		properties[name] = data
	}
	data, err := json.Marshal(properties)
	if err != nil {
		return nil, err
	}
	return parseSystemdProperties(scope, data)
}

var installationOutput = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, err
	}
	if os.Geteuid() == 0 {
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		if err := checkRootOnlyChain(path, lstatFile); err != nil {
			return nil, err
		}
	}
	return exec.CommandContext(ctx, path, args...).Output()
}

func parseSystemdProperties(scope serviceScope, data []byte) (*installationTarget, error) {
	var properties map[string]struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &properties); err != nil {
		return nil, errors.New("invalid service metadata")
	}
	var commands [][]json.RawMessage
	if err := json.Unmarshal(properties["ExecStart"].Data, &commands); err != nil || len(commands) != 1 || len(commands[0]) < 2 {
		return nil, errors.New("service must have one structured ExecStart")
	}
	var binary string
	var argv []string
	if json.Unmarshal(commands[0][0], &binary) != nil || json.Unmarshal(commands[0][1], &argv) != nil {
		return nil, errors.New("invalid service executable arguments")
	}
	noExpand := false
	if p, ok := properties["ExecStartEx"]; ok {
		var extended [][]json.RawMessage
		if json.Unmarshal(p.Data, &extended) != nil || len(extended) != 1 || len(extended[0]) < 3 {
			return nil, errors.New("invalid extended service executable metadata")
		}
		var exBinary string
		var exArgs, flags []string
		if json.Unmarshal(extended[0][0], &exBinary) != nil || json.Unmarshal(extended[0][1], &exArgs) != nil || json.Unmarshal(extended[0][2], &flags) != nil || exBinary != binary || !slices.Equal(exArgs, argv) {
			return nil, errors.New("service executable metadata changed during discovery")
		}
		for _, flag := range flags {
			switch flag {
			case "no-env-expand":
				noExpand = true
			case "ignore-failure":
			default:
				return nil, errors.New("service execution flags change its identity; inspect its definition before management")
			}
		}
	} else {
		for _, arg := range argv {
			if strings.Contains(arg, "$") {
				return nil, errors.New("cannot verify service argument expansion without ExecStartEx")
			}
		}
	}
	// ExecStart exposes pre-expansion argv. Only escaped literal dollars can
	// be resolved without reading or guessing the runtime environment.
	if !noExpand {
		for i := 1; i < len(argv); i++ {
			var literal strings.Builder
			for j := 0; j < len(argv[i]); j++ {
				if argv[i][j] == '$' {
					if j+1 == len(argv[i]) || argv[i][j+1] != '$' {
						return nil, errors.New("service arguments depend on environment expansion; use explicit literal paths")
					}
					j++
				}
				literal.WriteByte(argv[i][j])
			}
			argv[i] = literal.String()
		}
	}
	target, err := targetFromArgs(scope, binary, argv)
	if err != nil {
		return nil, err
	}
	for key, dest := range map[string]*string{"User": &target.RuntimeUser, "Group": &target.RuntimeGroup, "WorkingDirectory": &target.WorkingDirectory} {
		if p, ok := properties[key]; ok {
			if err := json.Unmarshal(p.Data, dest); err != nil {
				return nil, fmt.Errorf("invalid service property %s", key)
			}
		}
	}
	if target.RuntimeUser == "" && scope == scopeSystem {
		target.RuntimeUser = "root"
	}
	var env []string
	if p, ok := properties["Environment"]; ok {
		if json.Unmarshal(p.Data, &env) != nil {
			return nil, errors.New("invalid service environment")
		}
	}
	for _, item := range env {
		if key, value, ok := strings.Cut(item, "="); ok {
			target.Environment[key] = value
		}
	}
	if p, ok := properties["SupplementaryGroups"]; ok {
		if json.Unmarshal(p.Data, &target.SupplementaryGroups) != nil {
			return nil, errors.New("invalid supplementary groups")
		}
	}
	if p, ok := properties["ReadWritePaths"]; ok {
		if json.Unmarshal(p.Data, &target.ReadWritePaths) != nil {
			return nil, errors.New("invalid service writable paths")
		}
	}
	if p, ok := properties["EnvironmentFiles"]; ok {
		var entries [][]json.RawMessage
		if json.Unmarshal(p.Data, &entries) != nil {
			return nil, errors.New("invalid service environment files")
		}
		for _, entry := range entries {
			var path string
			var optional bool
			if len(entry) != 2 || json.Unmarshal(entry[0], &path) != nil || json.Unmarshal(entry[1], &optional) != nil || !filepath.IsAbs(path) {
				return nil, errors.New("invalid service environment file path")
			}
			if optional {
				return nil, errors.New("optional environment files require explicit service regeneration before management")
			}
			target.EnvironmentFiles = append(target.EnvironmentFiles, path)
		}
	}
	return target, nil
}
