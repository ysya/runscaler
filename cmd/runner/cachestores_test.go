package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/cachestore"
	"github.com/ysya/runscaler/internal/config"
)

// These tests cover the pure, Docker/Tart-connection-independent selection
// logic behind buildCacheStores: which scaleset's settings win when several
// share a socket or TART_HOME. This is exactly the logic that used to live
// duplicated inside each of the four sweepers before Task 7; a regression
// here would silently change what gets reclaimed without any cachestore or
// diskguard test catching it, since those packages only see the resulting
// config structs, not how sets selects them.

func TestDockerPruneSettingsFor(t *testing.T) {
	// 2026-08-14 revision: Enabled tracks only whether cmd/runner's own
	// periodic sweeper should run — PruneTTL/MaxAge/interval are still
	// populated with real (defaulted) values regardless, because the disk
	// guard reclaims through these stores whether or not any scaleset
	// enabled the periodic sweep (see
	// docs/superpowers/specs/2026-08-13-cache-architecture-design.md,
	// "各 store 的啟用開關只約束例行清理"). A bare zero-value PruneTTL would leave
	// the guard just as unable to reclaim as respecting Enabled() did.
	t.Run("no scaleset enabled yields disabled=false but real defaulted settings", func(t *testing.T) {
		sets := []config.ScaleSetConfig{{ScaleSetName: "a"}}
		garbage, buildCache, interval := dockerPruneSettingsFor(sets)
		if garbage.Enabled || buildCache.Enabled {
			t.Errorf("garbage.Enabled=%v buildCache.Enabled=%v, want both false — the periodic sweeper must still stay off", garbage.Enabled, buildCache.Enabled)
		}
		if garbage.PruneTTL != config.DefaultDockerPruneTTL {
			t.Errorf("PruneTTL = %v, want default %v even though nothing is enabled", garbage.PruneTTL, config.DefaultDockerPruneTTL)
		}
		if buildCache.MaxAge != config.DefaultDockerBuildCacheMaxAge {
			t.Errorf("build cache max age = %v, want default %v even though nothing is enabled", buildCache.MaxAge, config.DefaultDockerBuildCacheMaxAge)
		}
		if interval != config.DefaultDockerPruneInterval {
			t.Errorf("interval = %v, want default %v", interval, config.DefaultDockerPruneInterval)
		}
	})

	t.Run("first enabled scaleset wins, later enabled ones ignored", func(t *testing.T) {
		yes := true
		sets := []config.ScaleSetConfig{
			{ScaleSetName: "skip-disabled"},
			{ScaleSetName: "first", Docker: config.DockerConfig{Prune: &yes, PruneTTL: 12 * time.Hour, PruneInterval: 3 * time.Hour, BuildCacheMaxAge: 5 * time.Hour, BuildCacheBudgetGB: 7}},
			{ScaleSetName: "second", Docker: config.DockerConfig{Prune: &yes, PruneTTL: 99 * time.Hour}},
		}
		garbage, buildCache, interval := dockerPruneSettingsFor(sets)
		if !garbage.Enabled || garbage.PruneTTL != 12*time.Hour {
			t.Errorf("garbage = %+v, want Enabled=true PruneTTL=12h (first scaleset)", garbage)
		}
		if !buildCache.Enabled || buildCache.MaxAge != 5*time.Hour || buildCache.BudgetGB != 7 {
			t.Errorf("buildCache = %+v, want Enabled=true MaxAge=5h BudgetGB=7 (first scaleset)", buildCache)
		}
		if interval != 3*time.Hour {
			t.Errorf("interval = %v, want 3h (first scaleset)", interval)
		}
	})

	t.Run("PruneTTL exactly zero defaults, negative is preserved (disables container/image portion only)", func(t *testing.T) {
		yes := true
		sets := []config.ScaleSetConfig{{Docker: config.DockerConfig{Prune: &yes, PruneTTL: 0}}}
		garbage, _, _ := dockerPruneSettingsFor(sets)
		if garbage.PruneTTL != config.DefaultDockerPruneTTL {
			t.Errorf("PruneTTL = %v, want default %v for an explicit 0", garbage.PruneTTL, config.DefaultDockerPruneTTL)
		}

		sets[0].Docker.PruneTTL = -time.Hour
		garbage, _, _ = dockerPruneSettingsFor(sets)
		if garbage.PruneTTL != -time.Hour {
			t.Errorf("PruneTTL = %v, want -1h preserved (not defaulted)", garbage.PruneTTL)
		}
	})

	t.Run("zero interval and build-cache-max-age default", func(t *testing.T) {
		yes := true
		sets := []config.ScaleSetConfig{{Docker: config.DockerConfig{Prune: &yes}}}
		_, buildCache, interval := dockerPruneSettingsFor(sets)
		if interval != config.DefaultDockerPruneInterval {
			t.Errorf("interval = %v, want default %v", interval, config.DefaultDockerPruneInterval)
		}
		if buildCache.MaxAge != config.DefaultDockerBuildCacheMaxAge {
			t.Errorf("build cache max age = %v, want default %v", buildCache.MaxAge, config.DefaultDockerBuildCacheMaxAge)
		}
	})

	t.Run("tart scalesets are skipped", func(t *testing.T) {
		yes := true
		sets := []config.ScaleSetConfig{{Backend: "tart", Docker: config.DockerConfig{Prune: &yes, PruneTTL: time.Hour}}}
		garbage, _, _ := dockerPruneSettingsFor(sets)
		if garbage.Enabled {
			t.Error("a Tart scaleset must never enable the Docker garbage store")
		}
	})
}

