package diskguard

import (
	"context"
	"log/slog"
	"slices"

	"github.com/ysya/runscaler/internal/cachestore"
)

// Config configures a Guard: whether it runs at all, the free-space floor
// that triggers reclaim and the level it reclaims back up to, and the
// highest tier it is allowed to reach while doing so.
type Config struct {
	Enabled             bool
	MinFree, TargetFree Threshold
	MaxTier             cachestore.Tier
}

// Guard decides when to reclaim disk space, from which cachestore.CacheStore
// stores, in what order, and when to stop: a disk-pressure tier ladder that
// runs only when a filesystem's free space has dropped below MinFree, and
// never reclaims past MaxTier.
type Guard struct {
	cfg    Config
	stores []cachestore.CacheStore
	statFn func(string) (FSStat, error)
	logger *slog.Logger
}

// New constructs a Guard. statFn is injected rather than the package's own
// StatFor being called directly so tests can fake filesystem stats without
// touching a real mount; production callers pass StatFor itself.
func New(cfg Config, stores []cachestore.CacheStore, statFn func(string) (FSStat, error), logger *slog.Logger) *Guard {
	return &Guard{cfg: cfg, stores: stores, statFn: statFn, logger: logger}
}

// filesystemGroup pairs one filesystem's stores with a stat snapshot for
// it and a representative path to re-stat through. Every store in the
// group resolves (via Path()) to the same filesystem, so any one of their
// paths yields an equivalent statfs result — path is fixed to whichever
// store's path was stat'd first when the group was built.
type filesystemGroup struct {
	path   string
	stat   FSStat
	stores []cachestore.CacheStore
}

// statByFilesystem groups g.stores by the filesystem their Path() resolves
// to, statting each store's path exactly once. Unlike the package-level
// GroupByFilesystem (which aborts entirely on the first statfs failure —
// see its doc comment), a single store's statfs failure here only warns
// and excludes that one store; every other store is still grouped and
// swept normally.
func (g *Guard) statByFilesystem() map[string]filesystemGroup {
	groups := make(map[string]filesystemGroup)
	for _, s := range g.stores {
		path := s.Path()
		st, err := g.statFn(path)
		if err != nil {
			g.logger.Warn("Disk guard: statfs failed, skipping store",
				slog.String("store", s.Name()), slog.String("path", path), slog.Any("error", err))
			continue
		}

		grp, ok := groups[st.ID]
		if !ok {
			grp = filesystemGroup{path: path, stat: st}
		}
		grp.stores = append(grp.stores, s)
		groups[st.ID] = grp
	}
	return groups
}

// Sweep reclaims from every filesystem under disk pressure. It always
// returns nil: every failure mode below (a store's statfs or Reclaim call
// failing) is logged and skipped rather than propagated, so one
// misbehaving store or filesystem never stops the sweep from doing what it
// can for the rest.
func (g *Guard) Sweep(ctx context.Context) error {
	if !g.cfg.Enabled {
		return nil
	}

	for _, grp := range g.statByFilesystem() {
		g.sweepFilesystem(ctx, grp)
	}
	return nil
}

// sweepFilesystem reclaims from one filesystem's stores if it is under
// MinFree, walking tiers Tier1..MaxTier and re-statting after each one so
// it can stop the instant TargetFree is met — it never trusts stores' own
// freed-bytes reports to decide that, since several stores legitimately
// report 0 while genuinely freeing space (see e.g. buildxStore.Reclaim).
func (g *Guard) sweepFilesystem(ctx context.Context, grp filesystemGroup) {
	st := grp.stat
	if st.FreeBytes >= g.cfg.MinFree.BytesOf(st.TotalBytes) {
		return // this filesystem is healthy; nothing to do
	}
	targetBytes := g.cfg.TargetFree.BytesOf(st.TotalBytes)

	// Names of stores this filesystem needed but could not touch because
	// the operator disabled them — reported in the shortfall warning below
	// if the target is never met, so the warning is actionable.
	disabledSkipped := make(map[string]struct{})

	for tier := cachestore.Tier1; tier <= g.cfg.MaxTier; tier++ {
		for _, s := range grp.stores {
			if !slices.Contains(cachestore.TiersFor(s.Kind()), tier) {
				continue // this store has nothing to give at this tier
			}
			if !s.Enabled() {
				// Enabled() == false is a deliberate operator choice; the
				// guard must not override it.
				disabledSkipped[s.Name()] = struct{}{}
				continue
			}
			if _, err := s.Reclaim(ctx, tier); err != nil {
				g.logger.Warn("Disk guard: reclaim failed",
					slog.String("store", s.Name()), slog.Any("tier", tier), slog.Any("error", err))
			}
		}

		newSt, err := g.statFn(grp.path)
		if err != nil {
			g.logger.Warn("Disk guard: statfs failed mid-sweep, stopping this filesystem's sweep",
				slog.String("path", grp.path), slog.Any("error", err))
			return
		}
		st = newSt
		if st.FreeBytes >= targetBytes {
			return // target met — stop before reclaiming anything more
		}
	}

	g.warnShortfall(grp.path, st, targetBytes, disabledSkipped)
}

// warnShortfall reports that a filesystem is still short of TargetFree
// after reclaiming through every tier up to MaxTier, and — if any store
// this filesystem depends on was skipped for being disabled — names it and
// why, so the operator knows what re-enabling would buy back.
func (g *Guard) warnShortfall(path string, st FSStat, targetBytes uint64, disabledSkipped map[string]struct{}) {
	attrs := []any{
		slog.String("path", path),
		slog.Uint64("free_bytes", st.FreeBytes),
		slog.Uint64("target_bytes", targetBytes),
		// targetBytes > st.FreeBytes is guaranteed here: sweepFilesystem
		// only reaches this call when the loop above exits without ever
		// meeting the target, so this subtraction cannot underflow.
		slog.Uint64("shortfall_bytes", targetBytes-st.FreeBytes),
	}
	if len(disabledSkipped) > 0 {
		names := make([]string, 0, len(disabledSkipped))
		for name := range disabledSkipped {
			names = append(names, name)
		}
		slices.Sort(names)
		attrs = append(attrs,
			slog.Any("disabled_stores", names),
			slog.String("disabled_reason", "disabled by operator config, the guard will not reclaim from it"))
	}
	g.logger.Warn("Disk guard: reached max-tier without meeting target-free", attrs...)
}

// NeedsReclaim reports whether any store's filesystem is currently below
// MinFree. It is the cheap pre-job-start check: one statfs per distinct
// filesystem, no Measure calls (Measure can walk an entire volume) and no
// reclaiming — callers that get true back are expected to run Sweep
// themselves before proceeding.
func (g *Guard) NeedsReclaim() (bool, error) {
	if !g.cfg.Enabled {
		return false, nil
	}
	for _, grp := range g.statByFilesystem() {
		if grp.stat.FreeBytes < g.cfg.MinFree.BytesOf(grp.stat.TotalBytes) {
			return true, nil
		}
	}
	return false, nil
}
