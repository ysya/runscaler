# Runner Migrate & Upgrade-Compat Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let existing `runscaler` deployments upgrade to `runner` without breaking — via config-path fallback, a drop-in compat for legacy start invocations, and an opt-in `runner migrate` command that converts config/volume/service to the new format.

**Architecture:** Gradual deprecation (industry standard: GitHub CLI / Docker Compose v2 / AWS CLI v2), NOT a forced gate. All legacy identifiers and detection live in one isolated `legacy.go` module so the compat layer is maintainable and removable later. The start logic is extracted into a shared `startScaling` so both `runner run` and the root drop-in branch reuse it.

**Tech Stack:** Go, cobra, viper, systemd/launchd, Docker SDK.

**Spec:** `docs/superpowers/specs/2026-06-17-runner-migrate-and-compat-design.md`

**Branch:** `feat/runner-migrate` (already created, off `main`).

---

## File Structure

- `cmd/runner/legacy.go` (new) — legacy identifiers (as `var` for testability) + detection helpers + deprecation warning helper. Single home for all compat knowledge.
- `cmd/runner/legacy_test.go` (new) — detection tests.
- `cmd/runner/loadconfig.go` (modify) — add `/etc/runscaler` fallback search + deprecation warning.
- `cmd/runner/main.go` (modify) — extract `startScaling`; give root a RunE that routes (help vs drop-in start); register `migrateCmd`.
- `cmd/runner/main_test.go` (modify) — drop-in routing tests.
- `cmd/runner/cmd_service.go` (modify) — extract parameterized `removeSystemdUnit` / `removeLaunchdPlist` so migrate can remove legacy-named services.
- `cmd/runner/cmd_migrate.go` (new) — `runner migrate [--user]` command.
- `cmd/runner/cmd_migrate_test.go` (new) — migrate config-move + idempotency tests.
- `README.md` (modify) — migration section uses `runner migrate`.

---

## Task 1: `legacy.go` — centralized legacy identifiers + detection

**Files:**
- Create: `cmd/runner/legacy.go`
- Create: `cmd/runner/legacy_test.go`

- [ ] **Step 1: Write the failing tests**

Create `cmd/runner/legacy_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyConfigExists(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")

	orig := legacyConfigPath
	legacyConfigPath = cfg
	defer func() { legacyConfigPath = orig }()

	if legacyConfigExists() {
		t.Error("should be false before file exists")
	}
	if err := os.WriteFile(cfg, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !legacyConfigExists() {
		t.Error("should be true after file exists")
	}
}

func TestLegacyServiceInstalledUserLevel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if legacyServiceInstalled(true) {
		t.Error("should be false with no legacy unit/plist present")
	}
	// systemd user unit
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, legacySystemdUnit), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !legacyServiceInstalled(true) {
		t.Error("should detect legacy user systemd unit")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/runner/ -run 'TestLegacy' -v`
Expected: FAIL (undefined: legacyConfigPath, legacyConfigExists, etc.)

- [ ] **Step 3: Create `legacy.go`**

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Legacy identifiers from before the runscaler→runner rename, kept for upgrade
// compatibility. Centralized here so the compat layer is easy to maintain and
// to remove in a future release. Declared as vars (not consts) so tests can
// point detection at temp paths.
var (
	legacyConfigDir    = "/etc/runscaler"
	legacyConfigPath   = "/etc/runscaler/config.toml"
	legacySystemdUnit  = "runscaler.service"
	legacyServiceName  = "runscaler"
	legacyLaunchdLabel = "com.runscaler.agent"
	legacyLaunchdPlist = "com.runscaler.agent.plist"
	legacySharedVolume = "runscaler-shared"
	legacyTokenEnv     = "RUNSCALER_TOKEN"
)

// legacyConfigExists reports whether a config remains at the old location.
func legacyConfigExists() bool {
	_, err := os.Stat(legacyConfigPath)
	return err == nil
}

// legacySystemdUnitPath returns the legacy systemd unit path for the level.
func legacySystemdUnitPath(user bool) string {
	if user {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "systemd", "user", legacySystemdUnit)
	}
	return filepath.Join(systemdSystemDir, legacySystemdUnit)
}

// legacyLaunchdPlistPath returns the legacy launchd plist path for the level.
func legacyLaunchdPlistPath(user bool) string {
	if user {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "LaunchAgents", legacyLaunchdPlist)
	}
	return filepath.Join(launchdSystemDir, legacyLaunchdPlist)
}