func TestBuildxConfigFor(t *testing.T) {
	// See dockerPruneSettingsFor's identical 2026-08-14 revision note: the
	// periodic sweeper stays off (Enabled=false), but MaxAge/interval still
	// get real defaults so the disk guard has something to reclaim by.
	t.Run("disabled by default but real defaulted settings", func(t *testing.T) {
		cfg, interval := buildxConfigFor([]config.ScaleSetConfig{{}}, "/var/lib/docker")
		if cfg.Enabled {
			t.Errorf("cfg.Enabled = true, want false — the periodic sweeper must stay off (buildx-cleanup defaults off)")
		}
		if cfg.MaxAge != config.DefaultBuildxCleanupTTL {
			t.Errorf("cfg.MaxAge = %v, want default %v even though nothing is enabled", cfg.MaxAge, config.DefaultBuildxCleanupTTL)
		}
		if interval != config.DefaultBuildxCleanupInterval {
			t.Errorf("interval = %v, want default %v even though nothing is enabled", interval, config.DefaultBuildxCleanupInterval)
		}
		if cfg.RootDir != "/var/lib/docker" {
			t.Errorf("RootDir = %q, want passed through even when disabled", cfg.RootDir)
		}
	})

	t.Run("first enabled scaleset wins and non-positive ttl/interval default", func(t *testing.T) {
		yes := true
		sets := []config.ScaleSetConfig{
			{Docker: config.DockerConfig{BuildxCleanup: &yes}}, // ttl/interval unset (0)
			{Docker: config.DockerConfig{BuildxCleanup: &yes, BuildxCleanupTTL: 48 * time.Hour}},
		}
		cfg, interval := buildxConfigFor(sets, "/root")
		if !cfg.Enabled || cfg.MaxAge != config.DefaultBuildxCleanupTTL {
			t.Errorf("cfg = %+v, want Enabled=true MaxAge=default (first scaleset's ttl was unset)", cfg)
		}
		if interval != config.DefaultBuildxCleanupInterval {
			t.Errorf("interval = %v, want default", interval)
		}
	})
}

func TestSharedVolumeSweepTargetsFor(t *testing.T) {
	t.Run("no qualifying scaleset yields no targets", func(t *testing.T) {
		sets := []config.ScaleSetConfig{{}, {Docker: config.DockerConfig{SharedVolume: "/shared"}}} // TTL unset
		targets := sharedVolumeSweepTargetsFor(sets, nil, "/root", slog.New(slog.DiscardHandler))
		if len(targets) != 0 {
			t.Errorf("targets = %+v, want none (no TTL configured)", targets)
		}
	})

	t.Run("dedups by volume name, first wins, distinct names both get a target", func(t *testing.T) {
		sets := []config.ScaleSetConfig{
			{Docker: config.DockerConfig{SharedVolume: "/shared", SharedVolumeMaxAge: 24 * time.Hour}},  // default volume name
			{Docker: config.DockerConfig{SharedVolume: "/shared", SharedVolumeMaxAge: 999 * time.Hour}}, // same default name, ignored
			{Docker: config.DockerConfig{SharedVolume: "/shared", SharedVolumeMaxAge: time.Hour, SharedVolumeName: "team-a"}},
		}
		targets := sharedVolumeSweepTargetsFor(sets, nil, "/root", slog.New(slog.DiscardHandler))
		if len(targets) != 2 {
			t.Fatalf("targets = %+v, want 2 (one per unique volume name)", targets)
		}
		byName := map[string]sharedVolumeSweepTarget{}
		for _, tg := range targets {
			byName[tg.volumeName] = tg
		}
		if got := byName[config.DefaultSharedVolumeName].ttl; got != 24*time.Hour {
			t.Errorf("default-volume ttl = %v, want 24h (first scaleset wins)", got)
		}
		if got := byName["team-a"].ttl; got != time.Hour {
			t.Errorf("team-a ttl = %v, want 1h", got)
		}
		if byName[config.DefaultSharedVolumeName].store == nil || byName["team-a"].store == nil {
			t.Error("every target must carry a constructed store")
		}
	})

	t.Run("interval defaults when unset", func(t *testing.T) {
		sets := []config.ScaleSetConfig{{Docker: config.DockerConfig{SharedVolume: "/shared", SharedVolumeMaxAge: time.Hour}}}
		targets := sharedVolumeSweepTargetsFor(sets, nil, "/root", slog.New(slog.DiscardHandler))
		if len(targets) != 1 || targets[0].interval != config.DefaultSharedVolumeCleanupInterval {
			t.Errorf("targets = %+v, want 1 target with default interval", targets)
		}
	})
}

