package main

import (
	"cmp"
	"log/slog"
	"slices"
	"time"

	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/cachestore"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/diskguard"
	"github.com/ysya/runscaler/internal/health"
	"github.com/ysya/runscaler/internal/provider"
)

// This file builds every cachestore.CacheStore the process uses. Both the
// periodic sweepers (sweepers.go) and the disk guard (startDiskGuard) must
// reclaim through stores configured identically: two independently derived
// copies of "which scaleset's settings win" for the same store would drift
// apart. Every store this process constructs — whether for a sweeper or for
// the disk guard — goes through exactly one of the functions in this file;
// runManager() and buildCacheStores both call them, so a store's
// configuration always traces back to one selection, never two that could
// disagree.

// dockerSocketStores holds the cachestore.CacheStore instances scoped to
// one Docker socket, plus the config/interval each store's own sweeper
// needs to log and tick on (not recoverable from a constructed store —
// CacheStore's interface exposes Enabled()/Kind()/etc, not the settings
// behind them). cachestore.CacheStore.Name() is not socket-qualified (e.g.
// "docker-garbage" repeats verbatim across sockets in a multi-daemon
// config), so a flat, unscoped slice cannot be searched by name without
// ambiguity — the sweepers below address a field on this struct directly
// instead of searching.
type dockerSocketStores struct {
	garbage       cachestore.CacheStore
	garbageCfg    cachestore.DockerGarbageConfig
	buildCache    cachestore.CacheStore
	buildCacheCfg cachestore.DockerBuildCacheConfig
	pruneInterval time.Duration

	buildx         cachestore.CacheStore
	buildxCfg      cachestore.BuildxConfig
	buildxInterval time.Duration

	sharedVolume []sharedVolumeSweepTarget
	cacheVolume  []cachestore.CacheStore
}

// flatten returns every store in s as one slice, for the disk guard (which
// only needs a flat set — it groups by filesystem via Path(), not by
// socket or name; see diskguard.Guard.statByFilesystem) and for
// buildCacheStores' return value.
func (s dockerSocketStores) flatten() []cachestore.CacheStore {
	if s.garbage == nil {
		return nil // client was nil; see dockerCacheStoresForSocket
	}
	stores := make([]cachestore.CacheStore, 0, 3+len(s.sharedVolume)+len(s.cacheVolume))
	stores = append(stores, s.garbage, s.buildCache, s.buildx)
	for _, t := range s.sharedVolume {
		stores = append(stores, t.store)
	}
	stores = append(stores, s.cacheVolume...)
	return stores
}

// dockerCacheStoresForSocket builds every Docker-daemon-scoped store for one
// socket's scale sets: the garbage and build-cache stores (always — a
// disabled prune setting is carried as Enabled()==false rather than the
// store not existing, so the disk guard can still name it in a shortfall
// warning), the buildx store, one store per unique shared-volume name, and
// one store per unique cache-volume name.
func dockerCacheStoresForSocket(sets []config.ScaleSetConfig, client *dockerclient.Client, logger *slog.Logger) dockerSocketStores {
	if client == nil {
		return dockerSocketStores{}
	}

	// One Info() round trip per socket; every store on this daemon shares
	// the resolved data root.
	rootDir := cachestore.ResolveDockerRootDir(client)
	garbageCfg, buildCacheCfg, pruneInterval := dockerPruneSettingsFor(sets)
	garbageCfg.RootDir, buildCacheCfg.RootDir = rootDir, rootDir
	garbage := cachestore.NewDockerGarbageStore(client, garbageCfg)
	buildCache := cachestore.NewDockerBuildCacheStore(client, buildCacheCfg)

	buildxCfg, buildxInterval := buildxConfigFor(sets, rootDir)

	return dockerSocketStores{
		garbage:        garbage,
		garbageCfg:     garbageCfg,
		buildCache:     buildCache,
		buildCacheCfg:  buildCacheCfg,
		pruneInterval:  pruneInterval,
		buildx:         cachestore.NewBuildxStore(client, buildxCfg),
		buildxCfg:      buildxCfg,
		buildxInterval: buildxInterval,
		sharedVolume:   sharedVolumeSweepTargetsFor(sets, client, rootDir, logger),
		cacheVolume:    cacheVolumeStoresFor(sets, client, rootDir),
	}
}

