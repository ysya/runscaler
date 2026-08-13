package main

import (
	"log/slog"
	"testing"
	"time"

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
	t.Run("no scaleset enabled yields disabled configs and zero interval", func(t *testing.T) {
		sets := []config.ScaleSetConfig{{ScaleSetName: "a"}}
		garbage, buildCache, interval := dockerPruneSettingsFor(sets)
		if garbage.Enabled || buildCache.Enabled {
			t.Errorf("garbage.Enabled=%v buildCache.Enabled=%v, want both false", garbage.Enabled, buildCache.Enabled)
		}
		if interval != 0 {
			t.Errorf("interval = %v, want 0", interval)
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
	t.Run("disabled by default", func(t *testing.T) {
		cfg, interval := buildxConfigFor([]config.ScaleSetConfig{{}}, "/var/lib/docker")
		if cfg.Enabled || interval != 0 {
			t.Errorf("cfg=%+v interval=%v, want disabled/0 (buildx-cleanup defaults off)", cfg, interval)
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
			{Docker: config.DockerConfig{SharedVolume: "/shared", SharedVolumeTTL: 24 * time.Hour}},  // default volume name
			{Docker: config.DockerConfig{SharedVolume: "/shared", SharedVolumeTTL: 999 * time.Hour}}, // same default name, ignored
			{Docker: config.DockerConfig{SharedVolume: "/shared", SharedVolumeTTL: time.Hour, SharedVolumeName: "team-a"}},
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
		sets := []config.ScaleSetConfig{{Docker: config.DockerConfig{SharedVolume: "/shared", SharedVolumeTTL: time.Hour}}}
		targets := sharedVolumeSweepTargetsFor(sets, nil, "/root", slog.New(slog.DiscardHandler))
		if len(targets) != 1 || targets[0].interval != config.DefaultSharedVolumeCleanupInterval {
			t.Errorf("targets = %+v, want 1 target with default interval", targets)
		}
	})
}

func TestTartCacheSweepTargetsFor(t *testing.T) {
	t.Run("disabled scaleset yields no target for its home", func(t *testing.T) {
		no := false
		sets := []config.ScaleSetConfig{{Backend: "tart", Tart: config.TartConfig{Home: "/Volumes/A", CacheCleanup: &no}}}
		targets := tartCacheSweepTargetsFor(sets, slog.New(slog.DiscardHandler))
		if len(targets) != 0 {
			t.Errorf("targets = %+v, want none (cache-cleanup disabled)", targets)
		}
	})

	t.Run("first enabled scaleset per home wins, distinct homes each get a target", func(t *testing.T) {
		no := false
		sets := []config.ScaleSetConfig{
			{Backend: "tart", Tart: config.TartConfig{Home: "/Volumes/A", CacheCleanup: &no}},                // skipped: disabled
			{Backend: "tart", Tart: config.TartConfig{Home: "/Volumes/A", CacheMaxAge: 3 * 24 * time.Hour}},  // enabled by default, wins for /Volumes/A
			{Backend: "tart", Tart: config.TartConfig{Home: "/Volumes/A", CacheMaxAge: 99 * 24 * time.Hour}}, // ignored
			{Backend: "tart", Tart: config.TartConfig{Home: "/Volumes/B", CacheSpaceBudgetGB: 50}},
		}
		targets := tartCacheSweepTargetsFor(sets, slog.New(slog.DiscardHandler))
		if len(targets) != 2 {
			t.Fatalf("targets = %+v, want 2 (one per unique home)", targets)
		}
		byHome := map[string]tartCacheSweepTarget{}
		for _, tg := range targets {
			byHome[tg.home] = tg
		}
		if got := byHome["/Volumes/A"].maxAge; got != 3*24*time.Hour {
			t.Errorf("/Volumes/A max age = %v, want 3 days (first enabled scaleset)", got)
		}
		if got := byHome["/Volumes/B"].spaceBudgetGB; got != 50 {
			t.Errorf("/Volumes/B budget = %d, want 50", got)
		}
		if byHome["/Volumes/A"].store == nil || byHome["/Volumes/B"].store == nil {
			t.Error("every target must carry a constructed store")
		}
	})

	t.Run("non-tart scaleset never contributes a target", func(t *testing.T) {
		sets := []config.ScaleSetConfig{{Backend: "docker", Tart: config.TartConfig{Home: "/Volumes/A"}}}
		if targets := tartCacheSweepTargetsFor(sets, slog.New(slog.DiscardHandler)); len(targets) != 0 {
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
