// Package cachestore models the reclaimable storage runner manages on a
// host — Docker images and build cache, named cache volumes, the shared
// volume, and Tart's image cache — behind one interface so the disk guard
// can reclaim from all of them without knowing their details.
package cachestore

import "context"

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
	// Enabled reports whether the operator left this store's cleanup on.
	// The guard skips disabled stores rather than overriding the choice.
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
