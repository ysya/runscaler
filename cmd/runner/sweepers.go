package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/ysya/runscaler/internal/bytesize"
	"github.com/ysya/runscaler/internal/cachestore"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/diskguard"
)

// This file runs the periodic sweepers and the disk guard over the stores
// cachewiring.go builds; each sweeper ticks through runPeriodic.

// logReclaimResult logs the outcome of one store's Reclaim call: Info with
// the reclaimed total when it freed anything — so a data-deleting sweep
// leaves a record an operator can find, since cachestore.CacheStore.Reclaim's
// uniform (bytes, error) return carries no summary of its own — Debug when
// it ran but found nothing to reclaim, and Warn when it failed outright.
// action names what ran (e.g. "Docker garbage prune"); store names which
// store — dockerSocketStores' sweep reclaims through two stores in one
// pass, so it calls this once per store rather than logging one combined
// line that would hide which one actually did anything.
func logReclaimResult(logger *slog.Logger, action, store string, freed uint64, err error) {
	if err != nil {
		logger.Warn(action+" failed", slog.String("store", store), slog.Any("error", err))
		return
	}
	if freed > 0 {
		logger.Info(action+" reclaimed disk", slog.String("store", store), slog.String("reclaimed", bytesize.Format(freed)))
		return
	}
	logger.Debug(action+" found nothing to reclaim", slog.String("store", store))
}

// startSharedVolumeCleanup launches one background goroutine per shared-volume
// sweep target (see sharedVolumeSweepTargetsFor for the dedup/settings
// selection), running the TTL sweeper periodically through the shared-volume
// store's own Reclaim(Tier3) — the same reclaim path the disk guard uses,
// so there is exactly one implementation of "delete stale shared-volume
// files" in this process (see cachestore.sharedVolumeStore.Reclaim).
// No-op when targets is empty (no scaleset configures a shared volume, or
// the Docker client was unavailable — see dockerCacheStoresForSocket).
//
// A target with no max-age gets a startup warning instead of a sweeper.
// Since exit-time volume deletion was removed, nothing else would ever
// reclaim it: the disk guard cannot reach a KindScratch store below its TTL
// at any tier, so an unconfigured max-age really does mean unbounded growth,
// and the operator has to be told in the one place they will see it.
func startSharedVolumeCleanup(ctx context.Context, targets []sharedVolumeSweepTarget, logger *slog.Logger) {
	for _, t := range targets {
		t := t
		if !t.store.Enabled() {
			logger.Warn("Shared volume has no max-age — it will grow without bound and is never reclaimed automatically",
				slog.String("volume", t.volumeName),
				slog.String("path", t.mountPath),
				slog.String("action", "set [docker] shared-volume-max-age (e.g. \"72h\") to a value longer than your longest workflow run"),
			)
			continue
		}

		logger.Info("Shared volume TTL cleanup enabled",
			slog.String("volume", t.volumeName),
			slog.Duration("ttl", t.ttl),
			slog.Duration("interval", t.interval),
			slog.String("path", t.mountPath),
		)

		sweep := func() {
			freed, err := t.store.Reclaim(ctx, cachestore.Tier3)
			logReclaimResult(logger, "Shared volume cleanup", t.store.Name(), freed, err)
		}

		runPeriodic(ctx, t.interval, sweep)
	}
}

// startBuildxCleanup launches a background goroutine that periodically removes
// orphaned buildx BuildKit builder containers (and their state volumes) from
// the shared Docker daemon, through the buildx store's own Reclaim(Tier1) —
// the same reclaim path the disk guard uses (see
// cachestore.buildxStore.Reclaim). Builders are global to the
// daemon — like the shared volume — so a single sweeper covers all Docker
// scalesets; stores.buildxCfg/buildxInterval were selected from the first
// Docker scaleset with cleanup enabled (see buildxConfigFor). This is
// opt-in because the daemon may contain persistent or unrelated builders.
// No-op when it is not enabled for this socket.
func startBuildxCleanup(ctx context.Context, stores dockerSocketStores, logger *slog.Logger) {
	if !stores.buildx.Enabled() {
		return
	}

	logger.Info("Buildx builder cleanup enabled",
		slog.Duration("max_age", stores.buildxCfg.MaxAge),
		slog.Duration("interval", stores.buildxInterval),
	)

	sweep := func() {
		freed, err := stores.buildx.Reclaim(ctx, cachestore.Tier1)
		logReclaimResult(logger, "Buildx builder cleanup", stores.buildx.Name(), freed, err)
	}

	runPeriodic(ctx, stores.buildxInterval, sweep)
}

