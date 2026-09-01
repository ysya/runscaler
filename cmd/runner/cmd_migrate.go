package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	dockerclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

// newConfigPath is the destination for migrated config (var for testability).
var newConfigPath = defaultConfigPath // "/etc/runner/config.toml"

// migrateServiceForCommand is a test seam for the config/service transaction.
var migrateServiceForCommand = migrateService

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate an existing runscaler install to runner",
	Long: `Back up and canonicalize an existing runscaler config, validate it,
move it to the runner config location, and switch the service with rollback on
failure. Idempotent — safe to run multiple times. Destructive legacy Docker
volume cleanup is opt-in with --cleanup.`,
	RunE: runMigrate,
}

func init() {
	migrateCmd.Flags().Bool("user", false, "Migrate user-level service")
	migrateCmd.Flags().Bool("dry-run", false, "Validate and show the migration plan without changing anything")
	migrateCmd.Flags().Bool("cleanup", false, "Remove the legacy Docker volume after a successful migration")
	migrateCmd.Flags().String("backup-dir", "", "Directory for versioned config backups (default: <target-config-dir>/backups)")
	cmd.AddCommand(migrateCmd)
}

func runMigrate(c *cobra.Command, _ []string) error {
	user, _ := c.Flags().GetBool("user")
	dryRun, _ := c.Flags().GetBool("dry-run")
	cleanup, _ := c.Flags().GetBool("cleanup")
	if !dryRun {
		if err := checkPrivileges(user); err != nil {
			return err
		}
	}
	paths, err := resolveConfigMigrationPaths(c, user)
	if err != nil {
		return err
	}

	did := false
	configResult, err := migrateConfigFile(paths, dryRun)
	if err != nil {
		return fmt.Errorf("config migration failed: %w", err)
	}
	legacyInstalled := legacyServiceInstalled(user)
	if configResult.Found {
		if dryRun {
			fmt.Printf("  • Would back up %s under %s (%s, UTC timestamp, SHA-256)\n",
				configResult.Source, paths.BackupDir, migrationVersionLabel())
			if configResult.TargetChanged {
				fmt.Printf("  • Would write validated config %s → %s\n", configResult.Source, configResult.Target)
			}
			for _, change := range configResult.Changes {
				fmt.Printf("  • Would canonicalize %s\n", change)
			}
		} else {
			fmt.Printf("  ✓ Original config backed up at %s\n", configResult.BackupPath)
			fmt.Printf("    Manifest: %s\n", configResult.ManifestPath)
			fmt.Printf("    Restore: install -m 600 %s %s\n",
				shellQuotePath(configResult.BackupPath), shellQuotePath(configResult.Target))
			if configResult.TargetChanged {
				fmt.Printf("  ✓ Wrote validated config %s\n", configResult.Target)
			}
			for _, change := range configResult.Changes {
				fmt.Printf("  ✓ Canonicalized %s\n", change)
			}
			did = did || configResult.BackupCreated || configResult.TargetChanged
		}
	} else if legacyInstalled {
		return fmt.Errorf("legacy service is installed but no config was found at %s; use --config to specify its config", paths.Source)
	}

	if dryRun {
		if configResult.Found || legacyInstalled {
			printServiceMigrationPlan(user)
		} else {
			fmt.Println("  • No service migration needed")
		}
		if cleanup {
			fmt.Printf("  • Would remove legacy Docker volume %s after successful migration\n", legacySharedVolume)
		}
		if !configResult.Found && !legacyInstalled && !newServiceInstalled(user) {
			fmt.Println("  ✓ Nothing to migrate")
		}
		return nil
	}

	if configResult.Found || legacyInstalled {
		migratedSvc, cutover, err := migrateServiceForCommand(user, paths.Target)
		if err != nil {
			if !cutover {
				if rollbackErr := rollbackConfigMigration(configResult); rollbackErr != nil {
					err = errors.Join(err, fmt.Errorf("config rollback failed: %w", rollbackErr))
				}
			}
			return fmt.Errorf("service migration failed: %w", err)
		}
		if migratedSvc {
			fmt.Printf("  ✓ Switched service to runner\n")
			did = true
		}
	}

	// The original is removed only after the new service is running (or no
	// service existed), and only for known legacy default paths. Explicit
	// --config sources are never deleted.
	if configResult.Found && configResult.RemoveSource && !sameFilePath(configResult.Source, configResult.Target) {
		if err := os.Remove(configResult.Source); err == nil {
			fmt.Printf("  ✓ Removed legacy config %s (backup retained)\n", configResult.Source)
			did = true
		} else if !os.IsNotExist(err) {
			warnLegacy("migration succeeded but legacy config %s could not be removed: %v", configResult.Source, err)
		}
	}

	if cleanup {
		if cleaned := migrateVolume(c.Context()); cleaned {
			fmt.Printf("  ✓ Removed legacy Docker volume %s\n", legacySharedVolume)
			did = true
		}
	}

	if os.Getenv(legacyTokenEnv) != "" {
		warnLegacy("the %s env var is deprecated — rename it to RUNNER_TOKEN", legacyTokenEnv)
	}

	if !did {
		fmt.Println("  ✓ Nothing to migrate")
	}
	return nil
}