// legacyServiceInstalled reports whether a legacy systemd unit or launchd plist
// is present at the given level.
func legacyServiceInstalled(user bool) bool {
	for _, p := range []string{legacySystemdUnitPath(user), legacyLaunchdPlistPath(user)} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// warnLegacy prints a one-line deprecation notice to stderr.
func warnLegacy(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "  ⚠ "+format+"\n", args...)
}
```

(`systemdSystemDir` and `launchdSystemDir` are defined in `cmd_service.go`, same package.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/runner/ -run 'TestLegacy' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/runner/legacy.go cmd/runner/legacy_test.go
git commit -m "feat(migrate): add legacy identifiers and detection module"
```

---

## Task 2: config-path fallback to `/etc/runscaler` with deprecation warning

**Files:**
- Modify: `cmd/runner/loadconfig.go`
- Test: `cmd/runner/loadconfig_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `cmd/runner/loadconfig_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestLoadConfigFallsBackToLegacyDir(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	// Point the legacy dir at a temp dir containing a config, and make the
	// normal search dirs empty by running from an empty cwd.
	legacyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacyDir, "config.toml"),
		[]byte("url = \"https://github.com/org\"\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origDir := legacyConfigDir
	legacyConfigDir = legacyDir
	defer func() { legacyConfigDir = origDir }()

	emptyCwd := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(emptyCwd); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	c := &cobra.Command{}
	c.PersistentFlags().String("config", "", "")

	cfg, err := loadConfig(c)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Defaults.RegistrationURL != "https://github.com/org" {
		t.Errorf("expected config loaded from legacy dir, got url=%q", cfg.Defaults.RegistrationURL)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/runner/ -run TestLoadConfigFallsBackToLegacyDir -v`
Expected: FAIL (legacy dir not searched, URL empty).

- [ ] **Step 3: Add the fallback in `loadconfig.go`**

In `cmd/runner/loadconfig.go`, change the `else` branch to add the legacy search path and warn when the config actually came from there:

```go
	} else {
		viper.SetConfigName("config")
		viper.SetConfigType("toml")
		viper.AddConfigPath(".")
		viper.AddConfigPath("/etc/runner")
		viper.AddConfigPath(legacyConfigDir) // legacy /etc/runscaler — deprecated
		_ = viper.ReadInConfig()             // ignore error — default paths are optional

		if used := viper.ConfigFileUsed(); used != "" && filepath.Dir(used) == legacyConfigDir {
			warnLegacy("config loaded from legacy path %s — run 'runner migrate' or move it to /etc/runner", used)
		}
	}
```

Add `"path/filepath"` to the imports.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/runner/ -run TestLoadConfigFallsBackToLegacyDir -v`
Expected: PASS.

- [ ] **Step 5: Run full package tests**

Run: `go test ./cmd/runner/ 2>&1 | tail -3`
Expected: ok.

- [ ] **Step 6: Commit**

```bash
git add cmd/runner/loadconfig.go cmd/runner/loadconfig_test.go
git commit -m "feat(migrate): fall back to legacy /etc/runscaler config with warning"
```

---

## Task 3: extract `startScaling` + root drop-in routing

**Files:**
- Modify: `cmd/runner/main.go`
- Modify: `cmd/runner/main_test.go`

- [ ] **Step 1: Write the failing tests**

The root now gains a routing RunE, so the existing `TestRootHasNoRunE` (from the rename refactor — it asserts `cmd.RunE == nil`) becomes obsolete. Its intent ("bare runner doesn't start") is replaced by `TestRootBareInvocationDoesNotStart` below. **Delete `TestRootHasNoRunE`** from `cmd/runner/main_test.go`, then add these:

```go
func TestRootBareInvocationDoesNotStart(t *testing.T) {
	called := false
	orig := startScaling
	startScaling = func(c *cobra.Command) error { called = true; return nil }
	defer func() { startScaling = orig }()

	cmd.SetArgs([]string{})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	defer cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if called {
		t.Error("bare `runner` must print help, not start scaling")
	}
}

func TestRootConfigInvocationStartsViaDropIn(t *testing.T) {
	called := false
	orig := startScaling
	startScaling = func(c *cobra.Command) error { called = true; return nil }
	defer func() { startScaling = orig }()

	cmd.SetArgs([]string{"--config", "/nonexistent/x.toml"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	defer cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !called {
		t.Error("`runner --config X` must start via drop-in compat")
	}
}
```

Add `"strings"` to `main_test.go` imports if missing.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/runner/ -run 'TestRootBareInvocation|TestRootConfigInvocation' -v`
Expected: FAIL (startScaling undefined; root has no RunE).

- [ ] **Step 3: Extract `startScaling` and add root RunE**

In `cmd/runner/main.go`, replace the inline `runCommand.RunE` closure with a call to a shared package-level `startScaling`, and give the root `cmd` a routing RunE. Define `startScaling` as a `var` (so tests can swap it):

```go
// startScaling loads config, sets up signal handling, and runs the scaler.
// Shared by `runner run` and the root drop-in compat path. var (not func) so
// tests can stub it.
var startScaling = func(cmd *cobra.Command) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Force exit on second signal
	go func() {
		<-ctx.Done()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Fprintln(os.Stderr, "\nForce exit")
		os.Exit(1)
	}()

	return run(ctx, cfg)
}
```

Set `runCommand.RunE` to:

```go
	RunE: func(cmd *cobra.Command, args []string) error {
		return startScaling(cmd)
	},
