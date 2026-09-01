package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

const migrationTestSecret = "migration-test-secret-do-not-print"

type fakeMigrationServiceManager struct {
	actions      *[]string
	installed    *bool
	running      *bool
	installOpts  installOpts
	installErr   error
	startErr     error
	uninstallErr error
	stopErr      error
}

func (m *fakeMigrationServiceManager) install(opts installOpts) error {
	*m.actions = append(*m.actions, "install-new")
	m.installOpts = opts
	if m.installErr == nil {
		*m.installed = true
	}
	return m.installErr
}

func (m *fakeMigrationServiceManager) uninstall(bool) error {
	*m.actions = append(*m.actions, "uninstall-new")
	if m.uninstallErr == nil {
		*m.installed = false
		*m.running = false
	}
	return m.uninstallErr
}

func (m *fakeMigrationServiceManager) start(bool) error {
	*m.actions = append(*m.actions, "start-new")
	if m.startErr == nil {
		*m.running = true
	}
	return m.startErr
}

func (m *fakeMigrationServiceManager) stop(bool) error {
	*m.actions = append(*m.actions, "stop-new")
	if m.stopErr == nil {
		*m.running = false
	}
	return m.stopErr
}

func (m *fakeMigrationServiceManager) restart(bool) error         { return nil }
func (m *fakeMigrationServiceManager) status(bool) error          { return nil }
func (m *fakeMigrationServiceManager) logs(bool, bool, int) error { return nil }

func validLegacyConfig() []byte {
	return []byte(`# keep this comment and ordering
url = "https://github.com/example"
name = "legacy-runners"
token = "` + migrationTestSecret + `"
max-runners = 2
backend = "docker" # legacy spelling
`)
}

func testMigrationPaths(t *testing.T) configMigrationPaths {
	t.Helper()
	root := t.TempDir()
	return configMigrationPaths{
		Source:       filepath.Join(root, "runscaler", "config.toml"),
		Target:       filepath.Join(root, "runner", "config.toml"),
		BackupDir:    filepath.Join(root, "runner", "backups"),
		Mode:         "system",
		DirPerm:      0o755,
		RemoveSource: true,
	}
}

func writeTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode of %s = %o, want %o", path, got, want)
	}
}

func fixMigrationIdentity(t *testing.T) {
	t.Helper()
	originalVersion, originalCommit, originalDate, originalNow := version, commit, date, migrationNow
	version = "0.8.0"
	commit = "0123456789abcdef"
	date = "2026-09-01T01:02:03Z"
	migrationNow = func() time.Time { return time.Date(2026, 9, 1, 12, 34, 56, 0, time.UTC) }
	t.Cleanup(func() {
		version, commit, date, migrationNow = originalVersion, originalCommit, originalDate, originalNow
	})
}

func TestCanonicalizeDeprecatedConfigPreservesFormattingAndValues(t *testing.T) {
	input := []byte(`# backend = "comment"
description = """
backend = "inside multiline string"
"""
'backend' = "docker" # keep inline comment

[docker]
"shared-volume-ttl" = "24h"

[tart]
cache-space-budget = 40
`)
	want := []byte(`# backend = "comment"
description = """
backend = "inside multiline string"
"""
'provider' = "docker" # keep inline comment

[docker]
"shared-volume-max-age" = "24h"

[tart]
cache-budget = 40
`)
	got, changes, err := canonicalizeDeprecatedConfig(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonicalized config differs:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if len(changes) != 3 {
		t.Fatalf("changes = %v, want 3 entries", changes)
	}
}

func TestCanonicalizeDeprecatedConfigScopesScaleSetConflicts(t *testing.T) {
	input := []byte(`[[scaleset]]
backend = "docker"

[[scaleset]]
provider = "tart"
`)
	if _, _, err := canonicalizeDeprecatedConfig(input); err != nil {
		t.Fatalf("keys in separate scale sets must not conflict: %v", err)
	}

	conflict := []byte(`[[scaleset]]
backend = "docker"
provider = "tart"
`)
	if _, _, err := canonicalizeDeprecatedConfig(conflict); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("same-scale-set conflict error = %v, want explicit conflict", err)
	}
}