func resolveConfigMigrationPaths(c *cobra.Command, user bool) (configMigrationPaths, error) {
	paths := configMigrationPaths{Mode: "system", Target: newConfigPath, DirPerm: 0o755, RemoveSource: true}
	if user {
		home, err := os.UserHomeDir()
		if err != nil {
			return paths, fmt.Errorf("resolve user home: %w", err)
		}
		paths.Mode = "user"
		paths.DirPerm = 0o700
		paths.Target = filepath.Join(home, ".config", "runner", "config.toml")
	}
	explicitSource, _ := c.Flags().GetString("config")
	if explicitSource != "" {
		absolute, err := filepath.Abs(explicitSource)
		if err != nil {
			return paths, fmt.Errorf("resolve source config: %w", err)
		}
		paths.Source = absolute
		paths.RemoveSource = false
	} else if invocation, discoverErr := discoverLegacyServiceInvocation(user); discoverErr == nil && invocation.ConfigPath != "" {
		paths.Source = invocation.ConfigPath
		paths.RemoveSource = isDefaultLegacyConfigPath(invocation.ConfigPath, user)
	} else if user {
		home, _ := os.UserHomeDir()
		paths.Source = filepath.Join(home, ".config", "runscaler", "config.toml")
		// Some user services historically referenced the system legacy config.
		// It is safe to copy when readable, but a user migration must never try
		// to remove a root-owned source.
		if _, err := os.Stat(paths.Source); os.IsNotExist(err) {
			if _, legacyErr := os.Stat(legacyConfigPath); legacyErr == nil {
				paths.Source = legacyConfigPath
				paths.RemoveSource = false
			}
		}
	} else {
		paths.Source = legacyConfigPath
	}
	backupDir, _ := c.Flags().GetString("backup-dir")
	if backupDir != "" {
		absolute, err := filepath.Abs(backupDir)
		if err != nil {
			return paths, fmt.Errorf("resolve backup directory: %w", err)
		}
		paths.BackupDir = absolute
	} else {
		paths.BackupDir = filepath.Join(filepath.Dir(paths.Target), "backups")
	}
	return paths, nil
}

type legacyServiceInvocation struct {
	BinaryPath string
	ConfigPath string
}

func discoverLegacyServiceInvocation(user bool) (legacyServiceInvocation, error) {
	var servicePath string
	switch runtime.GOOS {
	case "linux":
		unitDir := systemdSystemDir
		if user {
			home, err := os.UserHomeDir()
			if err != nil {
				return legacyServiceInvocation{}, err
			}
			unitDir = filepath.Join(home, ".config", "systemd", "user")
		}
		servicePath = filepath.Join(unitDir, legacySystemdUnit)
	case "darwin":
		servicePath = legacyLaunchdPlistPath(user)
	default:
		return legacyServiceInvocation{}, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	data, err := os.ReadFile(servicePath)
	if err != nil {
		return legacyServiceInvocation{}, err
	}
	if runtime.GOOS == "linux" {
		return parseSystemdServiceInvocation(data)
	}
	return parseLaunchdServiceInvocation(data)
}

func parseSystemdServiceInvocation(data []byte) (legacyServiceInvocation, error) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		args := strings.Fields(strings.TrimPrefix(line, "ExecStart="))
		for i := range args {
			args[i] = strings.Trim(args[i], "\"'")
		}
		return invocationFromArgs(args)
	}
	return legacyServiceInvocation{}, errors.New("legacy systemd unit has no ExecStart")
}

