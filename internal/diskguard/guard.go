// Package diskguard reclaims disk space from cachestore.CacheStore
// instances under pressure: it groups stores by the filesystem their Path()
// resolves to, reads point-in-time capacity via StatFor, and walks
// cachestore's tier ladder (or a store's own configured budget) filesystem
// by filesystem until TargetFree is met or every allowed tier has run.
// Threshold parsing (percentages and absolute sizes) lives in the leaf
// package internal/bytesize, imported by both this package and
// internal/config.
package diskguard

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/ysya/runscaler/internal/bytesize"
	"github.com/ysya/runscaler/internal/cachestore"
)

// Config configures a Guard: whether it runs at all, the free-space floor
// that triggers reclaim and the level it reclaims back up to, and the
// highest tier it is allowed to reach while doing so.
type Config struct {
	Enabled             bool
	MinFree, TargetFree bytesize.Threshold
	MaxTier             cachestore.Tier
}

// Guard decides when to reclaim disk space, from which cachestore.CacheStore
// stores, in what order, and when to stop. It runs two independent
// mechanisms on every Sweep: a per-store budget check (Steps 5-9 of this
// package's design — runs regardless of disk pressure, unbounded by
// MaxTier), and a disk-pressure tier ladder (Steps 1-4 — runs only when a
// filesystem's free space has dropped below MinFree, and never reclaims
// past MaxTier).
//
// Neither mechanism consults a store's Enabled() (revised 2026-08-14 —
// see docs/superpowers/specs/2026-08-13-cache-architecture-design.md,
// "各 store 的啟用開關只約束例行清理"). Enabled() means "run this store's own
// routine periodic cleanup", not "never touch this even under disk
// pressure" — the periodic sweepers in cmd/runner still gate on it, but the
// guard reclaims from every configured store regardless, because refusing
// to touch a store the operator merely left off its routine schedule would
// leave the guard unable to reclaim anything on a stock config (prune and
// buildx-cleanup default off since v0.4). A host whose disk this guard must
// never touch at all should set `[disk] guard = false`, not rely on a
// per-store Enabled() it was never designed to satisfy.
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