func TestMigrateConfigBacksUpCanonicalizesValidatesAndIsIdempotent(t *testing.T) {
	fixMigrationIdentity(t)
	paths := testMigrationPaths(t)
	original := validLegacyConfig()
	writeTestFile(t, paths.Source, original, 0o644)
	if err := os.MkdirAll(paths.BackupDir, 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := migrateConfigFile(paths, false)
	if err != nil {
		t.Fatalf("migrateConfigFile: %v", err)
	}
	if !result.Found || !result.TargetChanged || !result.TargetCreated || !result.BackupCreated {
		t.Fatalf("result = %+v, want found/changed/created/backup-created", result)
	}
	if !strings.Contains(filepath.Base(result.BackupPath), "pre-migrate-v0.8.0-20260901T123456Z-") {
		t.Fatalf("backup filename = %s, want version and UTC timestamp", filepath.Base(result.BackupPath))
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backup, original) {
		t.Fatal("backup does not exactly match original config")
	}
	assertFileMode(t, result.BackupPath, 0o600)
	assertFileMode(t, result.ManifestPath, 0o600)
	assertFileMode(t, paths.BackupDir, 0o700)
	target, err := os.ReadFile(paths.Target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(target, []byte(`provider = "docker" # legacy spelling`)) || bytes.Contains(target, []byte("backend =")) {
		t.Fatalf("target was not canonicalized:\n%s", target)
	}
	if !bytes.Contains(target, []byte(migrationTestSecret)) || !bytes.Contains(target, []byte("# keep this comment and ordering")) {
		t.Fatal("migration did not preserve secret value and comments")
	}
	assertFileMode(t, paths.Target, 0o600)

	manifestData, err := os.ReadFile(result.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(manifestData, []byte(migrationTestSecret)) {
		t.Fatal("backup manifest leaked config secret")
	}
	var manifest configBackupManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.RunnerVersion != "0.8.0" || manifest.RunnerCommit != "0123456789abcdef" {
		t.Fatalf("manifest identity = %q/%q", manifest.RunnerVersion, manifest.RunnerCommit)
	}
	if manifest.ConfigSHA256 != sha256Hex(original) {
		t.Fatalf("manifest checksum = %q", manifest.ConfigSHA256)
	}
	if !strings.Contains(manifest.RestoreCommand, shellQuotePath(paths.Target)) {
		t.Fatalf("restore command = %q, want migrated target", manifest.RestoreCommand)
	}

	second, err := migrateConfigFile(paths, false)
	if err != nil {
		t.Fatalf("second migrateConfigFile: %v", err)
	}
	if second.TargetChanged || second.BackupCreated || second.BackupPath != result.BackupPath {
		t.Fatalf("second result = %+v, want existing target and reused backup", second)
	}
	backups, err := filepath.Glob(filepath.Join(paths.BackupDir, "*.bak"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("deduplicated backups = %v, %v; want one", backups, err)
	}
}

func TestMigrateConfigDryRunWritesNothing(t *testing.T) {
	paths := testMigrationPaths(t)
	writeTestFile(t, paths.Source, validLegacyConfig(), 0o600)
	result, err := migrateConfigFile(paths, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || !result.TargetChanged || len(result.Changes) != 1 {
		t.Fatalf("dry-run result = %+v", result)
	}
	if _, err := os.Stat(paths.Target); !os.IsNotExist(err) {
		t.Fatalf("dry-run created target: %v", err)
	}
	if _, err := os.Stat(paths.BackupDir); !os.IsNotExist(err) {
		t.Fatalf("dry-run created backup directory: %v", err)
	}
}

func TestMigrateConfigValidationFailureWritesNothing(t *testing.T) {
	paths := testMigrationPaths(t)
	writeTestFile(t, paths.Source, []byte("backend = \"docker\"\n"), 0o600)
	if _, err := migrateConfigFile(paths, false); err == nil || !strings.Contains(err.Error(), "registration URL") {
		t.Fatalf("validation error = %v", err)
	}
	if _, err := os.Stat(paths.Target); !os.IsNotExist(err) {
		t.Fatalf("invalid migration created target: %v", err)
	}
	if _, err := os.Stat(paths.BackupDir); !os.IsNotExist(err) {
		t.Fatalf("invalid migration created backups despite no config mutation: %v", err)
	}
}

func TestMigrateConfigConflictBacksUpBothAndDoesNotOverwrite(t *testing.T) {
	fixMigrationIdentity(t)
	paths := testMigrationPaths(t)
	source := validLegacyConfig()
	target := bytes.ReplaceAll(validLegacyConfig(), []byte("legacy-runners"), []byte("different-runners"))
	writeTestFile(t, paths.Source, source, 0o600)
	writeTestFile(t, paths.Target, target, 0o600)

	result, err := migrateConfigFile(paths, false)
	if err == nil || !strings.Contains(err.Error(), "neither was overwritten") {
		t.Fatalf("conflict error = %v", err)
	}
	if result.BackupPath == "" || result.TargetBackupPath == "" || result.BackupPath == result.TargetBackupPath {
		t.Fatalf("conflict backups = %q/%q, want distinct source and target backups", result.BackupPath, result.TargetBackupPath)
	}
	got, readErr := os.ReadFile(paths.Target)
	if readErr != nil || !bytes.Equal(got, target) {
		t.Fatal("conflict overwrote existing target")
	}
}

func TestRollbackConfigMigration(t *testing.T) {
	t.Run("new target is removed", func(t *testing.T) {
		paths := testMigrationPaths(t)
		writeTestFile(t, paths.Source, validLegacyConfig(), 0o600)
		result, err := migrateConfigFile(paths, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := rollbackConfigMigration(result); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(paths.Target); !os.IsNotExist(err) {
			t.Fatalf("rollback left newly created target: %v", err)
		}
	})

	t.Run("in-place target is restored", func(t *testing.T) {
		fixMigrationIdentity(t)
		paths := testMigrationPaths(t)
		paths.Source = paths.Target
		original := validLegacyConfig()
		writeTestFile(t, paths.Target, original, 0o600)
		result, err := migrateConfigFile(paths, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := rollbackConfigMigration(result); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(paths.Target)
		if err != nil || !bytes.Equal(got, original) {
			t.Fatal("rollback did not restore in-place target")
		}
	})
}

func TestResolveConfigMigrationPathsUsesUserOwnedDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := &cobra.Command{Use: "migrate-test"}
	c.Flags().String("config", "", "")
	c.Flags().String("backup-dir", "", "")
	paths, err := resolveConfigMigrationPaths(c, true)
	if err != nil {
		t.Fatal(err)
	}
	if paths.Source != filepath.Join(home, ".config", "runscaler", "config.toml") {
		t.Fatalf("user source = %s", paths.Source)
	}
	if paths.Target != filepath.Join(home, ".config", "runner", "config.toml") {
		t.Fatalf("user target = %s", paths.Target)
	}
	if paths.BackupDir != filepath.Join(home, ".config", "runner", "backups") || paths.DirPerm != 0o700 {
		t.Fatalf("user backup/permission = %s/%o", paths.BackupDir, paths.DirPerm)
	}

	explicit := filepath.Join(home, "custom.toml")
	if err := c.Flags().Set("config", explicit); err != nil {
		t.Fatal(err)
	}
	paths, err = resolveConfigMigrationPaths(c, true)
	if err != nil {
		t.Fatal(err)
	}
	if paths.Source != explicit || paths.RemoveSource {
		t.Fatalf("explicit source = %s remove=%v, want preserved", paths.Source, paths.RemoveSource)
	}
}

func TestParseLegacyServiceInvocation(t *testing.T) {
	t.Run("systemd", func(t *testing.T) {
		invocation, err := parseSystemdServiceInvocation([]byte(`[Service]
ExecStart=/opt/runscaler --config=/srv/runner/config.toml
`))
		if err != nil {
			t.Fatal(err)
		}
		if invocation.BinaryPath != "/opt/runscaler" || invocation.ConfigPath != "/srv/runner/config.toml" {
			t.Fatalf("invocation = %+v", invocation)
		}
	})

	t.Run("launchd", func(t *testing.T) {
		invocation, err := parseLaunchdServiceInvocation([]byte(`<?xml version="1.0"?>
<plist><dict>
<key>Label</key><string>com.runscaler.agent</string>
<key>ProgramArguments</key><array>
<string>/Applications/runscaler</string><string>--config</string><string>/Users/test/custom.toml</string>
</array>
</dict></plist>`))
		if err != nil {
			t.Fatal(err)
		}
		if invocation.BinaryPath != "/Applications/runscaler" || invocation.ConfigPath != "/Users/test/custom.toml" {
			t.Fatalf("invocation = %+v", invocation)
		}
	})
}

func TestResolveConfigMigrationPathsDiscoversUserServiceConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	customConfig := filepath.Join(home, "runner config.toml")
	var servicePath string
	var serviceData []byte
	switch runtime.GOOS {
	case "linux":
		// systemd paths cannot contain unescaped spaces with the legacy renderer,
		// so use a conventional custom path for this platform fixture.
		customConfig = filepath.Join(home, "custom.toml")
		servicePath = filepath.Join(home, ".config", "systemd", "user", legacySystemdUnit)
		serviceData = []byte("[Service]\nExecStart=/usr/bin/runscaler --config " + customConfig + "\n")
	case "darwin":
		servicePath = filepath.Join(home, "Library", "LaunchAgents", legacyLaunchdPlist)
		serviceData = []byte(`<?xml version="1.0"?><plist><dict><key>ProgramArguments</key><array><string>/usr/bin/runscaler</string><string>--config</string><string>` + customConfig + `</string></array></dict></plist>`)
	default:
		t.Skip("unsupported OS")
	}
	writeTestFile(t, servicePath, serviceData, 0o644)

	c := &cobra.Command{Use: "migrate-test"}
	c.Flags().String("config", "", "")
	c.Flags().String("backup-dir", "", "")
	paths, err := resolveConfigMigrationPaths(c, true)
	if err != nil {
		t.Fatal(err)
	}
	if paths.Source != customConfig || paths.RemoveSource {
		t.Fatalf("discovered source = %s remove=%v, want custom source preserved", paths.Source, paths.RemoveSource)
	}
}

func newMigrationServiceTestDeps(manager *fakeMigrationServiceManager, actions *[]string, legacyInstalled, legacyRunning, newInstalled, newRunning *bool) migrationServiceDeps {
	return migrationServiceDeps{
		legacyInstalled: func(bool) bool { return *legacyInstalled },
		newInstalled:    func(bool) bool { return *newInstalled },
		legacyRunning:   func(bool) bool { return *legacyRunning },
		newRunning:      func(bool) bool { return *newRunning },
		waitNew:         func(bool) bool { return *newRunning },
		validateNewConfig: func(bool, string) error {
			return nil
		},
		newManager:     func() (serviceManager, error) { return manager, nil },
		executable:     func() (string, error) { return "/usr/local/bin/runner", nil },
		evalSymlinks:   func(path string) (string, error) { return path, nil },
		detectProvider: func(string) string { return "docker" },
		detectDrain:    func(string) (*time.Duration, error) { return nil, nil },
		stopLegacy: func(bool) error {
			*actions = append(*actions, "stop-legacy")
			*legacyRunning = false
			return nil
		},
		startLegacy: func(bool) error {
			*actions = append(*actions, "start-legacy")
			*legacyRunning = true
			return nil
		},
		removeLegacyFile: func(bool) error {
			*actions = append(*actions, "remove-legacy")
			*legacyInstalled = false
			return nil
		},
	}
}

func TestMigrateServiceRejectsExistingNewServiceWithWrongConfig(t *testing.T) {
	var actions []string
	legacyInstalled, legacyRunning := true, true
	newInstalled, newRunning := true, false
	manager := &fakeMigrationServiceManager{actions: &actions, installed: &newInstalled, running: &newRunning}
	deps := newMigrationServiceTestDeps(manager, &actions, &legacyInstalled, &legacyRunning, &newInstalled, &newRunning)
	deps.validateNewConfig = func(bool, string) error {
		return errors.New("existing runner service uses a different config")
	}

	acted, cutover, err := migrateServiceWithDeps(true, "/home/test/.config/runner/config.toml", deps)
	if err == nil || !strings.Contains(err.Error(), "different config") {
		t.Fatalf("error = %v, want config mismatch", err)
	}
	if acted || cutover || len(actions) != 0 || !legacyRunning || newRunning {
		t.Fatalf("migration mutated service state: acted=%v cutover=%v actions=%v legacy=%v new=%v",
			acted, cutover, actions, legacyRunning, newRunning)
	}
}

func TestMigrateServiceCutsOverBeforeRemovingLegacy(t *testing.T) {
	var actions []string
	legacyInstalled, legacyRunning := true, true
	newInstalled, newRunning := false, false
	manager := &fakeMigrationServiceManager{
		actions: &actions, installed: &newInstalled, running: &newRunning,
	}
	deps := newMigrationServiceTestDeps(manager, &actions, &legacyInstalled, &legacyRunning, &newInstalled, &newRunning)
	acted, cutover, err := migrateServiceWithDeps(true, "/home/test/.config/runner/config.toml", deps)
	if err != nil {
		t.Fatal(err)
	}
	if !acted || !cutover || legacyInstalled || !newInstalled || !newRunning {
		t.Fatalf("state acted/cutover/legacy/new/running = %v/%v/%v/%v/%v", acted, cutover, legacyInstalled, newInstalled, newRunning)
	}
	want := []string{"install-new", "stop-legacy", "start-new", "remove-legacy"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
	if !manager.installOpts.user || manager.installOpts.configPath != "/home/test/.config/runner/config.toml" || !manager.installOpts.noStart {
		t.Fatalf("install options = %+v", manager.installOpts)
	}
}

func TestMigrateServiceRestoresLegacyWhenNewStartFails(t *testing.T) {
	var actions []string
	legacyInstalled, legacyRunning := true, true
	newInstalled, newRunning := false, false
	manager := &fakeMigrationServiceManager{
		actions: &actions, installed: &newInstalled, running: &newRunning,
		startErr: errors.New("start failed"),
	}
	deps := newMigrationServiceTestDeps(manager, &actions, &legacyInstalled, &legacyRunning, &newInstalled, &newRunning)
	_, cutover, err := migrateServiceWithDeps(false, "/etc/runner/config.toml", deps)
	if err == nil || !strings.Contains(err.Error(), "legacy service restored") {
		t.Fatalf("error = %v, want restored legacy message", err)
	}
	if cutover || !legacyRunning || newInstalled || newRunning {
		t.Fatalf("rollback state cutover/legacy/new/running = %v/%v/%v/%v", cutover, legacyRunning, newInstalled, newRunning)
	}
	want := []string{"install-new", "stop-legacy", "start-new", "uninstall-new", "start-legacy"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
}

func TestMigrateServiceKeepsCutoverWhenLegacyFileRemovalFails(t *testing.T) {
	var actions []string
	legacyInstalled, legacyRunning := true, true
	newInstalled, newRunning := false, false
	manager := &fakeMigrationServiceManager{actions: &actions, installed: &newInstalled, running: &newRunning}
	deps := newMigrationServiceTestDeps(manager, &actions, &legacyInstalled, &legacyRunning, &newInstalled, &newRunning)
	deps.removeLegacyFile = func(bool) error {
		actions = append(actions, "remove-legacy")
		return errors.New("permission denied")
	}
	_, cutover, err := migrateServiceWithDeps(false, "/etc/runner/config.toml", deps)
	if err == nil || !cutover {
		t.Fatalf("error/cutover = %v/%v, want cleanup error after successful cutover", err, cutover)
	}
	if !newRunning || legacyRunning {
		t.Fatalf("services after cutover cleanup error: new=%v legacy=%v", newRunning, legacyRunning)
	}
}

func TestRunMigrateRollsBackConfigWhenServiceCutoverFails(t *testing.T) {
	fixMigrationIdentity(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := filepath.Join(home, ".config", "runscaler", "config.toml")
	target := filepath.Join(home, ".config", "runner", "config.toml")
	writeTestFile(t, source, validLegacyConfig(), 0o600)

	originalMigrateService := migrateServiceForCommand
	migrateServiceForCommand = func(bool, string) (bool, bool, error) {
		return false, false, errors.New("simulated service failure")
	}
	t.Cleanup(func() { migrateServiceForCommand = originalMigrateService })

	c := &cobra.Command{Use: "migrate-test"}
	c.Flags().Bool("user", true, "")
	c.Flags().Bool("dry-run", false, "")
	c.Flags().Bool("cleanup", false, "")
	c.Flags().String("config", "", "")
	c.Flags().String("backup-dir", "", "")
	err := runMigrate(c, nil)
	if err == nil || !strings.Contains(err.Error(), "simulated service failure") {
		t.Fatalf("runMigrate error = %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("failed cutover left migrated target: %v", err)
	}
	if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, validLegacyConfig()) {
		t.Fatal("failed cutover changed legacy source")
	}
	backups, err := filepath.Glob(filepath.Join(home, ".config", "runner", "backups", "*.bak"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups after rollback = %v, %v; want one retained backup", backups, err)
	}
}

func TestRunMigrateRemovesDefaultLegacyConfigOnlyAfterCutover(t *testing.T) {
	fixMigrationIdentity(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := filepath.Join(home, ".config", "runscaler", "config.toml")
	target := filepath.Join(home, ".config", "runner", "config.toml")
	writeTestFile(t, source, validLegacyConfig(), 0o600)

	originalMigrateService := migrateServiceForCommand
	migrateServiceForCommand = func(_ bool, gotConfig string) (bool, bool, error) {
		if gotConfig != target {
			t.Fatalf("service config = %s, want %s", gotConfig, target)
		}
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("legacy source was removed before service cutover: %v", err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("migrated target missing before service cutover: %v", err)
		}
		return true, true, nil
	}
	t.Cleanup(func() { migrateServiceForCommand = originalMigrateService })

	c := &cobra.Command{Use: "migrate-test"}
	c.Flags().Bool("user", true, "")
	c.Flags().Bool("dry-run", false, "")
	c.Flags().Bool("cleanup", false, "")
	c.Flags().String("config", "", "")
	c.Flags().String("backup-dir", "", "")
	if err := runMigrate(c, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("legacy source remains after successful cutover: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("migrated target missing after cutover: %v", err)
	}
}

func TestNewServiceInstalledUserLevel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if newServiceInstalled(true) {
		t.Error("should be false with no new unit/plist present")
	}

	var path string
	switch runtime.GOOS {
	case "linux":
		path = filepath.Join(home, ".config", "systemd", "user", systemdUnitFile)
	case "darwin":
		path = filepath.Join(home, "Library", "LaunchAgents", launchdPlistFile)
	default:
		t.Skip("unsupported OS")
	}
	writeTestFile(t, path, []byte("test service"), 0o644)
	if !newServiceInstalled(true) {
		t.Error("should detect new service for current OS")
	}
}