// dockerSettingsSource picks which scaleset in sets provides settings for a
// daemon-global store: the first with the routine sweep enabled, so its
// values (and the periodic sweeper's interval) match what the sweeper
// itself would use. If none enables it, settings still come from the first
// Docker scaleset in sets (defaults applied the same way) rather than a
// bare zero value — the disk guard reclaims through these stores
// regardless of Enabled() (a store's switch only gates its own routine
// sweep; see store.go's Enabled doc comment), so
// it needs real settings even when no scaleset opted into the periodic
// sweep — a bare zero-value PruneTTL/MaxAge would leave the guard unable
// to reclaim. enabled reports whether the *periodic sweeper* should run at
// all; it is independent of whether the returned scaleset's own settings
// are populated.
func dockerSettingsSource(sets []config.ScaleSetConfig, isEnabled func(config.ScaleSetConfig) bool) (source config.ScaleSetConfig, enabled bool, ok bool) {
	var dockerSets []config.ScaleSetConfig
	for _, ss := range sets {
		if !ss.IsTart() {
			dockerSets = append(dockerSets, ss)
		}
	}
	if len(dockerSets) == 0 {
		return config.ScaleSetConfig{}, false, false
	}

	source = dockerSets[0]
	for _, ss := range dockerSets {
		if isEnabled(ss) {
			return ss, true, true
		}
	}
	return source, false, true
}

// dockerPruneSettingsFor picks the settings for one socket's garbage and
// build-cache stores, plus the interval their shared sweep ticks on — see
// dockerSettingsSource for the selection rule, and the config accessors
// (ScaleSetConfig.DockerPruneTTL and the rest in retention.go) for how an
// unset or negative value resolves.
func dockerPruneSettingsFor(sets []config.ScaleSetConfig) (cachestore.DockerGarbageConfig, cachestore.DockerBuildCacheConfig, time.Duration) {
	source, enabled, ok := dockerSettingsSource(sets, func(ss config.ScaleSetConfig) bool { return ss.IsDockerPruneEnabled() })
	if !ok {
		return cachestore.DockerGarbageConfig{}, cachestore.DockerBuildCacheConfig{}, 0
	}

	return cachestore.DockerGarbageConfig{Enabled: enabled, PruneTTL: source.DockerPruneTTL()},
		cachestore.DockerBuildCacheConfig{Enabled: enabled, MaxAge: source.DockerBuildCacheMaxAge(), BudgetGB: source.Docker.BuildCacheBudgetGB},
		source.DockerPruneInterval()
}

// buildxConfigFor picks the settings for one socket's buildx store and the
// interval its own sweep ticks on — see dockerSettingsSource for the
// selection rule.
func buildxConfigFor(sets []config.ScaleSetConfig, rootDir string) (cachestore.BuildxConfig, time.Duration) {
	source, enabled, ok := dockerSettingsSource(sets, func(ss config.ScaleSetConfig) bool { return ss.IsBuildxCleanupEnabled() })
	if !ok {
		return cachestore.BuildxConfig{RootDir: rootDir}, 0
	}

	return cachestore.BuildxConfig{Enabled: enabled, MaxAge: source.DockerBuildxCleanupTTL(), RootDir: rootDir}, source.DockerBuildxCleanupInterval()
}

// sharedVolumeSweepTarget pairs a shared-volume store with the settings its
// own periodic sweep logs and ticks on. These fields are sweeper
// orchestration, not part of cachestore.CacheStore (the same split the
// [disk] guard's own Interval makes from the store interface), so they
// cannot be recovered from store after construction — both it and they come
// from the same selection pass in sharedVolumeSweepTargetsFor, so they can
// never disagree.
type sharedVolumeSweepTarget struct {
	store      cachestore.CacheStore
	volumeName string
	mountPath  string
	// ttl is 0 when the operator configured a shared volume but no
	// max-age. The store still exists in that case (see
	// sharedVolumeSweepTargetsFor); its Enabled() is what tells the
	// periodic sweeper not to tick on it.
	ttl      time.Duration
	interval time.Duration
}

