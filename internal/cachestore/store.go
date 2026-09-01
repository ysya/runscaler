// Package cachestore models the reclaimable storage runner manages on a
// host — Docker images and build cache, named cache volumes, the shared
// volume, and Tart's image cache — behind one interface so the disk guard
// can reclaim from all of them without knowing their details.
package cachestore

import (
	"context"
	"fmt"
)

// StoreKind classifies a store by what happens when its contents vanish.
// The reclaim ladder is derived from this rather than hand-maintained, so a
// new store type cannot be filed into a tier that would destroy live data.
type StoreKind int

const (
	// KindGarbage is already-dead data: nothing observes its removal.
	KindGarbage StoreKind = iota
	// KindCache is regenerable: removal only costs time on the next job.
	KindCache
	// KindScratch is live handoff data between jobs of one workflow run.
	// Removing the un-expired portion breaks the run outright, so only its
	// TTL-expired portion is ever reclaimable.
	KindScratch
)

// Tier orders reclamation from free to expensive. The guard walks tiers in
// ascending order and stops as soon as the free-space target is met.
type Tier int

const (
	Tier1 Tier = iota + 1 // dangling images, stopped containers, buildx orphans
	Tier2                 // build cache and image layers past their max-age
	Tier3                 // shared-volume files past their max-age
	Tier4                 // wipe cache volumes — next build starts cold
)

// TiersFor returns the tiers at which a store of the given kind may be
// reclaimed. KindScratch deliberately excludes Tier4.
func TiersFor(k StoreKind) []Tier {
	switch k {
	case KindGarbage:
		return []Tier{Tier1}
	case KindCache:
		return []Tier{Tier2, Tier4}
	case KindScratch:
		return []Tier{Tier3}
	default:
		return nil
	}
}

// CacheStore is one reclaimable store on the host.
type CacheStore interface {
	Name() string
	Kind() StoreKind
	// Path is where this store's data lives, used to resolve which
	// filesystem its usage counts against.
	Path() string
	// Enabled reports whether the operator left this store's own routine
	// periodic cleanup on — consulted by that periodic sweep, not by the
	// disk guard (internal/diskguard.Guard), which reclaims from every
	// store regardless of Enabled() once the disk is actually under
	// pressure (revised 2026-08-14: Enabled() means "skip my own routine
	// schedule", not "never touch this even if the disk is full" — a host
	// whose disk the guard must never touch opts out wholesale via
	// `[disk] guard = false` instead).
	Enabled() bool
	// Measure reports current usage. May be expensive (it can walk a
	// volume), so it is never called on the pre-job check path.
	Measure(ctx context.Context) (uint64, error)
	// Reclaim frees what this store can release at the given tier and
	// reports the bytes freed. Tiers the store does not participate in are
	// a no-op returning 0.
	Reclaim(ctx context.Context, tier Tier) (uint64, error)
	// Budget reports this store's own operator-configured retention cap,
	// independent of the disk guard's tier ladder: bytes is the cap, and
	// onExceed is "wipe" or "warn" ("" behaves as "warn") for what the
	// guard does once usage exceeds it. A store with no such policy
	// returns (0, ""); the guard treats a zero budget as "not configured"
	// and never measures it for this purpose.
	Budget() (bytes uint64, onExceed string)
}

// serialized wraps store so that no two callers ever run its Measure or
// Reclaim at the same time. Every constructor in this package returns one,
// so the guarantee is a property of the store itself rather than of any one
// caller's discipline.
//
// The hazard is not hypothetical: cmd/runner hands the very same store
// instances to the periodic sweepers and to the disk guard, so a sweeper's
// ticker can fire while the guard is mid-sweep. diskguard.Guard's own
// sweepMu only serializes Sweep against Sweep and cannot see the sweepers at
// all. Concurrently, two helper containers would run `du; find -delete; du`
// over one volume — each measuring the other's deletions — and Docker's
// daemon-side prune lock would reject the second caller ("a prune operation
// is already running"), which the guard reads as "that tier freed nothing"
// and escalates past. Measure is covered by the same lock as Reclaim
// because interleaving them is what produces those bogus numbers.
//
// A second caller waits rather than returning zero-freed immediately. The
// callers here are a periodic sweeper doing its scheduled retention work and
// a guard measuring the result of each tier by re-running statfs: a caller
// that returned "freed 0, no error" without doing anything is
// indistinguishable from a tier that genuinely had nothing to give, so the
// guard would escalate to a more destructive tier on the strength of work
// that was merely skipped. Waiting is bounded — each store's own Reclaim
// already carries a 10-minute cap (volumeHelperTimeout, dockerReclaimTimeout)
// — and the wait itself honors ctx, so a caller on the job-start path is
// still released the moment its own deadline expires (see the timeout
// controller.startInstance wraps its pre-job Sweep in) rather than inheriting the
// holder's.
type serialized struct {
	CacheStore
	sem chan struct{} // capacity 1: acquiring is a context-aware Lock
}

// serialize returns store guarded by its own semaphore. Wrapping at
// construction (rather than at each call site) is what makes it impossible
// for a future caller to reach an unguarded Reclaim.
func serialize(store CacheStore) CacheStore {
	return &serialized{CacheStore: store, sem: make(chan struct{}, 1)}
}

// acquire takes the store's semaphore, or gives up if ctx ends first.
func (s *serialized) acquire(ctx context.Context) error {
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *serialized) release() { <-s.sem }

func (s *serialized) Measure(ctx context.Context) (uint64, error) {
	if err := s.acquire(ctx); err != nil {
		return 0, fmt.Errorf("measure %s: %w", s.Name(), err)
	}
	defer s.release()
	return s.CacheStore.Measure(ctx)
}

func (s *serialized) Reclaim(ctx context.Context, tier Tier) (uint64, error) {
	if err := s.acquire(ctx); err != nil {
		return 0, fmt.Errorf("reclaim %s: %w", s.Name(), err)
	}
	defer s.release()
	return s.CacheStore.Reclaim(ctx, tier)
}