func parseLaunchdServiceInvocation(data []byte) (legacyServiceInvocation, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var (
		lastKey        string
		inProgramArray bool
		args           []string
	)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return legacyServiceInvocation{}, fmt.Errorf("parse legacy launchd plist: %w", err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "key":
				if err := decoder.DecodeElement(&lastKey, &element); err != nil {
					return legacyServiceInvocation{}, err
				}
			case "array":
				inProgramArray = lastKey == "ProgramArguments"
			case "string":
				if inProgramArray {
					var value string
					if err := decoder.DecodeElement(&value, &element); err != nil {
						return legacyServiceInvocation{}, err
					}
					args = append(args, value)
				}
			}
		case xml.EndElement:
			if element.Name.Local == "array" && inProgramArray {
				inProgramArray = false
			}
		}
	}
	return invocationFromArgs(args)
}

func invocationFromArgs(args []string) (legacyServiceInvocation, error) {
	if len(args) == 0 {
		return legacyServiceInvocation{}, errors.New("legacy service has no program arguments")
	}
	invocation := legacyServiceInvocation{BinaryPath: args[0]}
	for i, arg := range args[1:] {
		switch {
		case arg == "--config" && i+2 < len(args):
			invocation.ConfigPath = args[i+2]
		case strings.HasPrefix(arg, "--config="):
			invocation.ConfigPath = strings.TrimPrefix(arg, "--config=")
		}
	}
	if invocation.ConfigPath == "" {
		return invocation, errors.New("legacy service has no --config argument")
	}
	return invocation, nil
}

func isDefaultLegacyConfigPath(path string, user bool) bool {
	if sameFilePath(path, legacyConfigPath) {
		return !user
	}
	if !user {
		return false
	}
	home, err := os.UserHomeDir()
	return err == nil && sameFilePath(path, filepath.Join(home, ".config", "runscaler", "config.toml"))
}

func printServiceMigrationPlan(user bool) {
	switch {
	case legacyServiceInstalled(user):
		fmt.Println("  • Would install runner service, stop runscaler, start runner, verify cutover, then remove runscaler")
	case newServiceInstalled(user):
		fmt.Println("  • Would verify the existing runner service is running")
	default:
		fmt.Println("  • No service migration needed")
	}
}

// migrateService performs a rollback-capable cutover. The legacy service is
// stopped but not removed until runner has started and is observable. cutover
// is true once runner owns the service role; callers must not roll back its
// config after that point even if legacy file cleanup reports an error.
func migrateService(user bool, configPath string) (acted, cutover bool, err error) {
	return migrateServiceWithDeps(user, configPath, defaultMigrationServiceDeps())
}

type migrationServiceDeps struct {
	legacyInstalled   func(bool) bool
	newInstalled      func(bool) bool
	legacyRunning     func(bool) bool
	newRunning        func(bool) bool
	waitNew           func(bool) bool
	validateNewConfig func(bool, string) error
	newManager        func() (serviceManager, error)
	executable        func() (string, error)
	evalSymlinks      func(string) (string, error)
	detectProvider    func(string) string
	detectDrain       func(string) (*time.Duration, error)
	stopLegacy        func(bool) error
	startLegacy       func(bool) error
	removeLegacyFile  func(bool) error
}