// sharedVolumeSweepTargetsFor selects one target per unique shared-volume
// name among sets, in first-occurrence order: the first scaleset to name a
// volume provides its settings, and a conflicting later one is warned about.
// Both startSharedVolumeCleanup and dockerCacheStoresForSocket (and so,
// transitively, the disk guard) select from it, so they can never disagree.
//
// A store is built for every configured shared volume, including one with
// no max-age, the same "construct regardless, default the settings"
// treatment dockerPruneSettingsFor, buildxConfigFor and tartCacheStoreFor
// apply. A shared volume has no separate retention default to fall back
// on — SharedVolumeMaxAge *is* the policy, and 0 means "none configured"
// (see DockerConfig.SharedVolumeMaxAge's doc comment) — but that is an
// argument for the store reclaiming nothing, not for it not existing.
// sharedVolumeStore.Enabled() already requires MaxAge > 0 and Reclaim
// returns early when it is false, so Reclaim(Tier3)'s `days < 1 { days = 1 }`
// floor is unreachable with MaxAge=0 and no TTL can be invented.
//
// Skipping construction would cost real safety: a shared volume with no
// max-age would have no store, so no sweeper would touch it, the disk guard
// could not reach it at any tier, and it would appear in neither
// `runner cache` nor any warning — it would simply grow forever.
// startSharedVolumeCleanup warns about the missing setting; the store's own
// presence is what gives visibility and guard coverage.
func sharedVolumeSweepTargetsFor(sets []config.ScaleSetConfig, client *dockerclient.Client, rootDir string, logger *slog.Logger) []sharedVolumeSweepTarget {
	type settings struct {
		volumeName, mountPath, helperImage string
		ttl, interval                      time.Duration
	}

	picked := make(map[string]settings)
	var order []string // insertion order — map iteration is not stable
	for _, ss := range sets {
		if ss.IsTart() || ss.Docker.SharedVolume == "" {
			continue
		}
		interval := ss.DockerSharedVolumeCleanupInterval()
		s := settings{
			volumeName:  ss.SharedVolumeName(),
			mountPath:   ss.Docker.SharedVolume,
			helperImage: ss.RunnerImage,
			ttl:         ss.Docker.SharedVolumeMaxAge,
			interval:    interval,
		}
		if existing, ok := picked[s.volumeName]; ok {
			switch {
			case existing.ttl <= 0 && s.ttl > 0:
				// The first scaleset to name this volume configured no
				// max-age and a later one did. Prefer the real retention
				// policy — the same rule dockerSettingsSource follows (the
				// first scaleset that actually enables the sweep provides its
				// settings), so "first wins" can never mean "the scaleset
				// that left retention unconfigured wins".
				picked[s.volumeName] = s
			case existing != s:
				logger.Warn("Conflicting shared volume cleanup settings for volume, keeping first",
					slog.String("volume", s.volumeName),
					slog.Duration("kept_ttl", existing.ttl),
					slog.Duration("kept_interval", existing.interval),
					slog.String("kept_path", existing.mountPath),
					slog.Duration("ignored_ttl", s.ttl),
					slog.Duration("ignored_interval", s.interval),
					slog.String("ignored_path", s.mountPath),
				)
			}
			continue
		}
		picked[s.volumeName] = s
		order = append(order, s.volumeName)
	}

	targets := make([]sharedVolumeSweepTarget, 0, len(order))
	for _, name := range order {
		s := picked[name]
		store := cachestore.NewSharedVolumeStore(client, cachestore.SharedVolumeConfig{
			VolumeName:  s.volumeName,
			MountPath:   s.mountPath,
			HelperImage: s.helperImage,
			RootDir:     rootDir,
			MaxAge:      s.ttl,
		})
		targets = append(targets, sharedVolumeSweepTarget{
			store: store, volumeName: s.volumeName, mountPath: s.mountPath,
			ttl: s.ttl, interval: s.interval,
		})
	}
	return targets
}

