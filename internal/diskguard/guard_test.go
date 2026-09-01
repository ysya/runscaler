package diskguard

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/cachestore"
)

func TestGuard_StopsAsSoonAsTargetMet(t *testing.T) {
	// 第一次 statfs 低於 min-free,Tier1 回收後即達標 → 不應走到 Tier2。
	calls := 0
	statFn := func(string) (FSStat, error) {
		calls++
		if calls == 1 {
			return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 5}, nil
		}
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 30}, nil
	}
	s := &fakeStore{name: "garbage", kind: cachestore.KindGarbage, path: "/"}
	g := New(Config{
		Enabled: true,
		MinFree: mustThreshold("10%"), TargetFree: mustThreshold("20%"),
		MaxTier: cachestore.Tier3,
	}, []cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	if got := s.reclaimedTiers; len(got) != 1 || got[0] != cachestore.Tier1 {
		t.Errorf("reclaimed tiers = %v, want only Tier1 — the guard must stop once the target is met", got)
	}
}

// TestGuard_ReclaimsFromDisabledStore pins the 2026-08-14 spec revision
// (docs/superpowers/specs/2026-08-13-cache-architecture-design.md,
// "各 store 的啟用開關只約束例行清理"): Enabled()==false means the operator left
// this store off its own periodic sweep schedule ("don't run routine
// cleanup"), not "never touch this even if the disk is full". The guard
// must reclaim from a disabled store exactly like an enabled one of the
// same kind — a host whose disk the guard must never touch at all should
// set `[disk] guard = false` (Config.Enabled, the guard-wide switch)
// instead, which is a deliberate, explicit opt-out rather than one
// borrowed from a setting designed for something else.
func TestGuard_ReclaimsFromDisabledStore(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 1}, nil
	}
	disabled := &fakeStore{name: "docker", kind: cachestore.KindGarbage, path: "/", disabled: true}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier1},
		[]cachestore.CacheStore{disabled}, statFn, slog.New(slog.DiscardHandler))

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	want := []cachestore.Tier{cachestore.Tier1}
	if !slices.Equal(disabled.reclaimedTiers, want) {
		t.Errorf("reclaimedTiers = %v, want %v — the guard must reclaim from a disabled store under disk pressure",
			disabled.reclaimedTiers, want)
	}
}

// TestGuard_TiersForGateStillAppliesRegardlessOfEnabled pins that the
// Enabled() change above did not loosen TiersFor — a KindScratch store
// (live inter-job handoff data) must still be unreachable at Tier4 whether
// or not it is enabled. This is a data-safety rule, not an operator
// preference, so it is orthogonal to Enabled() entirely.
func TestGuard_TiersForGateStillAppliesRegardlessOfEnabled(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 1}, nil // never healthy
	}
	s := &fakeStore{name: "shared-volume", kind: cachestore.KindScratch, path: "/", disabled: true}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier4},
		[]cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	// TiersFor(KindScratch) == {Tier3}; Tier4 must never be attempted even
	// though MaxTier permits it and the store is disabled (irrelevant either
	// way now — this proves TiersFor, not Enabled(), is what still gates it).
	want := []cachestore.Tier{cachestore.Tier3}
	if !slices.Equal(s.reclaimedTiers, want) {
		t.Errorf("reclaimedTiers = %v, want %v — TiersFor(KindScratch) must still exclude Tier4 regardless of Enabled()",
			s.reclaimedTiers, want)
	}
}

func TestGuard_NeverExceedsMaxTier(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 1}, nil // 永遠不達標
	}
	s := &fakeStore{name: "cache", kind: cachestore.KindCache, path: "/"}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier3},
		[]cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	_ = g.Sweep(context.Background())
	// KindCache participates in {Tier2, Tier4} (see TiersFor); with
	// MaxTier=Tier3, only Tier2 is both applicable and in range. Asserting
	// the exact slice — not just an upper bound — proves the ladder
	// actually ran and stopped at MaxTier, not merely that it never
	// overran while doing nothing at all.
	want := []cachestore.Tier{cachestore.Tier2}
	if !slices.Equal(s.reclaimedTiers, want) {
		t.Fatalf("reclaimedTiers = %v, want %v — guard must reclaim at every applicable tier up to MaxTier=Tier3, never above it", s.reclaimedTiers, want)
	}
}