func defaultMigrationServiceDeps() migrationServiceDeps {
	return migrationServiceDeps{
		legacyInstalled:   legacyServiceInstalled,
		newInstalled:      newServiceInstalled,
		legacyRunning:     legacyServiceRunning,
		newRunning:        newServiceRunning,
		waitNew:           func(user bool) bool { return waitForNewService(user, 2*time.Second) },
		validateNewConfig: validateInstalledNewServiceConfig,
		newManager:        newServiceManager,
		executable:        os.Executable,
		evalSymlinks:      filepath.EvalSymlinks,
		detectProvider:    detectProvider,
		detectDrain:       detectDrainTimeout,
		stopLegacy:        stopLegacyService,
		startLegacy:       startLegacyService,
		removeLegacyFile: func(user bool) error {
			switch runtime.GOOS {
			case "linux":
				return removeSystemdUnit(user, legacySystemdUnit, legacyServiceName)
			case "darwin":
				return removeLaunchdPlist(legacyLaunchdPlistPath(user))
			default:
				return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
			}
		},
	}
}

func migrateServiceWithDeps(user bool, configPath string, deps migrationServiceDeps) (acted, cutover bool, err error) {
	if deps.newInstalled(user) {
		if err := deps.validateNewConfig(user, configPath); err != nil {
			return false, false, err
		}
	}
	if !deps.legacyInstalled(user) {
		if deps.newInstalled(user) {
			if deps.newRunning(user) {
				return false, true, nil
			}
			mgr, err := deps.newManager()
			if err != nil {
				return false, false, err
			}
			if err := mgr.start(user); err != nil {
				return false, false, err
			}
			if !deps.waitNew(user) {
				return false, false, errors.New("runner service start returned success but service is not active")
			}
			return true, true, nil
		}
		return false, true, nil
	}
	mgr, err := deps.newManager()
	if err != nil {
		return false, false, err
	}
	installedNew := false
	if !deps.newInstalled(user) {
		binaryPath, err := deps.executable()
		if err != nil {
			return false, false, fmt.Errorf("cannot detect binary path: %w", err)
		}
		if resolved, rerr := deps.evalSymlinks(binaryPath); rerr == nil {
			binaryPath = resolved
		}
		drainTimeout, err := deps.detectDrain(configPath)
		if err != nil {
			return false, false, err
		}
		if err := mgr.install(installOpts{
			user:         user,
			configPath:   configPath,
			binaryPath:   binaryPath,
			provider:     deps.detectProvider(configPath),
			noStart:      true,
			drainTimeout: drainTimeout,
		}); err != nil {
			// install may have written the service file before a daemon command
			// failed. Remove only the unit this attempt created.
			if deps.newInstalled(user) {
				_ = mgr.uninstall(user)
			}
			return false, false, err
		}
		installedNew = true
	}

	legacyWasRunning := deps.legacyRunning(user)
	if legacyWasRunning {
		if err := deps.stopLegacy(user); err != nil {
			if installedNew {
				_ = mgr.uninstall(user)
			}
			return installedNew, false, fmt.Errorf("stop legacy service: %w", err)
		}
	}

	if !deps.newRunning(user) {
		if err := mgr.start(user); err != nil {
			return installedNew, false, rollbackServiceCutover(user, mgr, installedNew, legacyWasRunning, deps.startLegacy, err)
		}
	}
	if !deps.waitNew(user) {
		return installedNew, false, rollbackServiceCutover(user, mgr, installedNew, legacyWasRunning, deps.startLegacy,
			errors.New("runner service start returned success but service is not active"))
	}

	// runner is active. Only now remove the stopped legacy service definition.
	if err := deps.removeLegacyFile(user); err != nil {
		return true, true, err
	}
	return true, true, nil
}

func validateInstalledNewServiceConfig(user bool, migrationTarget string) error {
	invocation, err := discoverNewServiceInvocation(user)
	if err != nil {
		return fmt.Errorf("inspect existing runner service: %w", err)
	}
	if sameFilePath(invocation.ConfigPath, migrationTarget) {
		return nil
	}
	userFlag := ""
	if user {
		userFlag = " --user"
	}
	return fmt.Errorf("existing runner service uses config %s, but migration target is %s; run `runner service uninstall%s` and rerun migration",
		invocation.ConfigPath, migrationTarget, userFlag)
}