func TestTartCacheStores(t *testing.T) {
	// 2026-08-14 revision: every configured TART_HOME gets a target now —
	// even one where cache-cleanup is disabled everywhere it's shared —
	// because the disk guard reclaims through it regardless of enabled;
	// only the periodic sweeper (via target.enabled) skips it. See
	// tartCacheStores' doc comment.
	t.Run("disabled scaleset still yields a target, marked not enabled, with defaulted settings", func(t *testing.T) {
		no := false
		sets := []config.ScaleSetConfig{{Backend: "tart", Tart: config.TartConfig{Home: "/Volumes/A", CacheCleanup: &no}}}
		targets := tartCacheStores(sets, slog.New(slog.DiscardHandler))
		if len(targets) != 1 {
			t.Fatalf("targets = %+v, want 1 (the disk guard must still see this home)", targets)
		}
		tg := targets["/Volumes/A"]
		if tg.enabled {
			t.Error("enabled = true, want false — cache-cleanup is disabled, the periodic sweeper must stay off")
		}
		if tg.maxAge != config.DefaultTartCacheMaxAge {
			t.Errorf("maxAge = %v, want default %v so the guard has something real to reclaim by", tg.maxAge, config.DefaultTartCacheMaxAge)
		}
		if tg.store == nil {
			t.Error("target must carry a constructed store even when disabled")
		}
	})

	t.Run("first enabled scaleset per home wins, distinct homes each get a target", func(t *testing.T) {
		no := false
		sets := []config.ScaleSetConfig{
			{Backend: "tart", Tart: config.TartConfig{Home: "/Volumes/A", CacheCleanup: &no}},                // skipped: disabled
			{Backend: "tart", Tart: config.TartConfig{Home: "/Volumes/A", CacheMaxAge: 3 * 24 * time.Hour}},  // enabled by default, wins for /Volumes/A
			{Backend: "tart", Tart: config.TartConfig{Home: "/Volumes/A", CacheMaxAge: 99 * 24 * time.Hour}}, // ignored
			{Backend: "tart", Tart: config.TartConfig{Home: "/Volumes/B", CacheBudgetGB: 50}},
		}
		targets := tartCacheStores(sets, slog.New(slog.DiscardHandler))
		if len(targets) != 2 {
			t.Fatalf("targets = %+v, want 2 (one per unique home)", targets)
		}
		if got := targets["/Volumes/A"]; got.maxAge != 3*24*time.Hour || !got.enabled {
			t.Errorf("/Volumes/A = %+v, want maxAge=3days enabled=true (first enabled scaleset)", got)
		}
		if got := targets["/Volumes/B"]; got.spaceBudgetGB != 50 || !got.enabled {
			t.Errorf("/Volumes/B = %+v, want spaceBudgetGB=50 enabled=true", got)
		}
		if targets["/Volumes/A"].store == nil || targets["/Volumes/B"].store == nil {
			t.Error("every target must carry a constructed store")
		}
	})

	t.Run("non-tart scaleset never contributes a target", func(t *testing.T) {
		sets := []config.ScaleSetConfig{{Backend: "docker", Tart: config.TartConfig{Home: "/Volumes/A"}}}
		if targets := tartCacheStores(sets, slog.New(slog.DiscardHandler)); len(targets) != 0 {
			t.Errorf("targets = %+v, want none for a Docker scaleset", targets)
		}
	})
}