// Sweep runs the budget-enforcement pass and then the disk-pressure tier
// ladder, filesystem by filesystem. It always returns nil: every failure
// mode below (a store's statfs, Measure, or Reclaim call failing) is
// logged and skipped rather than propagated, so one misbehaving store or
// filesystem never stops the sweep from doing what it can for the rest.
func (g *Guard) Sweep(ctx context.Context) error {
	if !g.cfg.Enabled {
		return nil
	}

	// Budget enforcement is a separate concern from the tier ladder below:
	// it runs on every sweep regardless of disk pressure and is not bounded
	// by MaxTier. See enforceBudgets' doc comment.
	g.enforceBudgets(ctx)

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
//
// Every store in scope for a tier is reclaimed regardless of Enabled() —
// see Guard's doc comment for why the guard does not treat Enabled() as an
// opt-out the way the periodic sweepers do. TiersFor(s.Kind()) is still the
// one gate that always applies: it is a data-safety rule (what tier a kind
// of data may ever be reclaimed at), not an operator preference, so it is
// not something Enabled() ever controlled in the first place.
func (g *Guard) sweepFilesystem(ctx context.Context, grp filesystemGroup) {
	st := grp.stat
	if st.FreeBytes >= g.cfg.MinFree.BytesOf(st.TotalBytes) {
		return // this filesystem is healthy; nothing to do
	}
	targetBytes := g.cfg.TargetFree.BytesOf(st.TotalBytes)

	for tier := cachestore.Tier1; tier <= g.cfg.MaxTier; tier++ {
		for _, s := range grp.stores {
			if !slices.Contains(cachestore.TiersFor(s.Kind()), tier) {
				continue // this store has nothing to give at this tier
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

	g.warnShortfall(grp.path, st, targetBytes)
}

// warnShortfall reports that a filesystem is still short of TargetFree
// after reclaiming through every tier up to MaxTier — every store in scope
// was already reclaimed from regardless of Enabled() (see Guard's doc
// comment), so raising max-tier or investigating what is actually filling
// the disk are the only remaining options, and this warning says by how
// much.
func (g *Guard) warnShortfall(path string, st FSStat, targetBytes uint64) {
	g.logger.Warn("Disk guard: reached max-tier without meeting target-free",
		slog.String("path", path),
		slog.Uint64("free_bytes", st.FreeBytes),
		slog.Uint64("target_bytes", targetBytes),
		// targetBytes > st.FreeBytes is guaranteed here: sweepFilesystem
		// only reaches this call when the loop above exits without ever
		// meeting the target, so this subtraction cannot underflow.
		slog.Uint64("shortfall_bytes", targetBytes-st.FreeBytes),
	)
}

// enforceBudgets checks every store's own configured budget and acts on
// it, independent of disk pressure, unbounded by MaxTier, and regardless of
// Enabled() (see Guard's doc comment): it is the store's own retention
// policy taking effect, not the guard's disk-pressure escalation (that is
// sweepFilesystem, above). "Unbounded by MaxTier" only means the tier
// ceiling does not apply here — the wipe path below is still gated by
// TiersFor, the same kind-based safety rule every tier-ladder Reclaim call
// goes through. It calls Measure, so — unlike the rest of Sweep — it is
// never run from NeedsReclaim, which must stay on the cheap statfs-only
// path.
func (g *Guard) enforceBudgets(ctx context.Context) {
	for _, s := range g.stores {
		budget, onExceed := s.Budget()
		if budget == 0 {
			continue // no budget configured for this store
		}

		used, err := s.Measure(ctx)
		if err != nil {
			g.logger.Warn("Disk guard: measure failed, skipping budget check",
				slog.String("store", s.Name()), slog.Any("error", err))
			continue
		}
		if used <= budget {
			continue
		}

		if onExceed == "wipe" {
			// StoreKind exists precisely to make it structurally impossible
			// to reclaim a store's data at a tier that would destroy
			// something live (see store.go's StoreKind doc comment); every
			// tier-ladder Reclaim call above is gated by it via TiersFor,
			// and this call site must not be the one exception. Without
			// this check, a future KindScratch store that grows a budget
			// would have its un-expired, in-flight handoff data wiped
			// wholesale the first time it went over budget.
			if !slices.Contains(cachestore.TiersFor(s.Kind()), cachestore.Tier4) {
				g.logger.Warn("Disk guard: store exceeds its configured budget but its kind forbids a wholesale wipe, skipping",
					slog.String("store", s.Name()), slog.Any("kind", s.Kind()),
					slog.Uint64("used_bytes", used), slog.Uint64("budget_bytes", budget))
				continue
			}
			if _, err := s.Reclaim(ctx, cachestore.Tier4); err != nil {
				g.logger.Warn("Disk guard: budget wipe failed",
					slog.String("store", s.Name()), slog.Uint64("used_bytes", used),
					slog.Uint64("budget_bytes", budget), slog.Any("error", err))
			}
			continue
		}

		// "warn" (and the default "") never deletes data — fine-grained
		// eviction under the cap is left to the store's own tooling (e.g.
		// ccache's own max_size), the same way the docker-build-cache and
		// tart stores' Tier2 trims already handle their own BudgetGB.
		g.logger.Warn("Disk guard: store exceeds its configured budget",
			slog.String("store", s.Name()), slog.Uint64("used_bytes", used),
			slog.Uint64("budget_bytes", budget),
			slog.String("note", "fine-grained eviction is the tool's own responsibility"))
	}
}

// NeedsReclaim reports whether any store's filesystem is currently below
// MinFree. It is the cheap pre-job-start check: one statfs per distinct
// filesystem, no Measure calls (Measure can walk an entire volume) and no
// reclaiming — callers that get true back are expected to run Sweep
// themselves before proceeding.
//
// If statfs failed for every configured store, NeedsReclaim returns an
// error instead of a verdict, rather than silently reporting "false". This
// is the pre-job gate whose entire reason to exist is stopping a host from
// filling up mid-build, so reporting "healthy" when it actually has no
// idea would be the worst available failure mode — indistinguishable from
// the disk genuinely being fine. The bool returned alongside the error is
// true, not the zero value, as a second line of defense for a caller that
// inspects it without checking the error first. A partial failure — at
// least one filesystem still statfs-able — is unaffected: it still
// returns a real verdict from what it could read, exactly as before.
func (g *Guard) NeedsReclaim() (bool, error) {
	if !g.cfg.Enabled {
		return false, nil
	}
	groups := g.statByFilesystem()
	if len(g.stores) > 0 && len(groups) == 0 {
		return true, fmt.Errorf("statfs failed for all %d configured store(s): cannot tell whether free space is low", len(g.stores))
	}
	for _, grp := range groups {
		if grp.stat.FreeBytes < g.cfg.MinFree.BytesOf(grp.stat.TotalBytes) {
			return true, nil
		}
	}
	return false, nil
}
