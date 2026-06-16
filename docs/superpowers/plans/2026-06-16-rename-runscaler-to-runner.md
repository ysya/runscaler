# Rename `runscaler` → `runner` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rename the binary and user-facing identifiers from `runscaler` to `runner`, and move the "start scaling" behaviour out of the root command into a first-class `runner run` subcommand (bare `runner` prints help).

**Architecture:** cobra CLI. Root command loses its `RunE`; a new `run` subcommand owns the start flags and the listener loop. Binary, systemd/launchd identifiers, docker shared volume, and self-update assets are renamed to `runner`; the launchd label uses reverse-DNS `io.github.ysya.runner`. The Go module path `github.com/ysya/runscaler` and GitHub repo `ysya/runscaler` are **kept** — only string identifiers change, never import paths.

**Tech Stack:** Go, cobra, viper, goreleaser, systemd/launchd, Docker SDK.

**Spec:** `docs/superpowers/specs/2026-06-16-rename-runscaler-to-runner-design.md`

---

## Prerequisites (do before Task 1)

The working tree has uncommitted work in `cmd/runscaler/cmd_service.go` (modified) and `cmd/runscaler/cmd_service_test.go` (untracked). Task 1 runs `git mv` on the whole `cmd/runscaler` directory, which only moves *tracked* files cleanly.

- [ ] **Confirm a clean starting point**

Run: `git status --short`
Expected: empty output.

If not empty, commit the existing `cmd_service.go` / `cmd_service_test.go` work first (it is in-scope for this rename — `cmd_service.go` and its test are renamed in Task 3 anyway):

```bash
git add cmd/runscaler/cmd_service.go cmd/runscaler/cmd_service_test.go
git commit -m "test(service): add systemd unit template tests"
```

Re-run `git status --short` and confirm it is empty before proceeding.

---

## Task 1: Rename source directory `cmd/runscaler` → `cmd/runner` and fix build paths

**Files:**
- Rename: `cmd/runscaler/` → `cmd/runner/` (all `*.go` files, package stays `main`)
- Modify: `Makefile`
- Modify: `.goreleaser.yaml:3`
- Modify: `Dockerfile`

Moving the `main` package directory is safe: nothing imports it, so no import paths change. Only build paths (`./cmd/runscaler` → `./cmd/runner`) and the binary name must follow.

- [ ] **Step 1: Move the directory with git**

```bash
git mv cmd/runscaler cmd/runner
```

- [ ] **Step 2: Verify it still builds at the new path**

Run: `go build ./cmd/runner`
Expected: builds with no error (produces a `runner` binary in the repo root — delete it: `rm -f runner`).

- [ ] **Step 3: Update the Makefile**

In `Makefile`, change the binary name and every build path:
- `BINARY_NAME := runscaler` → `BINARY_NAME := runner`
- `dev:` target: `go run ./cmd/runscaler --log-level debug` → `go run ./cmd/runner --log-level debug`
- `build:` target: `-o $(BINARY_NAME) ./cmd/runscaler` → `-o $(BINARY_NAME) ./cmd/runner`
- platform build: `-o dist/$(BINARY_NAME)-$(OS)-$(ARCH)$(EXT) ./cmd/runscaler` → `./cmd/runner`

- [ ] **Step 4: Update `.goreleaser.yaml` build path**

In `.goreleaser.yaml`, change `main: ./cmd/runscaler` → `main: ./cmd/runner`. (The archive/binary naming is handled in Task 8.)

- [ ] **Step 5: Update the Dockerfile build path & binary name**

Run: `grep -n runscaler Dockerfile`
For each hit, change the build path `./cmd/runscaler` → `./cmd/runner`, the output binary name `runscaler` → `runner`, and the `ENTRYPOINT`/`CMD` accordingly. Keep any `github.com/ysya/runscaler` module references unchanged if present.

- [ ] **Step 6: Verify build & no stale path references**

Run: `make build && ls -l runner && rm -f runner`
Expected: a `runner` binary is produced.