func TestCacheVolumeStoresFor(t *testing.T) {
	t.Run("dedups by volume name across scalesets, first path wins", func(t *testing.T) {
		sets := []config.ScaleSetConfig{
			{RunnerImage: "img-a", Docker: config.DockerConfig{CacheVolumes: []string{"gradle-cache:/home/runner/.gradle"}}},
			{RunnerImage: "img-b", Docker: config.DockerConfig{CacheVolumes: []string{"gradle-cache:/root/.gradle", "pnpm:/home/runner/.pnpm"}}},
		}
		stores := cacheVolumeStoresFor(sets, nil, "/root")
		if len(stores) != 2 {
			t.Fatalf("stores = %d, want 2 (gradle-cache deduped, pnpm added)", len(stores))
		}
		names := map[string]bool{}
		for _, s := range stores {
			names[s.Name()] = true
		}
		if !names["cache-volume:gradle-cache"] || !names["cache-volume:pnpm"] {
			t.Errorf("store names = %v, want cache-volume:gradle-cache and cache-volume:pnpm", names)
		}
	})

	t.Run("tart scalesets never contribute a cache-volume store", func(t *testing.T) {
		sets := []config.ScaleSetConfig{{Backend: "tart"}}
		if stores := cacheVolumeStoresFor(sets, nil, "/root"); len(stores) != 0 {
			t.Errorf("stores = %d, want 0 for a Tart scaleset", len(stores))
		}
	})

	// TestLoad_DockerCacheLongForm (internal/config) already pins that TOML
	// [[docker.cache]] decodes into DockerConfig.Cache; this pins the next
	// link in the chain — that a parsed long-form budget/on-exceed actually
	// reaches the constructed cachestore.CacheStore's Budget(), which is
	// what internal/diskguard.Guard.enforceBudgets reads. Without this,
	// on-exceed = "wipe" would parse and validate cleanly but never fire.
	t.Run("long-form budget and on-exceed reach the constructed store", func(t *testing.T) {
		sets := []config.ScaleSetConfig{
			{RunnerImage: "img-a", Docker: config.DockerConfig{
				CacheVolumes: []string{"gradle-cache:/home/runner/.gradle"},
				Cache: []config.CacheVolumeSpec{
					{Name: "ccache", Path: "/home/runner/.ccache", Budget: "20GB", OnExceed: "wipe"},
				},
			}},
		}
		stores := cacheVolumeStoresFor(sets, nil, "/root")
		if len(stores) != 2 {
			t.Fatalf("stores = %d, want 2", len(stores))
		}
		byName := make(map[string]cachestore.CacheStore, len(stores))
		for _, s := range stores {
			byName[s.Name()] = s
		}
		if budget, onExceed := byName["cache-volume:ccache"].Budget(); budget != 20*1024*1024*1024 || onExceed != "wipe" {
			t.Errorf("ccache Budget() = (%d, %q), want (20GiB, \"wipe\")", budget, onExceed)
		}
		if budget, onExceed := byName["cache-volume:gradle-cache"].Budget(); budget != 0 || onExceed != "" {
			t.Errorf("gradle-cache (short form) Budget() = (%d, %q), want (0, \"\")", budget, onExceed)
		}
	})
}

func TestDockerSocketStoresFlatten(t *testing.T) {
	t.Run("zero value (nil client) flattens to nil, not a slice of nil stores", func(t *testing.T) {
		var s dockerSocketStores
		if got := s.flatten(); got != nil {
			t.Errorf("flatten() = %v, want nil", got)
		}
	})
}

func TestBuildCacheStoresEmptyInputYieldsNoStores(t *testing.T) {
	stores := buildCacheStores(nil, nil, slog.New(slog.DiscardHandler))
	if len(stores) != 0 {
		t.Errorf("buildCacheStores(nil, nil, ...) = %d stores, want 0", len(stores))
	}
}

// TestDiskStatusesForNeverCallsMeasure pins the health endpoint's cheap-path
// requirement directly: /healthz can be polled, so its disk section must
// come from statfs alone, never from a store's (possibly expensive) Measure.
func TestDiskStatusesForNeverCallsMeasure(t *testing.T) {
	measureCalls := 0
	measure := func(context.Context) (uint64, error) {
		measureCalls++
		return 999, nil
	}
	s1 := &fakeCacheStore{name: "a", path: "/", measureFn: measure}
	s2 := &fakeCacheStore{name: "b", path: "/", measureFn: measure}

	statuses := diskStatusesFor([]cachestore.CacheStore{s1, s2})
	if measureCalls != 0 {
		t.Errorf("Measure was called %d times, want 0 — the health disk section must be statfs-only", measureCalls)
	}
	if len(statuses) != 1 {
		t.Errorf("statuses = %+v, want exactly 1 (both stores share filesystem /, deduped)", statuses)
	}
}

func TestDiskStatusesForSkipsUnstattablePath(t *testing.T) {
	s := &fakeCacheStore{name: "bad", path: "/definitely/does/not/exist/xyz123"}
	statuses := diskStatusesFor([]cachestore.CacheStore{s})
	if len(statuses) != 0 {
		t.Errorf("statuses = %+v, want none for an unstattable path", statuses)
	}
}
