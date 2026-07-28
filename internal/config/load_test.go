package config

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// loadTOML parses the TOML source through a fresh viper instance and Load,
// mirroring the production path in cmd/runner.
func loadTOML(t *testing.T, src string) Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(strings.NewReader(src)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	cfg, err := Load(v)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// resolveTOML is loadTOML + ResolveScaleSets, with the token env fallbacks
// neutralized so an ambient RUNNER_TOKEN cannot leak into assertions.
func resolveTOML(t *testing.T, src string) []ScaleSetConfig {
	t.Helper()
	t.Setenv("RUNNER_TOKEN", "")
	t.Setenv("RUNSCALER_TOKEN", "")
	cfg := loadTOML(t, src)
	return cfg.ResolveScaleSets()
}

func TestLoad_MultiModeInheritance(t *testing.T) {
	cfg := loadTOML(t, `
runner-image = "default-image:latest"
runner-group = "default"
max-runners = 5

[docker]
socket = "/var/run/docker.sock"
dind = true
shared-volume = "/shared"

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"

[[scaleset]]
url = "https://github.com/org-b"
name = "runners-b"
token = "token-b"
runner-image = "custom-image:latest"
max-runners = 20

[scaleset.docker]
socket = "/run/podman/podman.sock"
dind = false
shared-volume = "/data"
`)
	if len(cfg.Warnings) != 0 {
		t.Errorf("Warnings = %q, want none", cfg.Warnings)
	}

	sets := cfg.ResolveScaleSets()
	if len(sets) != 2 {
		t.Fatalf("expected 2 scale sets, got %d", len(sets))
	}

	// First should inherit all defaults
	if sets[0].RunnerImage != "default-image:latest" {
		t.Errorf("sets[0].RunnerImage = %q, want default", sets[0].RunnerImage)
	}
	if sets[0].MaxRunners != 5 {
		t.Errorf("sets[0].MaxRunners = %d, want 5 (inherited)", sets[0].MaxRunners)
	}
	if sets[0].RunnerGroup != "default" {
		t.Errorf("sets[0].RunnerGroup = %q, want default (inherited)", sets[0].RunnerGroup)
	}
	if sets[0].Docker.SharedVolume != "/shared" {
		t.Errorf("sets[0].Docker.SharedVolume = %q, want /shared (inherited)", sets[0].Docker.SharedVolume)
	}
	if sets[0].Docker.Socket != DefaultDockerSocket {
		t.Errorf("sets[0].Docker.Socket = %q, want %s (inherited)", sets[0].Docker.Socket, DefaultDockerSocket)
	}
	if !sets[0].IsDinD() {
		t.Error("sets[0].IsDinD() = false, want true (inherited)")
	}

	// Second should keep its own values
	if sets[1].RunnerImage != "custom-image:latest" {
		t.Errorf("sets[1].RunnerImage = %q, want custom", sets[1].RunnerImage)
	}
	if sets[1].MaxRunners != 20 {
		t.Errorf("sets[1].MaxRunners = %d, want 20", sets[1].MaxRunners)
	}
	if sets[1].Docker.SharedVolume != "/data" {
		t.Errorf("sets[1].Docker.SharedVolume = %q, want /data (per-scaleset override)", sets[1].Docker.SharedVolume)
	}
	if sets[1].Docker.Socket != "/run/podman/podman.sock" {
		t.Errorf("sets[1].Docker.Socket = %q, want podman socket (per-scaleset override)", sets[1].Docker.Socket)
	}
	if sets[1].IsDinD() {
		t.Error("sets[1].IsDinD() = true, want false (per-scaleset override)")
	}
}

// TestLoad_ZeroValueOverrides is the point of map-level merging: an explicit
// zero value in a [[scaleset]] entry must override a non-zero default, which
// the old struct-level merge could not distinguish from "unset".
func TestLoad_ZeroValueOverrides(t *testing.T) {
	t.Run("memory and cpu", func(t *testing.T) {
		sets := resolveTOML(t, `
[docker]
memory = 8192
cpu = 4

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"

[scaleset.docker]
memory = 0

[[scaleset]]
url = "https://github.com/org-b"
name = "runners-b"
token = "token-b"
`)
		if sets[0].Docker.Memory != 0 {
			t.Errorf("sets[0].Docker.Memory = %d, want 0 (explicit override)", sets[0].Docker.Memory)
		}
		if sets[0].Docker.CPU != 4 {
			t.Errorf("sets[0].Docker.CPU = %d, want 4 (inherited)", sets[0].Docker.CPU)
		}
		if sets[1].Docker.Memory != 8192 {
			t.Errorf("sets[1].Docker.Memory = %d, want 8192 (inherited)", sets[1].Docker.Memory)
		}
	})

	t.Run("shared-volume empty string", func(t *testing.T) {
		sets := resolveTOML(t, `
[docker]
shared-volume = "/shared"

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"

[scaleset.docker]
shared-volume = ""
`)
		if sets[0].Docker.SharedVolume != "" {
			t.Errorf("sets[0].Docker.SharedVolume = %q, want \"\" (explicit override)", sets[0].Docker.SharedVolume)
		}
	})

	t.Run("dind false overrides default true", func(t *testing.T) {
		sets := resolveTOML(t, `
[docker]
dind = true

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"

[scaleset.docker]
dind = false
`)
		if sets[0].IsDinD() {
			t.Error("sets[0].IsDinD() = true, want false (explicit override)")
		}
	})

	t.Run("dind true overrides default false", func(t *testing.T) {
		sets := resolveTOML(t, `
[docker]
dind = false

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"

[scaleset.docker]
dind = true

[[scaleset]]
url = "https://github.com/org-b"
name = "runners-b"
token = "token-b"
`)
		if !sets[0].IsDinD() {
			t.Error("sets[0].IsDinD() = false, want true (explicit override)")
		}
		if sets[1].IsDinD() {
			t.Error("sets[1].IsDinD() = true, want false (inherited)")
		}
	})
}

func TestLoad_DurationInheritanceAndOverride(t *testing.T) {
	sets := resolveTOML(t, `
[docker]
shared-volume = "/shared"
shared-volume-ttl = "168h"
shared-volume-cleanup-interval = "30m"

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"

[[scaleset]]
url = "https://github.com/org-b"
name = "runners-b"
token = "token-b"

[scaleset.docker]
shared-volume-ttl = "24h"
`)
	// First inherits the whole docker table, durations included.
	if got := sets[0].Docker.SharedVolumeTTL; got != 168*time.Hour {
		t.Errorf("sets[0] TTL = %v, want 168h (inherited)", got)
	}
	if got := sets[0].Docker.SharedVolumeCleanupInterval; got != 30*time.Minute {
		t.Errorf("sets[0] interval = %v, want 30m (inherited)", got)
	}
	// Second overrides the TTL (duration string parsed inside the
	// scaleset docker table) and inherits the rest.
	if got := sets[1].Docker.SharedVolumeTTL; got != 24*time.Hour {
		t.Errorf("sets[1] TTL = %v, want 24h (override)", got)
	}
	if got := sets[1].Docker.SharedVolumeCleanupInterval; got != 30*time.Minute {
		t.Errorf("sets[1] interval = %v, want 30m (inherited)", got)
	}
	if got := sets[1].Docker.SharedVolume; got != "/shared" {
		t.Errorf("sets[1] shared-volume = %q, want /shared (inherited)", got)
	}
}

// TestLoad_DockerPruneInheritanceAndOverride pins that brand-new [docker]
// keys flow through the map-level merge with zero per-key code: they inherit
// from the top-level table into [[scaleset]] entries, and explicit zero/false
// values in an entry override non-zero defaults.
func TestLoad_DockerPruneInheritanceAndOverride(t *testing.T) {
	sets := resolveTOML(t, `
[docker]
prune = true
prune-interval = "2h"
prune-ttl = "48h"
build-cache-max-age = "72h"
build-cache-budget = 50

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"

[scaleset.docker]
prune = false
build-cache-budget = 0

[[scaleset]]
url = "https://github.com/org-b"
name = "runners-b"
token = "token-b"
`)
	// First entry: explicit false/zero win over the non-zero defaults...
	if sets[0].IsDockerPruneEnabled() {
		t.Error("sets[0] prune should be explicitly disabled")
	}
	if sets[0].Docker.BuildCacheBudgetGB != 0 {
		t.Errorf("sets[0] budget = %d, want 0 (explicit override)", sets[0].Docker.BuildCacheBudgetGB)
	}
	// ...while untouched keys still inherit.
	if sets[0].Docker.PruneTTL != 48*time.Hour {
		t.Errorf("sets[0] prune-ttl = %v, want 48h (inherited)", sets[0].Docker.PruneTTL)
	}
	// Second entry inherits the whole table.
	if !sets[1].IsDockerPruneEnabled() {
		t.Error("sets[1] prune should inherit enabled")
	}
	if sets[1].Docker.PruneInterval != 2*time.Hour {
		t.Errorf("sets[1] prune-interval = %v, want 2h (inherited)", sets[1].Docker.PruneInterval)
	}
	if sets[1].Docker.PruneTTL != 48*time.Hour {
		t.Errorf("sets[1] prune-ttl = %v, want 48h (inherited)", sets[1].Docker.PruneTTL)
	}
	if sets[1].Docker.BuildCacheMaxAge != 72*time.Hour {
		t.Errorf("sets[1] build-cache-max-age = %v, want 72h (inherited)", sets[1].Docker.BuildCacheMaxAge)
	}
	if sets[1].Docker.BuildCacheBudgetGB != 50 {
		t.Errorf("sets[1] build-cache-budget = %d, want 50 (inherited)", sets[1].Docker.BuildCacheBudgetGB)
	}
}

// TestLoad_DockerContainerKeysInheritanceAndOverride pins that the container
// isolation keys (network, pids-limit, shared-volume-name, cache-volumes)
// flow through the map-level merge with zero per-key code: they inherit from
// the top-level table into [[scaleset]] entries, and explicit empty values in
// an entry override non-empty defaults.
func TestLoad_DockerContainerKeysInheritanceAndOverride(t *testing.T) {
	sets := resolveTOML(t, `
[docker]
network = "runners"
pids-limit = 4096
shared-volume-name = "team-shared"
cache-volumes = ["gradle-cache:/home/runner/.gradle", "pnpm-store:/home/runner/.local/share/pnpm/store"]

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"

[scaleset.docker]
network = ""
cache-volumes = []

[[scaleset]]
url = "https://github.com/org-b"
name = "runners-b"
token = "token-b"
`)
	// First entry: explicit empty values win over the non-empty defaults...
	if sets[0].Docker.Network != "" {
		t.Errorf("sets[0] network = %q, want \"\" (explicit override)", sets[0].Docker.Network)
	}
	if len(sets[0].Docker.CacheVolumes) != 0 {
		t.Errorf("sets[0] cache-volumes = %q, want none (explicit override)", sets[0].Docker.CacheVolumes)
	}
	// ...while untouched keys still inherit.
	if sets[0].Docker.PidsLimit != 4096 {
		t.Errorf("sets[0] pids-limit = %d, want 4096 (inherited)", sets[0].Docker.PidsLimit)
	}
	if got := sets[0].SharedVolumeName(); got != "team-shared" {
		t.Errorf("sets[0] shared-volume-name = %q, want team-shared (inherited)", got)
	}
	// Second entry inherits the whole table.
	if sets[1].Docker.Network != "runners" {
		t.Errorf("sets[1] network = %q, want runners (inherited)", sets[1].Docker.Network)
	}
	if sets[1].Docker.PidsLimit != 4096 {
		t.Errorf("sets[1] pids-limit = %d, want 4096 (inherited)", sets[1].Docker.PidsLimit)
	}
	if got := sets[1].SharedVolumeName(); got != "team-shared" {
		t.Errorf("sets[1] shared-volume-name = %q, want team-shared (inherited)", got)
	}
	wantCaches := []string{"gradle-cache:/home/runner/.gradle", "pnpm-store:/home/runner/.local/share/pnpm/store"}
	if len(sets[1].Docker.CacheVolumes) != len(wantCaches) {
		t.Fatalf("sets[1] cache-volumes = %q, want %q (inherited)", sets[1].Docker.CacheVolumes, wantCaches)
	}
	for i, want := range wantCaches {
		if sets[1].Docker.CacheVolumes[i] != want {
			t.Errorf("sets[1] cache-volumes[%d] = %q, want %q (inherited)", i, sets[1].Docker.CacheVolumes[i], want)
		}
	}
}

func TestLoad_IdentityKeysNotInherited(t *testing.T) {
	t.Setenv("RUNNER_TOKEN", "")
	t.Setenv("RUNSCALER_TOKEN", "")
	cfg := loadTOML(t, `
url = "https://github.com/top"
name = "top-runners"
token = "top-token"
labels = ["top-label"]
min-runners = 3

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"
`)

	sets := cfg.ResolveScaleSets()
	if len(sets) != 1 {
		t.Fatalf("expected 1 scale set, got %d", len(sets))
	}
	if sets[0].RegistrationURL != "https://github.com/org-a" {
		t.Errorf("url = %q, want the scaleset's own", sets[0].RegistrationURL)
	}
	if sets[0].ScaleSetName != "runners-a" {
		t.Errorf("name = %q, want the scaleset's own", sets[0].ScaleSetName)
	}
	if sets[0].Token != "token-a" {
		t.Errorf("token = %q, want the scaleset's own", sets[0].Token)
	}
	if len(sets[0].Labels) != 0 {
		t.Errorf("labels = %q, want none (never inherited)", sets[0].Labels)
	}
	if sets[0].MinRunners != 0 {
		t.Errorf("min-runners = %d, want 0 (never inherited)", sets[0].MinRunners)
	}

	if len(cfg.Warnings) != 1 {
		t.Fatalf("Warnings = %q, want exactly the mixed-mode warning", cfg.Warnings)
	}
	want := "top-level url/name/token/labels/min-runners are ignored when [[scaleset]] entries exist — move them into a [[scaleset]]"
	if cfg.Warnings[0] != want {
		t.Errorf("warning = %q, want %q", cfg.Warnings[0], want)
	}
}

func TestLoad_MixedModeWarningListsOnlySetKeys(t *testing.T) {
	cfg := loadTOML(t, `
url = "https://github.com/top"

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"
`)
	if len(cfg.Warnings) != 1 {
		t.Fatalf("Warnings = %q, want 1", cfg.Warnings)
	}
	if !strings.Contains(cfg.Warnings[0], "top-level url are ignored") {
		t.Errorf("warning = %q, want it to list only url", cfg.Warnings[0])
	}
	if strings.Contains(cfg.Warnings[0], "name") || strings.Contains(cfg.Warnings[0], "token") {
		t.Errorf("warning = %q, must not list unset keys", cfg.Warnings[0])
	}
}

func TestLoad_MinRunnersInScaleset(t *testing.T) {
	sets := resolveTOML(t, `
[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"
min-runners = 3
max-runners = 5
`)
	if sets[0].MinRunners != 3 {
		t.Errorf("min-runners = %d, want 3", sets[0].MinRunners)
	}
	if sets[0].MaxRunners != 5 {
		t.Errorf("max-runners = %d, want 5", sets[0].MaxRunners)
	}
}

func TestLoad_UnknownKeys(t *testing.T) {
	cfg := loadTOML(t, `
typo-key = 1

[docker]
foo = "bar"

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"
badkey = 2

[scaleset.docker]
nested-bad = 3
`)
	want := []string{
		`unknown config key "docker.foo" — ignored (check for typos; run 'runner validate')`,
		`unknown config key "typo-key" — ignored (check for typos; run 'runner validate')`,
		`scaleset[0]: unknown config key "badkey" — ignored (check for typos; run 'runner validate')`,
		`scaleset[0]: unknown config key "docker.nested-bad" — ignored (check for typos; run 'runner validate')`,
	}
	if len(cfg.Warnings) != len(want) {
		t.Fatalf("Warnings = %q, want %d entries", cfg.Warnings, len(want))
	}
	for i, w := range want {
		if cfg.Warnings[i] != w {
			t.Errorf("Warnings[%d] = %q, want %q", i, cfg.Warnings[i], w)
		}
	}
}

// TestLoad_ValidConfigNoWarnings pins that a fully populated valid config
// produces zero warnings — guarding against Metadata.Unused false positives
// (e.g. squashed structs or nested tables being misreported).
func TestLoad_ValidConfigNoWarnings(t *testing.T) {
	cfg := loadTOML(t, `
log-level = "debug"
log-format = "json"
health-port = 9090
dry-run = false

backend = "docker"
runner-image = "ghcr.io/actions/actions-runner:latest"
runner-group = "default"
max-runners = 10

[docker]
socket = "/var/run/docker.sock"
dind = true
shared-volume = "/shared"
shared-volume-name = "runner-shared"
memory = 8192
cpu = 4
pids-limit = 4096
platform = "linux/amd64"
network = "runners"
cache-volumes = ["gradle-cache:/home/runner/.gradle", "pnpm-store:/home/runner/.local/share/pnpm/store"]
shared-volume-ttl = "168h"
shared-volume-cleanup-interval = "6h"
buildx-cleanup = true
buildx-cleanup-ttl = "24h"
buildx-cleanup-interval = "6h"
prune = true
prune-interval = "6h"
prune-ttl = "24h"
build-cache-max-age = "168h"
build-cache-budget = 50

[tart]
home = "/Volumes/tart"
runner-dir = "/Users/admin/actions-runner"
cpu = 4
memory = 8192
pool-size = 1
cache-cleanup = true
cache-max-age = "168h"
cache-space-budget = 80
cache-cleanup-interval = "24h"

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"
labels = ["linux", "x64"]
min-runners = 1
max-runners = 5

[scaleset.docker]
memory = 0

[[scaleset]]
url = "https://github.com/org-b"
name = "macos-runners"
token = "token-b"
backend = "tart"
runner-image = "ghcr.io/cirruslabs/macos-sequoia-xcode:latest"

[scaleset.tart]
cpu = 8
`)
	if len(cfg.Warnings) != 0 {
		t.Errorf("valid config produced warnings: %q", cfg.Warnings)
	}
	if len(cfg.ScaleSets) != 2 {
		t.Fatalf("expected 2 scale sets, got %d", len(cfg.ScaleSets))
	}
}

// TestLoad_SingleMode covers the no-[[scaleset]] path, including duration
// decoding of the top-level [docker] table and env: token resolution.
func TestLoad_SingleMode(t *testing.T) {
	t.Setenv("MY_CUSTOM_TOKEN", "resolved-token")
	cfg := loadTOML(t, `
url = "https://github.com/test-org"
name = "single-runners"
token = "env:MY_CUSTOM_TOKEN"
max-runners = 3

[docker]
shared-volume = "/shared"
shared-volume-ttl = "168h"
shared-volume-cleanup-interval = "30m"
`)
	if len(cfg.Warnings) != 0 {
		t.Errorf("Warnings = %q, want none", cfg.Warnings)
	}
	if got := cfg.Defaults.Docker.SharedVolumeTTL; got != 168*time.Hour {
		t.Errorf("SharedVolumeTTL = %v, want 168h", got)
	}
	if got := cfg.Defaults.Docker.SharedVolumeCleanupInterval; got != 30*time.Minute {
		t.Errorf("SharedVolumeCleanupInterval = %v, want 30m", got)
	}

	sets := cfg.ResolveScaleSets()
	if len(sets) != 1 {
		t.Fatalf("expected 1 scale set, got %d", len(sets))
	}
	if sets[0].ScaleSetName != "single-runners" {
		t.Errorf("name = %q, want single-runners", sets[0].ScaleSetName)
	}
	if sets[0].MaxRunners != 3 {
		t.Errorf("max-runners = %d, want 3", sets[0].MaxRunners)
	}
	if sets[0].Token != "resolved-token" {
		t.Errorf("token = %q, want env-resolved value", sets[0].Token)
	}
	if sets[0].Backend != DefaultBackend {
		t.Errorf("backend = %q, want %q", sets[0].Backend, DefaultBackend)
	}
}

func TestLoad_TartDecodingAndInheritance(t *testing.T) {
	t.Run("single mode decoding", func(t *testing.T) {
		cfg := loadTOML(t, `
[tart]
home = "/Volumes/Data/tart"
cache-cleanup = false
cache-max-age = "168h"
cache-space-budget = 80
cache-cleanup-interval = "12h"
`)
		if len(cfg.Warnings) != 0 {
			t.Errorf("Warnings = %q, want none", cfg.Warnings)
		}
		if got := cfg.Defaults.Tart.Home; got != "/Volumes/Data/tart" {
			t.Errorf("Tart.Home = %q, want /Volumes/Data/tart", got)
		}
		if cfg.Defaults.Tart.CacheCleanup == nil || *cfg.Defaults.Tart.CacheCleanup != false {
			t.Errorf("Tart.CacheCleanup = %v, want explicit false", cfg.Defaults.Tart.CacheCleanup)
		}
		if got := cfg.Defaults.Tart.CacheMaxAge; got != 168*time.Hour {
			t.Errorf("Tart.CacheMaxAge = %v, want 168h", got)
		}
		if got := cfg.Defaults.Tart.CacheSpaceBudgetGB; got != 80 {
			t.Errorf("Tart.CacheSpaceBudgetGB = %d, want 80", got)
		}
		if got := cfg.Defaults.Tart.CacheCleanupInterval; got != 12*time.Hour {
			t.Errorf("Tart.CacheCleanupInterval = %v, want 12h", got)
		}
	})

	t.Run("multi mode inheritance and override", func(t *testing.T) {
		sets := resolveTOML(t, `
backend = "tart"
runner-image = "macos-base:latest"

[tart]
cache-cleanup = true
cache-max-age = "168h"
cache-space-budget = 50
cache-cleanup-interval = "24h"

[[scaleset]]
url = "https://github.com/org-a"
name = "macos-a"
token = "token-a"

[scaleset.tart]
cache-cleanup = false
cache-max-age = "48h"
cache-space-budget = 100
cache-cleanup-interval = "6h"

[[scaleset]]
url = "https://github.com/org-b"
name = "macos-b"
token = "token-b"
`)
		// First: per-scaleset values win, including explicit false.
		if sets[0].IsTartCacheCleanupEnabled() {
			t.Error("sets[0] cache cleanup should be explicitly disabled")
		}
		if sets[0].Tart.CacheMaxAge != 48*time.Hour {
			t.Errorf("sets[0] max age = %v, want 48h", sets[0].Tart.CacheMaxAge)
		}
		if sets[0].Tart.CacheSpaceBudgetGB != 100 {
			t.Errorf("sets[0] budget = %d, want 100", sets[0].Tart.CacheSpaceBudgetGB)
		}
		if sets[0].Tart.CacheCleanupInterval != 6*time.Hour {
			t.Errorf("sets[0] interval = %v, want 6h", sets[0].Tart.CacheCleanupInterval)
		}
		// Second: inherits everything from the top-level [tart] table.
		if !sets[1].IsTartCacheCleanupEnabled() {
			t.Error("sets[1] cache cleanup should inherit enabled")
		}
		if sets[1].Tart.CacheMaxAge != 168*time.Hour {
			t.Errorf("sets[1] max age = %v, want 168h", sets[1].Tart.CacheMaxAge)
		}
		if sets[1].Tart.CacheSpaceBudgetGB != 50 {
			t.Errorf("sets[1] budget = %d, want 50", sets[1].Tart.CacheSpaceBudgetGB)
		}
		if sets[1].Tart.CacheCleanupInterval != 24*time.Hour {
			t.Errorf("sets[1] interval = %v, want 24h", sets[1].Tart.CacheCleanupInterval)
		}
	})
}

func TestLoad_MixedBackends(t *testing.T) {
	sets := resolveTOML(t, `
max-runners = 5
runner-image = "global-macos:latest"

[[scaleset]]
url = "https://github.com/org"
name = "linux-runners"
token = "token-a"

[[scaleset]]
url = "https://github.com/org"
name = "macos-runners"
token = "token-b"
backend = "tart"
runner-image = "custom-macos:latest"
`)
	if len(sets) != 2 {
		t.Fatalf("expected 2 scale sets, got %d", len(sets))
	}

	// First: Docker (default backend from builtin defaults)
	if sets[0].IsTart() {
		t.Error("sets[0] should not be tart")
	}
	if sets[0].Backend != DefaultBackend {
		t.Errorf("sets[0].Backend = %q, want %q", sets[0].Backend, DefaultBackend)
	}

	// Second: Tart with custom image and backend-dependent defaults applied
	if !sets[1].IsTart() {
		t.Error("sets[1] should be tart")
	}
	if sets[1].RunnerImage != "custom-macos:latest" {
		t.Errorf("sets[1].RunnerImage = %q, want custom", sets[1].RunnerImage)
	}
	if sets[1].Tart.RunnerDir != DefaultTartRunnerDir {
		t.Errorf("sets[1].Tart.RunnerDir = %q, want %q", sets[1].Tart.RunnerDir, DefaultTartRunnerDir)
	}
}

// TestLoad_BuiltinDefaults pins that scale sets get sane defaults even when
// the viper instance has no CLI flag bindings (as in tests and library use).
func TestLoad_BuiltinDefaults(t *testing.T) {
	sets := resolveTOML(t, `
[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"
`)
	if sets[0].RunnerImage != DefaultRunnerImage {
		t.Errorf("RunnerImage = %q, want %q", sets[0].RunnerImage, DefaultRunnerImage)
	}
	if sets[0].RunnerGroup != DefaultRunnerGroup {
		t.Errorf("RunnerGroup = %q, want %q", sets[0].RunnerGroup, DefaultRunnerGroup)
	}
	if sets[0].MaxRunners != DefaultMaxRunners {
		t.Errorf("MaxRunners = %d, want %d", sets[0].MaxRunners, DefaultMaxRunners)
	}
	if sets[0].Backend != DefaultBackend {
		t.Errorf("Backend = %q, want %q", sets[0].Backend, DefaultBackend)
	}
	if sets[0].Docker.Socket != DefaultDockerSocket {
		t.Errorf("Docker.Socket = %q, want %q", sets[0].Docker.Socket, DefaultDockerSocket)
	}
	if !sets[0].IsDinD() {
		t.Error("IsDinD() = false, want default true")
	}
}

// TestLoad_FlagDefaultsNoMixedWarning pins that the zero-valued defaults CLI
// flag bindings inject (url = "", min-runners = 0, labels = [], ...) do not
// trigger the mixed-mode warning on their own.
func TestLoad_FlagDefaultsNoMixedWarning(t *testing.T) {
	v := viper.New()
	v.SetDefault("url", "")
	v.SetDefault("name", "")
	v.SetDefault("token", "")
	v.SetDefault("labels", []string{})
	v.SetDefault("min-runners", 0)
	v.SetDefault("max-runners", DefaultMaxRunners)
	v.SetConfigType("toml")
	if err := v.ReadConfig(strings.NewReader(`
[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "token-a"
`)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	cfg, err := Load(v)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("Warnings = %q, want none for zero-valued flag defaults", cfg.Warnings)
	}
	if len(cfg.ScaleSets) != 1 {
		t.Fatalf("expected 1 scale set, got %d", len(cfg.ScaleSets))
	}
}

// TestLoad_ScaleSetsViaSet covers the []map[string]any shape viper produces
// for programmatic Set, plus the malformed-value error paths.
func TestLoad_ScaleSetsViaSet(t *testing.T) {
	t.Run("slice of maps", func(t *testing.T) {
		v := viper.New()
		v.Set("scaleset", []map[string]any{
			{"url": "https://github.com/org-a", "name": "runners-a", "token": "token-a"},
		})
		cfg, err := Load(v)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.ScaleSets) != 1 {
			t.Fatalf("expected 1 scale set, got %d", len(cfg.ScaleSets))
		}
		if cfg.ScaleSets[0].ScaleSetName != "runners-a" {
			t.Errorf("name = %q, want runners-a", cfg.ScaleSets[0].ScaleSetName)
		}
		if cfg.ScaleSets[0].MaxRunners != DefaultMaxRunners {
			t.Errorf("MaxRunners = %d, want builtin default %d", cfg.ScaleSets[0].MaxRunners, DefaultMaxRunners)
		}
	})

	t.Run("scalar value errors", func(t *testing.T) {
		v := viper.New()
		v.Set("scaleset", "nope")
		if _, err := Load(v); err == nil {
			t.Error("expected an error for a non-array scaleset value")
		}
	})

	t.Run("non-table element errors", func(t *testing.T) {
		v := viper.New()
		v.Set("scaleset", []any{"nope"})
		if _, err := Load(v); err == nil {
			t.Error("expected an error for a non-table scaleset element")
		}
	})
}