```

Add a `RunE` to the root `cmd` (the `var cmd = &cobra.Command{...}` block) — append this field:

```go
	RunE: func(cmd *cobra.Command, args []string) error {
		// Drop-in compat: an old `runscaler --config X` invocation (e.g. a
		// pre-rename systemd unit after self-update) reaches the root with
		// --config set and no subcommand. Warn and start anyway so the
		// service keeps working; bare `runner` still just prints help.
		if cmd.Flags().Changed("config") {
			warnLegacy("starting via `runner --config` is deprecated — use `runner run` (or `runner migrate` to update your service)")
			return startScaling(cmd)
		}
		return cmd.Help()
	},
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/runner/ -run 'TestRoot' -v`
Expected: PASS (existing root tests + the two new ones).

- [ ] **Step 5: Manual smoke check**

Run: `go run ./cmd/runner` → Expected: prints help, does not start.
Run: `go run ./cmd/runner run --help` → Expected: shows start flags.
Then: `rm -f runner` if a binary was produced.

- [ ] **Step 6: Commit**

```bash
git add cmd/runner/main.go cmd/runner/main_test.go
git commit -m "feat(migrate): drop-in compat for legacy 'runner --config' start"
```

---

## Task 4: parameterize service removal so migrate can drop legacy services

**Files:**
- Modify: `cmd/runner/cmd_service.go`

- [ ] **Step 1: Extract `removeSystemdUnit` / `removeLaunchdPlist` helpers**

In `cmd/runner/cmd_service.go`, add two package-level helpers that take identifiers (so they work for both current and legacy names):

```go
// removeSystemdUnit stops, disables, and removes a systemd unit by name.
func removeSystemdUnit(user bool, unitFile, serviceName string) error {
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
	_ = runCmd("systemctl", append(userFlag, "stop", serviceName)...)
	_ = runCmd("systemctl", append(userFlag, "disable", serviceName)...)
	if err := os.Remove(unitPath); err != nil {
		return fmt.Errorf("failed to remove unit file: %w", err)
	}
	_ = runCmd("systemctl", append(userFlag, "daemon-reload")...)
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
```

- [ ] **Step 2: Make the existing uninstall methods use the helpers**

Replace the body of `(m *systemdManager) uninstall(user bool)` with:

```go
func (m *systemdManager) uninstall(user bool) error {
	if err := removeSystemdUnit(user, systemdUnitFile, serviceName); err != nil {
		return err
	}
	fmt.Printf("  ✓ Service stopped, disabled, and removed\n")
	return nil
}
```

Replace the body of `(m *launchdManager) uninstall(user bool)` with:

```go
func (m *launchdManager) uninstall(user bool) error {
	if err := removeLaunchdPlist(m.plistPath(user)); err != nil {
		return err
	}
	fmt.Printf("  ✓ Service unloaded and removed\n")
	return nil
}
```

- [ ] **Step 3: Run service tests to verify nothing broke**

Run: `go test ./cmd/runner/ -run 'TestSystemd|TestLaunchd' -v && go build ./cmd/runner && rm -f runner`
Expected: PASS + BUILD OK. (The template tests are unaffected; this is an internal refactor.)

- [ ] **Step 4: Commit**

```bash
git add cmd/runner/cmd_service.go
git commit -m "refactor(service): extract parameterized unit/plist removal helpers"
```

---

## Task 5: `runner migrate [--user]` command

**Files:**
- Create: `cmd/runner/cmd_migrate.go`
- Create: `cmd/runner/cmd_migrate_test.go`

- [ ] **Step 1: Write the failing test (config move + idempotency)**

Create `cmd/runner/cmd_migrate_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateConfigMovesAndIsIdempotent(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	oldCfg := filepath.Join(oldDir, "config.toml")
	newCfg := filepath.Join(newDir, "config.toml")
	if err := os.WriteFile(oldCfg, []byte("url=\"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origOld, origNew := legacyConfigPath, newConfigPath
	legacyConfigPath, newConfigPath = oldCfg, newCfg
	defer func() { legacyConfigPath, newConfigPath = origOld, origNew }()

	moved, err := migrateConfig()
	if err != nil {
		t.Fatalf("migrateConfig: %v", err)
	}
	if !moved {
		t.Error("expected config to be moved")
	}
	if _, err := os.Stat(newCfg); err != nil {
		t.Errorf("new config missing: %v", err)
	}
	if _, err := os.Stat(oldCfg); !os.IsNotExist(err) {
		t.Errorf("old config should be gone after move")
	}

	// Idempotent: second run finds nothing to move.
	moved2, err := migrateConfig()
	if err != nil {
		t.Fatalf("second migrateConfig: %v", err)
	}
	if moved2 {
		t.Error("second run should be a no-op")
	}
}

func TestMigrateConfigSkipsWhenTargetExists(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	oldCfg := filepath.Join(oldDir, "config.toml")
	newCfg := filepath.Join(newDir, "config.toml")
	if err := os.WriteFile(oldCfg, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newCfg, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origOld, origNew := legacyConfigPath, newConfigPath
	legacyConfigPath, newConfigPath = oldCfg, newCfg
	defer func() { legacyConfigPath, newConfigPath = origOld, origNew }()

	moved, err := migrateConfig()
	if err != nil {
		t.Fatalf("migrateConfig: %v", err)
	}
	if moved {
		t.Error("must not move when target exists")
	}
	data, _ := os.ReadFile(newCfg)
	if string(data) != "new\n" {
		t.Error("existing target must not be overwritten")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/runner/ -run TestMigrateConfig -v`
Expected: FAIL (undefined: newConfigPath, migrateConfig).

- [ ] **Step 3: Create `cmd_migrate.go`**

```go
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	dockerclient "github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

// newConfigPath is the destination for migrated config (var for testability).
var newConfigPath = defaultConfigPath // "/etc/runner/config.toml"

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate an existing runscaler install to runner",
	Long: `Move config from /etc/runscaler to /etc/runner, reinstall the service
under the new name, and clean up the old docker volume. Idempotent — safe to
run multiple times; a fresh install has nothing to migrate.`,
	RunE: runMigrate,
}

func init() {
	migrateCmd.Flags().Bool("user", false, "Migrate user-level service")
	cmd.AddCommand(migrateCmd)
}

func runMigrate(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	if err := checkPrivileges(user); err != nil {
		return err
	}

	did := false

	moved, err := migrateConfig()
	if err != nil {
		return fmt.Errorf("config migration failed: %w", err)
	}
	if moved {
		fmt.Printf("  ✓ Moved config %s → %s\n", legacyConfigPath, newConfigPath)
		did = true
	}

	migratedSvc, err := migrateService(user)
	if err != nil {
		return fmt.Errorf("service migration failed: %w", err)
	}
	if migratedSvc {
		fmt.Printf("  ✓ Reinstalled service under the new name\n")
		did = true
	}

	if cleaned := migrateVolume(cmd.Context()); cleaned {
		fmt.Printf("  ✓ Removed legacy docker volume %s\n", legacySharedVolume)
		did = true
	}

	if os.Getenv(legacyTokenEnv) != "" {
		warnLegacy("the %s env var is deprecated — rename it to RUNNER_TOKEN", legacyTokenEnv)
	}

	if !did {
		fmt.Println("  ✓ Nothing to migrate")
	}
	return nil
}

// migrateConfig moves the legacy config to the new path. Returns whether it
// moved anything. No-op if the legacy file is absent or the target exists.
func migrateConfig() (bool, error) {
	if _, err := os.Stat(legacyConfigPath); err != nil {
		return false, nil // nothing at the old path
	}
	if _, err := os.Stat(newConfigPath); err == nil {
		warnLegacy("legacy config %s exists but %s is already present — leaving both, not overwriting", legacyConfigPath, newConfigPath)
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(newConfigPath), 0o755); err != nil {
		return false, err
	}
	if err := os.Rename(legacyConfigPath, newConfigPath); err != nil {
		return false, err
	}
	return true, nil
}

// migrateService removes a legacy-named service and installs the new one.
// Returns whether it did anything.
func migrateService(user bool) (bool, error) {
	if !legacyServiceInstalled(user) {
		return false, nil
	}
	switch runtime.GOOS {
	case "linux":
		if err := removeSystemdUnit(user, legacySystemdUnit, legacyServiceName); err != nil {
			return false, err
		}
	case "darwin":
		if err := removeLaunchdPlist(legacyLaunchdPlistPath(user)); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	mgr, err := newServiceManager()
	if err != nil {
		return false, err
	}
	binaryPath, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("cannot detect binary path: %w", err)
	}
	configPath := newConfigPath
	return true, mgr.install(installOpts{
		user:       user,
		configPath: configPath,
		binaryPath: binaryPath,
		backend:    detectBackend(configPath),
	})
}

// migrateVolume removes the legacy shared docker volume. Best-effort; returns
// whether it removed anything.
func migrateVolume(ctx context.Context) bool {
	client, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return false
	}
	defer client.Close()
	if _, err := client.VolumeInspect(ctx, legacySharedVolume); err != nil {
		return false
	}
	rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return client.VolumeRemove(rmCtx, legacySharedVolume, true) == nil
}
```

Note: `detectBackend`, `defaultConfigPath`, `checkPrivileges`, `newServiceManager`, `installOpts`, `removeSystemdUnit`, `removeLaunchdPlist` are all in the same `main` package (`cmd_service.go` / Task 4) — accessible directly. If `goimports`/`go build` reports an unused import, remove it.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/runner/ -run TestMigrateConfig -v`
Expected: PASS.

- [ ] **Step 5: Build + full package test + smoke**

Run: `go build ./cmd/runner && go test ./cmd/runner/ 2>&1 | tail -3`
Run: `go run ./cmd/runner migrate --help` → Expected: shows the migrate command help.
Then: `rm -f runner`.

- [ ] **Step 6: Commit**

```bash
git add cmd/runner/cmd_migrate.go cmd/runner/cmd_migrate_test.go
git commit -m "feat(migrate): add 'runner migrate' command (config, service, volume)"
```

---

## Task 6: update README migration section to use `runner migrate`

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Rewrite the "Upgrading from runscaler" section**

Replace the numbered manual steps with `runner migrate` as the primary path, keeping manual steps as a fallback:

```
## Upgrading from runscaler

The `runscaler` binary is now `runner` (start is a subcommand: `runner run`).
After installing the new binary, run:

    sudo runner migrate          # system-level install
    runner migrate --user        # user-level install

`migrate` moves your config (`/etc/runscaler` → `/etc/runner`), reinstalls the
service under the new name, and removes the old docker volume. It is idempotent.

During the transition the old binary's `runscaler update` can still fetch this
release (compat assets are published), a legacy `/etc/runscaler/config.toml` is
still read (with a warning), and an old `runscaler --config` service invocation
still starts (with a warning) — so nothing breaks before you migrate.

Manual alternative: uninstall the old service with the old binary, move the
config, run `sudo runner service install`, and `runner doctor --fix` to clean
the old volume.
```

- [ ] **Step 2: Verify**

Run: `grep -n "runner migrate" README.md`
Expected: shows the new section.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document 'runner migrate' as the upgrade path"
```

---

## Task 7: Final verification

- [ ] **Step 1: Build, vet, test**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail`
Expected: all pass.

- [ ] **Step 2: CLI smoke**

Run: `make build`
Run: `./runner` → prints help, does not start.
Run: `./runner migrate --help` → present.
Run: `./runner --config /nonexistent.toml` → prints the deprecation warning then fails on missing config (proves drop-in routing reached startScaling, not help).
Then: `rm -f runner`.

- [ ] **Step 3: Confirm legacy isolation**

Run: `grep -rn "runscaler" cmd/runner/legacy.go | head` — all legacy identifiers should live here, confirming the compat layer is centralized for easy future removal.

---

## Spec Coverage Check

- A. config fallback + warning → Task 2
- B. drop-in start compat (bare → help; `--config` → warn+start; `run` → start) → Task 3
- C. `runner migrate` (config + service + volume + env hint, system+user, idempotent) → Task 5 (+ Task 4 parameterized removal)
- D. legacy isolation module → Task 1
- README migration → Task 6
- final verification → Task 7