// cacheVolumeStoresFor builds one cache-volume store per unique cache
// volume name referenced across sets (the same volume may be named by more
// than one scaleset sharing this socket — see DockerConfig.CacheVolumes'
// doc comment on shared-vs-isolated caches). There is no periodic sweeper
// to match settings against: cache volumes are never removed at exit and
// never swept by any TTL (same doc comment), so the disk guard's Tier4 is
// their only reclaim path. MountPath only
// matters inside the throwaway helper container this store's Measure/Reclaim
// run `du`/`find` in (see cachestore's runVolumeHelper) — never to the
// runner containers that mount the volume — so the first scaleset to name a
// given volume picks that path arbitrarily; ParseCacheVolumes errors are
// ignored here the same way NewDockerProvider ignores them, since
// ScaleSetConfig.Validate already surfaced them before startup.
//
// BudgetBytes/OnExceed pass straight through from the parsed mount
// (m.BudgetBytes/m.OnExceed): 0/"" for a short-form (cache-volumes) entry or
// a long-form ([[docker.cache]]) one with no budget set, matching
// cachestore.CacheStore.Budget's "a store with no such policy returns
// (0, \"\")" contract — the disk guard's enforceBudgets skips a zero budget
// entirely, so no extra "is this configured" branch is needed here.
func cacheVolumeStoresFor(sets []config.ScaleSetConfig, client *dockerclient.Client, rootDir string) []cachestore.CacheStore {
	seen := make(map[string]bool)
	var stores []cachestore.CacheStore
	for _, ss := range sets {
		if ss.IsTart() {
			continue
		}
		mounts, err := ss.Docker.ParseCacheVolumes()
		if err != nil {
			continue
		}
		for _, m := range mounts {
			if seen[m.Volume] {
				continue
			}
			seen[m.Volume] = true
			stores = append(stores, cachestore.NewCacheVolumeStore(client, cachestore.CacheVolumeConfig{
				VolumeName:  m.Volume,
				MountPath:   m.Path,
				HelperImage: ss.RunnerImage,
				RootDir:     rootDir,
				BudgetBytes: m.BudgetBytes,
				OnExceed:    m.OnExceed,
			}))
		}
	}
	return stores
}

// tartCacheSweepTarget pairs a Tart-cache store with the settings its own
// periodic sweep logs and ticks on. Every configured home gets a target
// (see tartCacheStores); the store's Enabled() says whether the periodic
// sweeper runs for it.
type tartCacheSweepTarget struct {
	store         cachestore.CacheStore
	home          string
	maxAge        time.Duration
	spaceBudgetGB int
	interval      time.Duration
	keepImages    []string // runner images every prune marks used first
}

// tartCacheStores builds one target per unique TART_HOME across scaleSets —
// every Tart scaleset contributes its home, regardless of whether
// cache-cleanup is enabled there: the disk guard reclaims through these
// stores regardless of Enabled() (see dockerSettingsSource's identical
// rationale, which this mirrors for Tart), so it needs to see every
// configured home, not just the ones with a periodic sweep turned on.
func tartCacheStores(scaleSets []config.ScaleSetConfig, logger *slog.Logger) map[string]tartCacheSweepTarget {
	homes := make(map[string][]config.ScaleSetConfig)
	for _, ss := range scaleSets {
		if ss.IsTart() {
			homes[ss.Tart.Home] = append(homes[ss.Tart.Home], ss)
		}
	}

	targets := make(map[string]tartCacheSweepTarget, len(homes))
	for home, sets := range homes {
		targets[home] = tartCacheStoreFor(home, sets, logger)
	}
	return targets
}