// startDockerPrune launches a background goroutine that periodically reclaims
// disk on the shared Docker daemon: stopped containers and dangling images
// older than the TTL (the garbage store's Reclaim(Tier1)), plus age/budget-
// based build cache retention (the build-cache store's Reclaim(Tier2)) — the
// same two reclaim paths the disk guard uses (see cachestore.dockerGarbageStore
// and cachestore.dockerBuildCacheStore). With DooD, job-created garbage lands
// directly on the host daemon, so an exit-time prune alone would never
// reclaim disk on a long-running process; a single sweeper covers all Docker
// scalesets sharing this socket, using the settings selected in
// dockerPruneSettingsFor (the first Docker scaleset with prune enabled).
// This is opt-in because the target daemon may also contain non-runner
// workloads. No-op when prune is not enabled for this socket.
//
// build-cache-budget only takes effect through dockerBuildCacheStore.Reclaim
// (Tier2) — the only place BudgetGB is consumed (see that store's Budget()
// doc comment on why it is deliberately not surfaced to the disk guard's
// separate, statfs-triggered budget-enforcement pass too, which would
// otherwise double-drive the same number). Reclaim(Tier2) is reached two
// ways: this sweep (only when prune is enabled here) and the disk guard's
// own tier ladder under disk pressure, which runs regardless of whether
// prune is enabled (see store.go's Enabled doc comment), as long as
// max-tier >= 2 (the default is 3). So: with prune disabled, a configured
// build-cache-budget is enforced only once the guard's tier ladder reaches
// Tier2 under real disk pressure, not on this sweep's own schedule.
func startDockerPrune(ctx context.Context, stores dockerSocketStores, logger *slog.Logger) {
	if !stores.garbage.Enabled() {
		return
	}

	logger.Info("Docker runtime prune enabled",
		slog.Duration("ttl", stores.garbageCfg.PruneTTL),
		slog.Duration("interval", stores.pruneInterval),
		slog.Duration("build_cache_max_age", stores.buildCacheCfg.MaxAge),
		slog.Int("build_cache_budget_gb", stores.buildCacheCfg.BudgetGB),
	)

	sweep := func() {
		freed, err := stores.garbage.Reclaim(ctx, cachestore.Tier1)
		logReclaimResult(logger, "Docker garbage prune", stores.garbage.Name(), freed, err)

		freed, err = stores.buildCache.Reclaim(ctx, cachestore.Tier2)
		logReclaimResult(logger, "Docker build-cache prune", stores.buildCache.Name(), freed, err)
	}

	runPeriodic(ctx, stores.pruneInterval, sweep)
}

// startTartCacheCleanup launches one background goroutine per enabled Tart
// cache target (see tartCacheStores for the per-home dedup/settings
// selection — targets includes every configured TART_HOME, so this
// function filters for enabled itself), running `tart prune` on a timer
// through the tart store's own Reclaim(Tier2) — the same reclaim path the
// disk guard uses (see cachestore.tartStore.Reclaim). Enabled by default;
// no-op for any home where no Tart scaleset has cleanup enabled.
func startTartCacheCleanup(ctx context.Context, targets map[string]tartCacheSweepTarget, logger *slog.Logger) {
	for _, s := range targets {
		if !s.store.Enabled() {
			continue
		}
		s := s
		logger.Info("Tart cache cleanup enabled",
			slog.String("home", s.home),
			slog.Duration("max_age", s.maxAge),
			slog.Int("space_budget_gb", s.spaceBudgetGB),
			slog.Duration("interval", s.interval),
		)

		sweep := func() {
			freed, err := s.store.Reclaim(ctx, cachestore.Tier2)
			logReclaimResult(logger, "Tart cache cleanup", s.store.Name(), freed, err)
		}

		runPeriodic(ctx, s.interval, sweep)
	}
}

// startDiskGuard launches a background goroutine that periodically reclaims
// disk space host-wide once a filesystem's free space drops below
// cfg.Disk's min-free threshold, walking cachestore's tier ladder up to
// max-tier (see diskguard.Guard.Sweep). Structured like startDockerPrune:
// an early return when disabled, an Info log of the effective settings
// (resolved once by config.DiskConfig.Resolve), and a periodic sweep that
// starts immediately, so a host already under pressure at startup does
// not wait a full interval. A failed sweep is a Warn, never fatal.
//
// Returns the constructed Guard, or nil when the guard is disabled or
// misconfigured. The caller (runManager()) threads this same instance into every
// scale set's pre-job-start check (see runScaleSetController, controller.WithDiskChecker)
// instead of building a second Guard over the same stores.
func startDiskGuard(ctx context.Context, stores []cachestore.CacheStore, cfg config.Config, logger *slog.Logger) *diskguard.Guard {
	if !cfg.IsDiskGuardEnabled() {
		return nil
	}

	// ValidateGlobal already rejected these settings before startup if they
	// did not resolve; the error is handled anyway rather than ignored.
	settings, err := cfg.Disk.Resolve()
	if err != nil {
		logger.Warn("Disk guard: invalid [disk] settings, guard disabled", slog.Any("error", err))
		return nil
	}

	guard := diskguard.New(diskguard.Config{
		Enabled:    true,
		MinFree:    settings.MinFreeThreshold,
		TargetFree: settings.TargetFreeThreshold,
		MaxTier:    cachestore.Tier(settings.MaxTier),
	}, stores, diskguard.StatFor, logger)

	logger.Info("Disk guard enabled",
		slog.String("min_free", settings.MinFree),
		slog.String("target_free", settings.TargetFree),
		slog.Duration("interval", settings.Interval),
		slog.Int("max_tier", settings.MaxTier),
		slog.Int("stores", len(stores)),
	)

	runPeriodic(ctx, settings.Interval, func() {
		if err := guard.Sweep(ctx); err != nil {
			logger.Warn("Disk guard sweep failed", slog.Any("error", err))
		}
	})

	return guard
}

// runPeriodic runs sweep once right away, then every interval until ctx
// ends. The immediate first sweep is deliberate: a host already bloated at
// startup, after a crash say, should not wait a full interval for relief.
func runPeriodic(ctx context.Context, interval time.Duration, sweep func()) {
	go func() {
		sweep()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}