Run: `grep -rn "cmd/runscaler" Makefile .goreleaser.yaml Dockerfile`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "refactor: rename cmd/runscaler dir to cmd/runner and fix build paths"
```

---

## Task 2: Root command loses `RunE`; add `run` subcommand; migrate start flags

**Files:**
- Modify: `cmd/runner/main.go`
- Test: `cmd/runner/main_test.go` (create)

The start flags currently hang off the root's local `cmd.Flags()`. Moving them is a one-line change (`cmd.Flags()` → `runCmd.Flags()`) because the `viper.BindPFlag` calls use the `flags` local variable. `--config` stays a **persistent** flag on root so `validate`/`status`/`run` all inherit it.

- [ ] **Step 1: Write the failing tests**

Create `cmd/runner/main_test.go`:

```go
package main

import "testing"

func TestRootHasNoRunE(t *testing.T) {
	// Bare `runner` must print help, not start scaling. cobra prints usage
	// for a command with neither Run nor RunE.
	if cmd.RunE != nil || cmd.Run != nil {
		t.Error("root command must not have Run/RunE so bare `runner` prints help")
	}
}

func TestRunSubcommandRegistered(t *testing.T) {
	found := false
	for _, c := range cmd.Commands() {
		if c.Name() == "run" {
			found = true
			break
		}
	}
	if !found {
		t.Error("`run` subcommand must be registered on root")
	}
}

func TestRunOwnsStartFlags(t *testing.T) {
	for _, name := range []string{"url", "name", "token", "max-runners", "backend", "health-port"} {
		if runCmd.Flags().Lookup(name) == nil {
			t.Errorf("`run` must own the --%s start flag", name)
		}
	}
}

func TestRootNoLongerOwnsStartFlags(t *testing.T) {
	if cmd.Flags().Lookup("url") != nil {
		t.Error("root must not own --url anymore; it moved to `run`")
	}
}

