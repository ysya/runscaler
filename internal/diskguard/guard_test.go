package diskguard

import (
	"bytes"
	"context"
	"log/slog"
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
	s := &fakeStore{name: "docker", kind: cachestore.KindGarbage, path: "/", disabled: true}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier4},
		[]cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	if len(s.reclaimedTiers) != 0 {
		t.Error("a disabled store must not be reclaimed — the operator turned it off deliberately")
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
	for _, tier := range s.reclaimedTiers {
		if tier > cachestore.Tier3 {
			t.Fatalf("guard reclaimed at %v, above MaxTier=Tier3", tier)
		}
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
