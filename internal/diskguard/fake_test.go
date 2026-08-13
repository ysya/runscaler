package diskguard

import (
	"context"

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

// mustThreshold parses s and panics on error — a test-only helper so
// table-driven test setup can stay a one-liner instead of threading `t` and
// `t.Fatalf` through every threshold literal.
func mustThreshold(s string) Threshold {
	th, err := ParseThreshold(s)
	if err != nil {
		panic(err)
	}
	return th
}
