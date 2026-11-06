package diskguard

import (
	"context"
	"sync"

	"github.com/ysya/runscaler/internal/bytesize"
	"github.com/ysya/runscaler/internal/cachestore"
)

// fakeStore is a minimal cachestore.CacheStore double used only by this
// package's own tests. It records every tier Reclaim was called with
// (reclaimedTiers) and how many times Measure was called (measureCalls), so
// a test can assert on the guard's call pattern directly instead of
// inferring it from side effects a real store would have.
type fakeStore struct {
	name     string
	kind     cachestore.StoreKind
	path     string
	disabled bool

	// size, budget and onExceed back Measure/Budget for the budget-
	// enforcement tests: Measure reports size, Budget reports (budget,
	// onExceed) verbatim — mirroring how cacheVolumeStore carries
	// BudgetBytes/OnExceed straight from its config (see volume.go).
	size     uint64
	budget   uint64
	onExceed string

	reclaimedTiers []cachestore.Tier
	measureCalls   int
}

func (f *fakeStore) Name() string               { return f.name }
func (f *fakeStore) Kind() cachestore.StoreKind { return f.kind }
func (f *fakeStore) Path() string               { return f.path }
func (f *fakeStore) Enabled() bool              { return !f.disabled }

func (f *fakeStore) Measure(_ context.Context) (uint64, error) {
	f.measureCalls++
	return f.size, nil
}

func (f *fakeStore) Reclaim(_ context.Context, tier cachestore.Tier) (uint64, error) {
	f.reclaimedTiers = append(f.reclaimedTiers, tier)
	return 0, nil
}

func (f *fakeStore) Budget() (uint64, string) { return f.budget, f.onExceed }

// blockingStore is a cachestore.CacheStore double whose Reclaim blocks
// until the test releases it, used only by
// TestGuard_SweepDoesNotRunConcurrently to hold a Sweep "in progress" for
// as long as the test needs — deterministically, via channels, rather than
// a sleep — so a second, concurrent Sweep call can be raced against it.
// Budget() deliberately returns (0, "") so enforceBudgets skips this store
// entirely (no Measure call to also coordinate); every entry/exit of
// Reclaim is recorded under mu so the test can assert no two ever overlap.
type blockingStore struct {
	name string
	kind cachestore.StoreKind
	path string

	entered chan struct{} // Reclaim sends here right after it starts, so the test can wait for "now in progress"
	release chan struct{} // Reclaim blocks reading this until the test closes it

	mu        sync.Mutex
	active    int // number of Reclaim calls currently in flight
	maxActive int // high-water mark of active, across the whole test
	reclaimed int // total completed Reclaim calls
}

func (b *blockingStore) Name() string                            { return b.name }
func (b *blockingStore) Kind() cachestore.StoreKind              { return b.kind }
func (b *blockingStore) Path() string                            { return b.path }
func (b *blockingStore) Enabled() bool                           { return true }
func (b *blockingStore) Measure(context.Context) (uint64, error) { return 0, nil }
func (b *blockingStore) Budget() (uint64, string)                { return 0, "" }

func (b *blockingStore) Reclaim(_ context.Context, _ cachestore.Tier) (uint64, error) {
	b.mu.Lock()
	b.active++
	if b.active > b.maxActive {
		b.maxActive = b.active
	}
	b.mu.Unlock()

	b.entered <- struct{}{}
	<-b.release

	b.mu.Lock()
	b.active--
	b.reclaimed++
	b.mu.Unlock()
	return 0, nil
}

// mustThreshold parses s and panics on error — a test-only helper so
// table-driven test setup can stay a one-liner instead of threading `t` and
// `t.Fatalf` through every threshold literal.
func mustThreshold(s string) bytesize.Threshold {
	th, err := bytesize.ParseThreshold(s)
	if err != nil {
		panic(err)
	}
	return th
}