// tartCacheStoreFor builds the target for one TART_HOME: settings come from
// the first scaleset sharing it with cache-cleanup enabled (a conflicting
// later enabled one is warned about), which is also what the periodic
// sweeper itself uses — or, if no scaleset sharing this home enables it,
// from the first scaleset's own values (still defaulted) purely so the disk
// guard has something real to reclaim by; see tartCacheStores' doc comment.
func tartCacheStoreFor(home string, sets []config.ScaleSetConfig, logger *slog.Logger) tartCacheSweepTarget {
	type settings struct {
		maxAge, interval time.Duration
		budgetGB         int
	}

	var chosen *settings
	for _, ss := range sets {
		if !ss.IsTartCacheCleanupEnabled() {
			continue
		}
		s := settings{maxAge: ss.TartCacheMaxAge(), interval: ss.TartCacheCleanupInterval(), budgetGB: ss.Tart.CacheBudgetGB}
		if chosen == nil {
			chosen = &s
			continue
		}
		if *chosen != s {
			logger.Warn("Conflicting tart cache cleanup settings for TART_HOME, keeping first",
				slog.String("home", home),
				slog.Duration("kept_max_age", chosen.maxAge),
				slog.Int("kept_budget_gb", chosen.budgetGB),
				slog.Duration("kept_interval", chosen.interval),
				slog.Duration("ignored_max_age", s.maxAge),
				slog.Int("ignored_budget_gb", s.budgetGB),
				slog.Duration("ignored_interval", s.interval),
			)
		}
	}

	enabled := chosen != nil
	if chosen == nil {
		// No scaleset sharing this home enables cache-cleanup — the
		// periodic sweeper stays off, but the disk guard still needs real
		// settings to reclaim by. Fall back to the first scaleset's own
		// values, still defaulted the same way.
		chosen = &settings{maxAge: sets[0].TartCacheMaxAge(), budgetGB: sets[0].Tart.CacheBudgetGB}
	}

	// Every scaleset cloning from this home needs its image kept, including
	// one that leaves cleanup to another scaleset sharing the home.
	var keep []string
	for _, ss := range sets {
		if ss.RunnerImage != "" && !slices.Contains(keep, ss.RunnerImage) {
			keep = append(keep, ss.RunnerImage)
		}
	}

	store := cachestore.NewTartStore(provider.TartCommandRunner(home), cachestore.TartConfig{
		Enabled:    enabled,
		Home:       home,
		MaxAge:     chosen.maxAge,
		BudgetGB:   chosen.budgetGB,
		KeepImages: keep,
	})
	return tartCacheSweepTarget{
		store: store, home: home, maxAge: chosen.maxAge, spaceBudgetGB: chosen.budgetGB,
		interval: chosen.interval, keepImages: keep,
	}
}

// buildCacheStores constructs every cachestore.CacheStore configured across
// scaleSets: the Docker-daemon-scoped stores for each unique socket (via
// dockerCacheStoresForSocket) and the Tart-scoped stores for each unique
// TART_HOME (via tartCacheStores). It is the entry point for callers that
// want the full host-wide set without wiring the periodic sweepers, such as
// `runner cache`.
//
// runManager() does not call this function — see its own comment at the
// call site for why (avoiding double construction and double
// "Conflicting ... settings" warnings) — but it builds its stores through
// the same two functions, so both paths select settings identically.
func buildCacheStores(scaleSets []config.ScaleSetConfig, dockerClients map[string]*dockerclient.Client, logger *slog.Logger) []cachestore.CacheStore {
	var stores []cachestore.CacheStore
	for socket, sets := range groupDockerScaleSets(scaleSets) {
		stores = append(stores, dockerCacheStoresForSocket(sets, dockerClients[socket], logger).flatten()...)
	}
	for _, t := range tartCacheStores(scaleSets, logger) {
		stores = append(stores, t.store)
	}
	return stores
}

// diskStatusesFor reports capacity for every distinct filesystem stores
// live on, deduplicated by filesystem so a host with many stores on one
// disk (the common case) reports one row, not one per store — matching
// diskguard.Guard.statByFilesystem's own per-filesystem grouping, though
// this never reclaims or logs, it only reads. Statfs only, never Measure
// (see health.DiskStatus's doc comment) — this is called on every /healthz
// request, so it must stay that cheap. A store whose Path() fails statfs is
// silently omitted rather than warned about: unlike diskguard.Guard (which
// sweeps a handful of times an hour on its own schedule), this can run on
// every poll from an external monitor, and logging on every one of those
// would spam the log for a condition the response already reflects (that
// filesystem's entry is simply missing).
func diskStatusesFor(stores []cachestore.CacheStore) []health.DiskStatus {
	seen := make(map[string]bool)
	var statuses []health.DiskStatus
	for _, s := range stores {
		path := s.Path()
		st, err := diskguard.StatFor(path)
		if err != nil || seen[st.ID] {
			continue
		}
		seen[st.ID] = true

		var freePercent float64
		if st.TotalBytes > 0 {
			freePercent = float64(st.FreeBytes) / float64(st.TotalBytes) * 100
		}
		statuses = append(statuses, health.DiskStatus{
			Filesystem:  path,
			FreePercent: freePercent,
			FreeBytes:   st.FreeBytes,
			TotalBytes:  st.TotalBytes,
		})
	}
	slices.SortFunc(statuses, func(a, b health.DiskStatus) int { return cmp.Compare(a.Filesystem, b.Filesystem) })
	return statuses
}