func discoverNewServiceInvocation(user bool) (legacyServiceInvocation, error) {
	var servicePath string
	switch runtime.GOOS {
	case "linux":
		unitDir := systemdSystemDir
		if user {
			home, err := os.UserHomeDir()
			if err != nil {
				return legacyServiceInvocation{}, err
			}
			unitDir = filepath.Join(home, ".config", "systemd", "user")
		}
		servicePath = filepath.Join(unitDir, systemdUnitFile)
	case "darwin":
		if user {
			home, err := os.UserHomeDir()
			if err != nil {
				return legacyServiceInvocation{}, err
			}
			servicePath = filepath.Join(home, "Library", "LaunchAgents", launchdPlistFile)
		} else {
			servicePath = filepath.Join(launchdSystemDir, launchdPlistFile)
		}
	default:
		return legacyServiceInvocation{}, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	data, err := os.ReadFile(servicePath)
	if err != nil {
		return legacyServiceInvocation{}, err
	}
	if runtime.GOOS == "linux" {
		return parseSystemdServiceInvocation(data)
	}
	return parseLaunchdServiceInvocation(data)
}

func rollbackServiceCutover(user bool, mgr serviceManager, installedNew, legacyWasRunning bool, startLegacy func(bool) error, cause error) error {
	var rollbackErrors []error
	if installedNew {
		if err := mgr.uninstall(user); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove new service: %w", err))
		}
	} else {
		if err := mgr.stop(user); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("stop new service: %w", err))
		}
	}
	if legacyWasRunning {
		if err := startLegacy(user); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restart legacy service: %w", err))
		}
	}
	if len(rollbackErrors) == 0 {
		return fmt.Errorf("runner service failed to start; legacy service restored: %w", cause)
	}
	return errors.Join(append([]error{fmt.Errorf("runner service failed to start: %w", cause)}, rollbackErrors...)...)
}

func legacyServiceRunning(user bool) bool {
	switch runtime.GOOS {
	case "linux":
		args := append(systemdUserFlag(user), "is-active", "--quiet", legacyServiceName)
		return runCmd("systemctl", args...) == nil
	case "darwin":
		label := strings.TrimSuffix(legacyLaunchdPlist, filepath.Ext(legacyLaunchdPlist))
		return runCmd("launchctl", "list", label) == nil
	default:
		return false
	}
}

func newServiceRunning(user bool) bool {
	switch runtime.GOOS {
	case "linux":
		args := append(systemdUserFlag(user), "is-active", "--quiet", serviceName)
		return runCmd("systemctl", args...) == nil
	case "darwin":
		return runCmd("launchctl", "list", launchdLabel) == nil
	default:
		return false
	}
}

func waitForNewService(user bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	var activeSince time.Time
	for {
		if newServiceRunning(user) {
			if activeSince.IsZero() {
				activeSince = time.Now()
			} else if time.Since(activeSince) >= 500*time.Millisecond {
				return true
			}
		} else {
			activeSince = time.Time{}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func stopLegacyService(user bool) error {
	switch runtime.GOOS {
	case "linux":
		return runCmd("systemctl", append(systemdUserFlag(user), "stop", legacyServiceName)...)
	case "darwin":
		return runCmd("launchctl", "unload", legacyLaunchdPlistPath(user))
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func startLegacyService(user bool) error {
	switch runtime.GOOS {
	case "linux":
		return runCmd("systemctl", append(systemdUserFlag(user), "start", legacyServiceName)...)
	case "darwin":
		return runCmd("launchctl", "load", "-w", legacyLaunchdPlistPath(user))
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// migrateVolume removes the legacy shared docker volume. Best-effort; returns
// whether it removed anything.
func migrateVolume(ctx context.Context) bool {
	client, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		return false
	}
	defer func() { _ = client.Close() }()
	if _, err := client.VolumeInspect(ctx, legacySharedVolume, dockerclient.VolumeInspectOptions{}); err != nil {
		return false
	}
	rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := client.VolumeRemove(rmCtx, legacySharedVolume, dockerclient.VolumeRemoveOptions{Force: true}); err != nil {
		warnLegacy("found legacy volume %s but could not remove it: %v", legacySharedVolume, err)
		return false
	}
	return true
}
