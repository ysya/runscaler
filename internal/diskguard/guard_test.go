package diskguard

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

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

func TestGuard_SkipsDisabledStore(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 1}, nil
	}
	disabled := &fakeStore{name: "docker", kind: cachestore.KindGarbage, path: "/", disabled: true}
	// Control: a second, enabled store of the same kind in the same sweep.
	// Without it, disabled.reclaimedTiers being empty would be equally
	// consistent with a guard that reclaims from nothing at all — this
	// proves the guard actively distinguishes disabled from enabled rather
	// than merely doing nothing here.
	enabled := &fakeStore{name: "buildx", kind: cachestore.KindGarbage, path: "/"}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier4},
		[]cachestore.CacheStore{disabled, enabled}, statFn, slog.New(slog.DiscardHandler))

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	if len(disabled.reclaimedTiers) != 0 {
		t.Error("a disabled store must not be reclaimed — the operator turned it off deliberately")
	}
	want := []cachestore.Tier{cachestore.Tier1}
	if !slices.Equal(enabled.reclaimedTiers, want) {
		t.Errorf("enabled control store reclaimedTiers = %v, want %v — an enabled store of the same kind in the same sweep must still be reclaimed",
			enabled.reclaimedTiers, want)
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

// TestGuard_ShortfallWarningNamesDisabledStores is not one of the brief's
// given tests — added because none of them can check log content
// (slog.DiscardHandler discards it), yet property 3 ("say in the warning
// which stores it could not touch and why") is specifically about that
// content. Uses a real handler over a buffer instead.
func TestGuard_ShortfallWarningNamesDisabledStores(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 1}, nil // never healthy
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	s := &fakeStore{name: "docker-garbage", kind: cachestore.KindGarbage, path: "/", disabled: true}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier1},
		[]cachestore.CacheStore{s}, statFn, logger)

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "docker-garbage") {
		t.Errorf("shortfall warning does not name the disabled store it could not reclaim from: %s", out)
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