func TestConfigStaysPersistentOnRoot(t *testing.T) {
	if cmd.PersistentFlags().Lookup("config") == nil {
		t.Error("--config must remain a persistent flag on root")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/runner/ -run 'TestRoot|TestRun|TestConfigStays' -v`
Expected: FAIL (e.g. `runCmd` undefined / root still has RunE).

- [ ] **Step 3: Add the `run` subcommand and strip the root's RunE**

In `cmd/runner/main.go`, replace the `var cmd = &cobra.Command{...}` block (the one with `Use: "runscaler [flags]"` and the `RunE`) with a root command that has **no** `RunE`, plus a new `runCmd` that carries the old `RunE`:

```go
var cmd = &cobra.Command{
	Use:     "runner",
	Version: version,
	Short:   "GitHub Actions Runner Auto-Scaler",
	Long: `Dynamically scales GitHub Actions self-hosted runners as Docker containers
or Tart VMs using the actions/scaleset library. Runners are ephemeral — each
handles one job and is removed upon completion.

Supports multiple scale sets via [[scaleset]] entries in TOML config,
or a single scale set via CLI flags.`,
	Example: `  # Quick start
  runner init                            # Generate config.toml interactively
  runner validate --config config.toml   # Verify configuration
  runner run --config config.toml        # Start scaling

  # Using CLI flags
  runner run --url https://github.com/org --name my-runners --token ghp_xxx`,
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start scaling (run the listener in the foreground)",
	Long: `Connect to GitHub, create or reuse the runner scale set(s), and listen for
jobs — scaling runners up and down until interrupted.`,
	Example: `  runner run --config config.toml
  runner run --url https://github.com/org --name my-runners --token ghp_xxx
  runner run --dry-run --config config.toml`,
	RunE: func(cmd *cobra.Command, args []string) error {
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
	},
}
```

- [ ] **Step 4: Move start flags onto `run` and register the subcommand**

In `cmd/runner/main.go`'s `init()`:
1. Keep `cmd.PersistentFlags().String("config", ...)` on the root.
2. Change `flags := cmd.Flags()` to `flags := runCmd.Flags()`. (All the `flags.String(...)`/`flags.Int(...)` definitions and every `viper.BindPFlag(..., flags.Lookup(...))` call stay exactly as-is — they reference the `flags` variable.)
3. Add `runCmd` to the registration line:
   `cmd.AddCommand(initCmd, validateCmd, statusCmd, doctorCmd, versionCmd, serviceCmd, runCmd)`

- [ ] **Step 5: Update remaining `runscaler` strings in main.go (except the shared volume — that is Task 4)**

In `cmd/runner/main.go`, change the version-check hint string:
`"A newer version of runscaler is available — run 'runscaler update' to upgrade"` → `"A newer version of runner is available — run 'runner update' to upgrade"`

Do **not** touch the two `runscaler-shared` comments (Task 4) or any `github.com/ysya/runscaler` import.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./cmd/runner/ -run 'TestRoot|TestRun|TestConfigStays' -v`
Expected: PASS.

- [ ] **Step 7: Manually verify CLI behaviour**

Run: `go run ./cmd/runner` → Expected: prints help (usage), does **not** start.
Run: `go run ./cmd/runner run --help` → Expected: shows the start flags (`--url`, `--max-runners`, …) and `--config`.

- [ ] **Step 8: Commit**

```bash
git add cmd/runner/main.go cmd/runner/main_test.go
git commit -m "feat(cli): move start into 'runner run' subcommand; bare runner prints help"
```

---

## Task 3: Rename service identifiers and add `run` to the service templates

**Files:**
- Modify: `cmd/runner/cmd_service.go`
- Modify: `cmd/runner/cmd_service_test.go`

- [ ] **Step 1: Update the failing tests first**

In `cmd/runner/cmd_service_test.go`:
1. In `TestSystemdUnitSystemModeKeepsDockerDependency`, change `ConfigPath` to `/etc/runner/config.toml` and `ReadWritePaths` to `/etc/runner /var/run/docker.sock`, and add an assertion that the unit invokes the `run` subcommand:

```go
	if !strings.Contains(unit, "run --config") {
		t.Errorf("ExecStart must invoke the `run` subcommand:\n%s", unit)
	}
```

2. Add a launchd template test:

```go
func renderLaunchdPlist(t *testing.T, data launchdData) string {
	t.Helper()
	var sb strings.Builder
	if err := launchdTmpl.Execute(&sb, data); err != nil {
		t.Fatalf("render launchd template: %v", err)
	}
	return sb.String()
}

func TestLaunchdPlistInvokesRunSubcommand(t *testing.T) {
	plist := renderLaunchdPlist(t, launchdData{
		Label:      launchdLabel,
		BinaryPath: "/usr/local/bin/runner",
		ConfigPath: "/etc/runner/config.toml",
		LogPath:    "/var/log/runner.log",
	})
	if !strings.Contains(plist, "<string>run</string>") {
		t.Errorf("ProgramArguments must include the `run` subcommand:\n%s", plist)
	}
	if !strings.Contains(plist, "io.github.ysya.runner") {
		t.Errorf("launchd label should be io.github.ysya.runner:\n%s", plist)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/runner/ -run 'TestSystemd|TestLaunchd' -v`
Expected: FAIL.

- [ ] **Step 3: Rename the identifier constants**

In `cmd/runner/cmd_service.go` constants block:

```go
const (
	serviceName        = "runner"
	serviceDescription = "GitHub Actions Runner Auto-Scaler"

	defaultConfigPath = "/etc/runner/config.toml"

	// systemd
	systemdSystemDir = "/etc/systemd/system"
	systemdUnitFile  = "runner.service"

	// launchd
	launchdSystemDir = "/Library/LaunchDaemons"
	launchdLabel     = "io.github.ysya.runner"
	launchdPlistFile = "io.github.ysya.runner.plist"
)
```

- [ ] **Step 4: Add `run` to the systemd template**

In the `systemdTmpl` string, change the ExecStart line:
`ExecStart={{.BinaryPath}} --config {{.ConfigPath}}` → `ExecStart={{.BinaryPath}} run --config {{.ConfigPath}}`

- [ ] **Step 5: Add `run` to the launchd template**

In the `launchdTmpl` ProgramArguments array, insert a `run` string between the binary and `--config`:

```xml
    <key>ProgramArguments</key>
    <array>
        <string>{{.BinaryPath}}</string>
        <string>run</string>
        <string>--config</string>
        <string>{{.ConfigPath}}</string>
    </array>
```

- [ ] **Step 6: Rename the log paths**

In `cmd_service.go`'s `(*launchdManager).logPath`: user path `runscaler.log` → `runner.log`; system path `/var/log/runscaler.log` → `/var/log/runner.log`.

- [ ] **Step 7: Update help strings in this file**

Run: `grep -n runscaler cmd/runner/cmd_service.go`
Change every remaining user-facing `runscaler ...` example/hint to `runner ...` (e.g. `sudo runscaler service install` → `sudo runner service install`, `runscaler service status` → `runner service status`, the "Run 'runscaler service uninstall' first" / "Run 'runscaler init'" hints).

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test ./cmd/runner/ -run 'TestSystemd|TestLaunchd' -v`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add cmd/runner/cmd_service.go cmd/runner/cmd_service_test.go
git commit -m "feat(service): rename identifiers to runner and invoke 'run' subcommand"
```

---

## Task 4: Rename docker shared volume `runscaler-shared` → `runner-shared`

**Files:**
- Modify: `internal/backend/docker.go`
- Modify: `internal/backend/docker_test.go`
- Modify: `cmd/runner/main.go` (comments only)

- [ ] **Step 1: Update tests first**

In `internal/backend/docker_test.go`, replace every `"runscaler-shared"` literal with `"runner-shared"`. This includes the buildx-reaper guard at line ~551 (`{Name: "runner-shared"}, // must be left untouched`), the mount-source assertions, and the volume-removal assertions.

Run: `grep -n "runscaler-shared" internal/backend/docker_test.go`
Expected (after edit): no output.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/backend/ -v`
Expected: FAIL (production code still mounts `runscaler-shared`).

- [ ] **Step 3: Update the production volume name**

In `internal/backend/docker.go`, replace every `"runscaler-shared"` literal with `"runner-shared"` (mount sources and the `VolumeRemove` call + its debug log).

Run: `grep -n "runscaler-shared" internal/backend/docker.go`
Expected (after edit): no output.

- [ ] **Step 4: Update the comments in main.go**

In `cmd/runner/main.go`, update the two comments mentioning `runscaler-shared` to `runner-shared`.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/backend/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/backend/docker.go internal/backend/docker_test.go cmd/runner/main.go
git commit -m "feat(docker): rename shared volume to runner-shared"
```

---

## Task 5: `doctor` cleans both `runner-shared` and the legacy `runscaler-shared`

**Files:**
- Modify: `cmd/runner/cmd_doctor.go`
- Test: `cmd/runner/cmd_doctor_test.go` (create)

After the rename an old `runscaler-shared` volume becomes an orphan. `doctor` must detect and clean it alongside the current `runner-shared`. Refactor `checkDockerVolume` to take a tiny interface so it is unit-testable.

- [ ] **Step 1: Write the failing test**

Create `cmd/runner/cmd_doctor_test.go`:

```go
package main

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/volume"
)

type fakeVolumeAPI struct {
	existing map[string]bool
	removed  []string
}

func (f *fakeVolumeAPI) VolumeInspect(_ context.Context, id string) (volume.Volume, error) {
	if f.existing[id] {
		return volume.Volume{Name: id}, nil
	}
	// checkDockerVolume treats any non-nil error as "volume absent", so the
	// exact error type does not matter here.
	return volume.Volume{}, errors.New("not found")
}

func (f *fakeVolumeAPI) VolumeRemove(_ context.Context, id string, _ bool) error {
	f.removed = append(f.removed, id)
	return nil
}

func TestCheckDockerVolumeRemovesCurrentAndLegacy(t *testing.T) {
	f := &fakeVolumeAPI{existing: map[string]bool{
		"runner-shared":    true,
		"runscaler-shared": true,
	}}
	issues, err := checkDockerVolume(context.Background(), f, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != 0 {
		t.Errorf("issues = %d, want 0 after fixing", issues)
	}
	if len(f.removed) != 2 {
		t.Fatalf("expected both volumes removed, got %v", f.removed)
	}
}

func TestCheckDockerVolumeNoneFound(t *testing.T) {
	f := &fakeVolumeAPI{existing: map[string]bool{}}
	issues, err := checkDockerVolume(context.Background(), f, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != 0 {
		t.Errorf("issues = %d, want 0 when no volumes exist", issues)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/runner/ -run TestCheckDockerVolume -v`
Expected: FAIL (signature mismatch — `checkDockerVolume` takes `*dockerclient.Client`).

- [ ] **Step 3: Refactor `checkDockerVolume` to the interface + multi-name loop**

In `cmd/runner/cmd_doctor.go`, replace the `checkDockerVolume` function (and add the interface + names list above it):

```go
// volumeAPI is the subset of the Docker client doctor needs (so it can be faked in tests).
type volumeAPI interface {
	VolumeInspect(ctx context.Context, id string) (volume.Volume, error)
	VolumeRemove(ctx context.Context, id string, force bool) error
}

// sharedVolumeNames lists shared volumes doctor treats as orphaned: the current
// name plus legacy names from before the runscaler→runner rename.
var sharedVolumeNames = []string{"runner-shared", "runscaler-shared"}

// checkDockerVolume reports/cleans orphaned shared volumes. Returns the number
// of unresolved issues.
func checkDockerVolume(ctx context.Context, client volumeAPI, fix bool) (int, error) {
	found, issues := 0, 0
	for _, name := range sharedVolumeNames {
		if _, err := client.VolumeInspect(ctx, name); err != nil {
			continue // not found (or unknown error treated as absent)
		}
		found++
		if !fix {
			fmt.Printf("  ⚠ Found orphaned volume: %s\n", name)
			issues++
			continue
		}
		if err := client.VolumeRemove(ctx, name, true); err != nil {
			fmt.Printf("  ✗ Failed to remove volume %s: %s\n", name, err)
			issues++
			continue
		}
		fmt.Printf("  ✓ Removed orphaned volume: %s\n", name)
	}
	if found == 0 {
		fmt.Println("  ✓ No orphaned Docker volumes")
	}
	return issues, nil
}
```

Ensure `github.com/docker/docker/api/types/volume` is imported. If `cerrdefs` is now unused in this file, drop its import (the fake test references it, not the production code).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/runner/ -run TestCheckDockerVolume -v`
Expected: PASS.

- [ ] **Step 5: Verify the caller still compiles & update the doctor comment**

The caller passes `*dockerclient.Client`, which satisfies `volumeAPI` — no change needed. Update the function's old doc comment if it still says "runscaler-shared".

Run: `go build ./cmd/runner`
Expected: builds clean. (Then `rm -f runner`.)

- [ ] **Step 6: Commit**

```bash
git add cmd/runner/cmd_doctor.go cmd/runner/cmd_doctor_test.go
git commit -m "feat(doctor): clean both runner-shared and legacy runscaler-shared volumes"
```

---

## Task 6: self-update fetches `runner-*` assets

**Files:**
- Modify: `internal/versioncheck/update.go`
- Test: `internal/versioncheck/update_test.go` (create)

The GitHub repo (`githubRepo = "ysya/runscaler"`) is **kept** — only the asset/binary names change.

- [ ] **Step 1: Write the failing test**

Create `internal/versioncheck/update_test.go`:

```go
package versioncheck

import "testing"

func TestDownloadURLUsesRunnerAsset(t *testing.T) {
	got := DownloadURL("v1.2.3", "linux", "amd64")
	want := "https://github.com/ysya/runscaler/releases/download/v1.2.3/runner-linux-amd64.tar.gz"
	if got != want {
		t.Errorf("DownloadURL = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/versioncheck/ -run TestDownloadURLUsesRunnerAsset -v`
Expected: FAIL (URL still contains `runscaler-`).

- [ ] **Step 3: Update asset and binary names in update.go**

In `internal/versioncheck/update.go`:
- `DownloadURL`: `"…/%s/runscaler-%s-%s.tar.gz"` → `"…/%s/runner-%s-%s.tar.gz"`
- `Update`: `archiveName := fmt.Sprintf("runscaler-%s-%s.tar.gz", …)` → `"runner-%s-%s.tar.gz"`
- `Update`: temp dir prefix `"runscaler-update-*"` → `"runner-update-*"`; temp file prefix `".runscaler-update-*"` → `".runner-update-*"`
- `Update`: `binaryPath := filepath.Join(tmpDir, "runscaler")` → `"runner"`, and `extractBinary(archivePath, "runscaler", binaryPath)` → `"runner"`

Leave `githubRepo` and the `https://github.com/%s/...` host template unchanged.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/versioncheck/ -v`
Expected: PASS (existing tests + the new one).

- [ ] **Step 5: Commit**

```bash
git add internal/versioncheck/update.go internal/versioncheck/update_test.go
git commit -m "feat(update): fetch runner-* release assets"
```

---

## Task 7: Rename remaining user-facing strings in Go code

**Files:**
- Modify: `cmd/runner/cmd_init.go`, `cmd_status.go`, `cmd_update.go`, `cmd_version.go`, `cmd_validate.go`, `loadconfig.go`
- Modify: `internal/config/config.go`, `internal/scaler/scaler.go`, `internal/health/health.go`, `internal/health/health_test.go`, `internal/backend/tart.go`, `internal/versioncheck/check_test.go`

Rename user-facing strings only. **Never** change `github.com/ysya/runscaler` import paths, the `go.mod` module line, or `githubRepo = "ysya/runscaler"` in `check.go`.

- [ ] **Step 1: Find every remaining occurrence**

Run: `grep -rn runscaler cmd/runner/ internal/ | grep -v "github.com/ysya/runscaler"`
This lists all strings to change (cobra `Use`/`Short`/`Long`/`Example`, the `runscaler %s` line in `cmd_version.go:58`, the `/etc/runscaler` config search path in `loadconfig.go:25`, help hints, and test-asserted strings).

- [ ] **Step 2: Apply the renames**

For each hit from Step 1:
- `runscaler` in command examples/help/output → `runner`
- start examples that imply launching (e.g. `runscaler --config x`) → `runner run --config x`
- `loadconfig.go`: `viper.AddConfigPath("/etc/runscaler")` → `"/etc/runner"`
- `cmd_version.go`: `fmt.Fprintf(... "runscaler %s\n", ...)` → `"runner %s\n"`
- in `*_test.go`, update any asserted literal that expects `runscaler` to expect `runner`

- [ ] **Step 3: Verify nothing user-facing remains (import paths excluded)**

Run: `grep -rn runscaler cmd/runner/ internal/ | grep -v "github.com/ysya/runscaler"`
Expected: no output.

- [ ] **Step 4: Build, vet, and test everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "refactor: rename remaining user-facing runscaler strings to runner"
```

---

## Task 8: goreleaser dual assets (`runner-*` + transitional `runscaler-*`)

**Files:**
- Modify: `.goreleaser.yaml`

Produce both archive names so an old `runscaler update` can still fetch the final release; the new binary is shipped under both names.

- [ ] **Step 1: Rewrite `.goreleaser.yaml`**

```yaml
version: 2
builds:
  - id: runner
    main: ./cmd/runner
    binary: runner
    ldflags:
      - -s -w
      - -X main.version={{.Version}}
      - -X main.commit={{.ShortCommit}}
      - -X main.date={{.Date}}
    goos: [linux, darwin]
    goarch: [amd64, arm64]
  - id: runscaler-compat
    main: ./cmd/runner
    binary: runscaler
    ldflags:
      - -s -w
      - -X main.version={{.Version}}
      - -X main.commit={{.ShortCommit}}
      - -X main.date={{.Date}}
    goos: [linux, darwin]
    goarch: [amd64, arm64]

archives:
  - id: runner
    builds: [runner]
    name_template: "runner-{{ .Os }}-{{ .Arch }}"
  - id: runscaler-compat
    builds: [runscaler-compat]
    name_template: "runscaler-{{ .Os }}-{{ .Arch }}"

checksum:
  name_template: "checksums.txt"
```

- [ ] **Step 2: Validate the goreleaser config**

Run: `goreleaser check`
Expected: `config is valid` (if `goreleaser` is installed; otherwise note to verify in CI).

- [ ] **Step 3: Commit**

```bash
git add .goreleaser.yaml
git commit -m "build(release): emit runner-* and transitional runscaler-* assets"
```

---

## Task 9: Documentation and install script

**Files:**
- Modify: `README.md`, `config.example.toml`, `install.sh`, `.gitignore`, `images/actions-runner/README.md`

- [ ] **Step 1: List doc occurrences**

Run: `grep -rn runscaler README.md config.example.toml install.sh .gitignore images/actions-runner/README.md`

- [ ] **Step 2: Apply renames, preserving repo URLs**

For each hit:
- commands and paths (`runscaler …`, `/etc/runscaler`, `/var/log/runscaler.log`, the systemd `ExecStart=…runscaler --config …` at `README.md:351` → `…runner run --config …`, unit name `runscaler.service` → `runner.service`) → rename to `runner`
- launchd label references → `io.github.ysya.runner`
- `.gitignore` build-artifact entry `runscaler` → `runner`
- **Keep** any `github.com/ysya/runscaler` clone/module URL unchanged

- [ ] **Step 3: Add a migration note to README**

Add a short "Upgrading from runscaler" subsection covering: service must be reinstalled (`runscaler service uninstall` with the old binary → install the new `runner` binary → `runner service install`), config moves `/etc/runscaler` → `/etc/runner` (or pass `--config`), and `runner doctor` cleans the old `runscaler-shared` volume.

- [ ] **Step 4: Verify only repo URLs remain**

Run: `grep -rn runscaler README.md config.example.toml install.sh .gitignore images/actions-runner/README.md | grep -v "github.com/ysya/runscaler"`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add README.md config.example.toml install.sh .gitignore images/actions-runner/README.md
git commit -m "docs: rename runscaler to runner across docs and install script"
```

---

## Task 10: CI workflows

**Files:**
- Modify: `.github/workflows/ci.yml`, `.github/workflows/release.yml`

- [ ] **Step 1: List occurrences**

Run: `grep -rn "runscaler\|cmd/runscaler" .github/workflows/`

- [ ] **Step 2: Apply renames**

Change build paths `./cmd/runscaler` → `./cmd/runner`, any binary-name references `runscaler` → `runner`, and artifact names if hard-coded. Keep `github.com/ysya/runscaler` module references unchanged.

- [ ] **Step 3: Verify**

Run: `grep -rn "cmd/runscaler" .github/workflows/`
Expected: no output.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/
git commit -m "ci: update build paths and binary name to runner"
```

---

## Task 11: Final full-repo verification

- [ ] **Step 1: No stray user-facing `runscaler` (import paths & repo URLs excluded)**

Run: `grep -rn runscaler . --exclude-dir=.git | grep -v "github.com/ysya/runscaler" | grep -v "docs/superpowers/"`
Expected: only intentional transitional references remain — the `runscaler-compat` build id / `runscaler-*` archive template in `.goreleaser.yaml`, the legacy `runscaler-shared` name in `doctor`/its test, and the README "Upgrading from runscaler" note. Confirm each remaining hit is one of these; anything else must be renamed.

- [ ] **Step 2: Build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass.

- [ ] **Step 3: Smoke-test the CLI surface**

Run: `make build`
Run: `./runner` → prints help, does not start.
Run: `./runner version` → prints `runner <version>`.
Run: `./runner run --help` → shows start flags.
Run: `./runner doctor --help` → present.
Then: `rm -f runner`

- [ ] **Step 4: Final commit (if Step 3 produced any tweaks)**

```bash
git add -A
git commit -m "chore: final verification tweaks for runner rename"
```

---

## Spec Coverage Check

- binary + identifiers → `runner`; module path/repo kept → Tasks 1, 7 (+ `check.go` untouched)
- `runner run` start + bare `runner` prints help → Task 2
- service templates invoke `run`; identifiers renamed → Task 3
- launchd label `io.github.ysya.runner` → Task 3
- shared volume → `runner-shared` → Task 4
- doctor cleans legacy `runscaler-shared` → Task 5
- self-update fetches `runner-*` → Task 6
- goreleaser dual assets → Task 8
- docs/migration, install, CI → Tasks 9, 10
- full verification → Task 11