func TestGuard_NeedsReclaimDoesNotMeasure(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 5}, nil
	}
	s := &fakeStore{name: "cache", kind: cachestore.KindCache, path: "/"}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier3},
		[]cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	need, err := g.NeedsReclaim()
	if err != nil || !need {
		t.Fatalf("NeedsReclaim() = %v, %v; want true, nil", need, err)
	}
	if s.measureCalls != 0 {
		t.Error("NeedsReclaim must stay on the cheap path — Measure() may walk a whole volume")
	}
}

// TestGuard_NeedsReclaimErrorsWhenAllStatfsFail is a fix-round addition
// (not one of the brief's given tests): when statfs fails for every
// configured store, NeedsReclaim must return an error rather than the
// silent (false, nil) it used to — this is the pre-job gate whose entire
// purpose is stopping a host from filling up mid-build, so reporting
// "healthy" while actually having no idea is the worst available failure
// mode.
func TestGuard_NeedsReclaimErrorsWhenAllStatfsFail(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{}, errors.New("statfs boom")
	}
	s := &fakeStore{name: "cache", kind: cachestore.KindCache, path: "/"}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier3},
		[]cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	if _, err := g.NeedsReclaim(); err == nil {
		t.Fatal("NeedsReclaim() error = nil, want non-nil when statfs fails for every store — " +
			"silently reporting \"healthy\" would fail the pre-job gate open")
	}
}

// TestGuard_NeedsReclaimUsesReadableFilesystemWhenOneFails pins the other
// half of the same fix: a partial failure (at least one filesystem still
// readable) must still produce a real verdict from what it could read,
// not degrade to an error just because some other store's path failed.
func TestGuard_NeedsReclaimUsesReadableFilesystemWhenOneFails(t *testing.T) {
	statFn := func(path string) (FSStat, error) {
		if path == "/bad" {
			return FSStat{}, errors.New("statfs boom")
		}
		return FSStat{ID: "fs-good", TotalBytes: 100, FreeBytes: 5}, nil
	}
	good := &fakeStore{name: "good", kind: cachestore.KindCache, path: "/good"}
	bad := &fakeStore{name: "bad", kind: cachestore.KindCache, path: "/bad"}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier3},
		[]cachestore.CacheStore{good, bad}, statFn, slog.New(slog.DiscardHandler))

	need, err := g.NeedsReclaim()
	if err != nil {
		t.Fatalf("NeedsReclaim() error = %v, want nil — one readable filesystem should still produce a verdict", err)
	}
	if !need {
		t.Error("NeedsReclaim() = false, want true — the readable filesystem (5/100 free) is below MinFree (10%)")
	}
}

// TestGuard_ShortfallWarningReportsShortfall checks the shortfall warning's
// actual log content (slog.DiscardHandler, used by every other test here,
// discards it) rather than just that Sweep didn't error. Since the
// 2026-08-14 revision (see TestGuard_ReclaimsFromDisabledStore), the
// warning no longer names disabled stores — that mechanism is gone — but it
// must still fire and report the shortfall once every allowed tier has run
// and the target is still unmet.
func TestGuard_ShortfallWarningReportsShortfall(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 1}, nil // never healthy
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	// Enabled() irrelevant to reaching this warning post-revision — set
	// disabled anyway to confirm reclaim (and therefore the eventual
	// shortfall) still happens for it as it would for any other store.
	s := &fakeStore{name: "docker-garbage", kind: cachestore.KindGarbage, path: "/", disabled: true}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier1},
		[]cachestore.CacheStore{s}, statFn, logger)

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "reached max-tier without meeting target-free") {
		t.Errorf("shortfall warning did not fire: %s", out)
	}
	if !strings.Contains(out, "shortfall_bytes") {
		t.Errorf("shortfall warning does not report shortfall_bytes: %s", out)
	}
	if strings.Contains(out, "disabled_stores") || strings.Contains(out, "disabled_reason") {
		t.Errorf("shortfall warning still mentions disabled stores — that mechanism was removed: %s", out)
	}
}

func TestGuard_EnforcesBudgetEvenWhenDiskHealthy(t *testing.T) {
	// 磁碟充裕 → tier ladder 完全不該啟動
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 90}, nil
	}
	over := &fakeStore{
		name: "ccache", kind: cachestore.KindCache, path: "/",
		size: 25 << 30, budget: 20 << 30, onExceed: "wipe",
	}
	under := &fakeStore{
		name: "gradle", kind: cachestore.KindCache, path: "/",
		size: 1 << 30, budget: 20 << 30, onExceed: "wipe",
	}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier3},
		[]cachestore.CacheStore{over, under}, statFn, slog.New(slog.DiscardHandler))

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	if len(over.reclaimedTiers) != 1 || over.reclaimedTiers[0] != cachestore.Tier4 {
		t.Errorf("over-budget store should be wiped regardless of disk pressure, got %v",
			over.reclaimedTiers)
	}
	if len(under.reclaimedTiers) != 0 {
		t.Errorf("under-budget store must be left alone, got %v", under.reclaimedTiers)
	}
}

func TestGuard_BudgetWarnDoesNotTouchData(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 90}, nil
	}
	over := &fakeStore{
		name: "ccache", kind: cachestore.KindCache, path: "/",
		size: 25 << 30, budget: 20 << 30, onExceed: "warn",
	}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier3},
		[]cachestore.CacheStore{over}, statFn, slog.New(slog.DiscardHandler))

	_ = g.Sweep(context.Background())
	if len(over.reclaimedTiers) != 0 {
		t.Error(`on-exceed="warn" must never delete data — the tool's own LRU handles it`)
	}
}

// TestGuard_BudgetWipeRespectsTiersForGate is a fix-round addition (not
// one of the brief's given tests): the budget wipe must still be gated by
// TiersFor, the same kind-based rule every tier-ladder Reclaim call goes
// through. TiersFor(KindScratch) excludes Tier4 specifically to protect an
// in-flight workflow's un-expired handoff data (see
// TestScratchNeverReclaimedWholesale in cachestore) — a KindScratch store
// that grows a budget must not have that guarantee bypassed just because
// it went over budget.
func TestGuard_BudgetWipeRespectsTiersForGate(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 90}, nil // healthy — tier ladder must not run
	}
	s := &fakeStore{
		name: "shared-volume", kind: cachestore.KindScratch, path: "/",
		size: 25 << 30, budget: 20 << 30, onExceed: "wipe",
	}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier3},
		[]cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	// Disk stays healthy throughout, so the tier ladder never runs at all —
	// the only code path that could call Reclaim here is the budget wipe.
	// An empty reclaimedTiers therefore proves the TiersFor gate blocked
	// it, not just that Tier4 specifically was avoided.
	if len(s.reclaimedTiers) != 0 {
		t.Errorf("reclaimedTiers = %v, want none — TiersFor(KindScratch) excludes Tier4, so the budget wipe must be skipped entirely, not routed through Reclaim",
			s.reclaimedTiers)
	}
}

// TestGuard_SweepDoesNotRunConcurrently is a fix-round addition (Task 9
// review): since controller.WithDiskChecker landed, this Guard's Sweep can be
// called from several goroutines at once (every scale set's
// pre-job-start check, plus the periodic ticker, all sharing one
// instance) — see Sweep's own doc comment. This test drives Sweep from
// several goroutines concurrently against a store that deliberately blocks
// inside Reclaim until released, and asserts both halves of the
// TryLock/return-immediately design: no two Reclaim calls ever overlap,
// and none of the concurrent callers queues behind the in-progress one —
// they all return right away instead.
func TestGuard_SweepDoesNotRunConcurrently(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 1}, nil // always below MinFree
	}
	s := &blockingStore{
		name: "docker-garbage", kind: cachestore.KindGarbage, path: "/",
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier1},
		[]cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	// Start a first sweep and let it run until it's inside Reclaim — at
	// that point it holds sweepMu and is blocked until this test releases
	// it below.
	firstDone := make(chan error, 1)
	go func() { firstDone <- g.Sweep(context.Background()) }()
	<-s.entered

	// Several more Sweep calls race in while the first is still in
	// progress. Each must return immediately (nil, no wait) rather than
	// queue behind it — that is the whole point of TryLock over Lock here.
	const extra = 5
	othersDone := make(chan error, extra)
	for range extra {
		go func() { othersDone <- g.Sweep(context.Background()) }()
	}
	for range extra {
		select {
		case err := <-othersDone:
			if err != nil {
				t.Errorf("concurrent Sweep() error = %v, want nil", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a concurrent Sweep() did not return promptly — it must not queue behind an in-progress sweep")
		}
	}

	// Only now release the first sweep, having already proven the other
	// five didn't wait for this.
	close(s.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Sweep() error = %v, want nil", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxActive > 1 {
		t.Errorf("max concurrent Reclaim calls = %d, want at most 1 — Sweep must never run concurrently with itself", s.maxActive)
	}
	if s.reclaimed != 1 {
		t.Errorf("Reclaim invocations = %d, want exactly 1 — every concurrent Sweep call must skip the work entirely, not repeat it", s.reclaimed)
	}
}
