package cachestore

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// fakeCommandRunner implements backend.CommandRunner for tartStore's tests.
// Kept to what tartStore.Measure actually needs — one canned Run() result
// plus its last invocation — rather than mirroring
// internal/backend/tart_test.go's fuller mockCommandRunner (multi-call
// history, prefix matching): PruneTartCache's own command construction is
// already covered by that package's tests, and tartStore.Reclaim(Tier2)
// delegates to the exported backend.PruneTartCache wrapper directly (it
// builds its own execCommandRunner internally), so this double is never
// consulted for Reclaim at all — only for Measure's direct `du` call.
type fakeCommandRunner struct {
	output []byte
	err    error

	lastName string
	lastArgs []string
}

func (f *fakeCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.lastName = name
	f.lastArgs = args
	return f.output, f.err
}

func (f *fakeCommandRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	_, err := f.Run(ctx, name, args...)
	return err
}

func TestTartStore_PathFollowsTartHome(t *testing.T) {
	s := NewTartStore(nil, TartConfig{Enabled: true, Home: "/Volumes/FrankData/tart"})
	if s.Path() != "/Volumes/FrankData/tart" {
		t.Errorf("Path() = %q, want the configured TART_HOME — usage counts "+
			"against that filesystem, not the system disk", s.Path())
	}
}

// TestTartStore_PathFallsBackToDefaultTartHome pins the correctness point
// specific to this task: an empty Home must resolve to tart's own default
// ($HOME/.tart), never the system disk and never an empty string.
func TestTartStore_PathFallsBackToDefaultTartHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	s := NewTartStore(nil, TartConfig{Enabled: true})
	want := filepath.Join(tmp, ".tart")
	if got := s.Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestTartStore_NameKindEnabled(t *testing.T) {
	s := NewTartStore(nil, TartConfig{Enabled: true, Home: "/Volumes/FrankData/tart"})
	if s.Name() != "tart-cache" {
		t.Errorf("Name() = %q, want %q", s.Name(), "tart-cache")
	}
	if s.Kind() != KindCache {
		t.Errorf("Kind() = %v, want KindCache", s.Kind())
	}
	if !s.Enabled() {
		t.Error("Enabled() = false, want true")
	}
}

// TestTartStore_Tier4NeverWipesWholesale pins the second deliberate-zero
// decision the brief calls out: re-pulling a 140 GB macOS image is far more
// expensive than any disk pressure a wholesale wipe would relieve, and
// tart prune (Tier2) already applies its own LRU.
func TestTartStore_Tier4NeverWipesWholesale(t *testing.T) {
	s := NewTartStore(nil, TartConfig{
		Enabled: true, Home: "/Volumes/FrankData/tart", MaxAge: 24 * time.Hour, BudgetGB: 50,
	})

	freed, err := s.Reclaim(context.Background(), Tier4)
	if err != nil || freed != 0 {
		t.Errorf("Reclaim(Tier4) = (%d, %v), want (0, nil) — Tart images are never wiped wholesale", freed, err)
	}
}

func TestTartStore_Tier1AndTier3AreNoOps(t *testing.T) {
	s := NewTartStore(nil, TartConfig{
		Enabled: true, Home: "/Volumes/FrankData/tart", MaxAge: 24 * time.Hour, BudgetGB: 50,
	})

	for _, tier := range []Tier{Tier1, Tier3} {
		freed, err := s.Reclaim(context.Background(), tier)
		if err != nil || freed != 0 {
			t.Errorf("Reclaim(%v) should be a no-op, got freed=%d err=%v", tier, freed, err)
		}
	}
}

func TestTartStore_DisabledReclaimsNothing(t *testing.T) {
	s := NewTartStore(nil, TartConfig{
		Enabled: false, Home: "/Volumes/FrankData/tart", MaxAge: 24 * time.Hour, BudgetGB: 50,
	})

	for _, tier := range []Tier{Tier2, Tier4} {
		freed, err := s.Reclaim(context.Background(), tier)
		if err != nil || freed != 0 {
			t.Errorf("Reclaim(%v) should be a no-op when disabled, got freed=%d err=%v", tier, freed, err)
		}
	}
}

// TestTartStore_Tier2DelegatesToPruneTartCacheWithoutShellingOut proves
// Reclaim(Tier2) is wired to backend.PruneTartCache without actually
// invoking the `tart` binary: PruneTartCache itself no-ops (returns nil
// before running any command) when both MaxAge and BudgetGB are <= 0 — the
// one Tier2 scenario safely exercisable from a unit test. This machine (and
// any other with tart installed) must never have `go test` trigger a real
// `tart prune`; PruneTartCache's actual command construction is already
// covered by internal/backend/tart_test.go against an injected
// CommandRunner, which this package has no access to (pruneTartCacheWith is
// unexported).
func TestTartStore_Tier2DelegatesToPruneTartCacheWithoutShellingOut(t *testing.T) {
	s := NewTartStore(nil, TartConfig{Enabled: true, Home: "/Volumes/FrankData/tart"}) // MaxAge/BudgetGB both 0

	freed, err := s.Reclaim(context.Background(), Tier2)
	if err != nil {
		t.Fatalf("Reclaim(Tier2) error: %v", err)
	}
	if freed != 0 {
		t.Errorf("Reclaim(Tier2) = %d, want 0", freed)
	}
}

func TestTartStore_Measure(t *testing.T) {
	runner := &fakeCommandRunner{output: []byte("123456\t/Volumes/FrankData/tart/cache\n")}
	s := NewTartStore(runner, TartConfig{Enabled: true, Home: "/Volumes/FrankData/tart"})

	got, err := s.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure() error: %v", err)
	}
	if got != 123456 {
		t.Errorf("Measure() = %d, want 123456", got)
	}
	if runner.lastName != "du" {
		t.Errorf("Measure() ran %q, want du", runner.lastName)
	}
	wantArgs := []string{"-sb", "/Volumes/FrankData/tart/cache"}
	if !reflect.DeepEqual(runner.lastArgs, wantArgs) {
		t.Errorf("Measure() args = %v, want %v", runner.lastArgs, wantArgs)
	}
}

func TestTartStore_Measure_UsesDefaultHomeWhenUnset(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	runner := &fakeCommandRunner{output: []byte("42\t/cache\n")}
	s := NewTartStore(runner, TartConfig{Enabled: true})

	if _, err := s.Measure(context.Background()); err != nil {
		t.Fatalf("Measure() error: %v", err)
	}
	want := filepath.Join(tmp, ".tart", "cache")
	if len(runner.lastArgs) != 2 || runner.lastArgs[1] != want {
		t.Errorf("Measure() args = %v, want [-sb %s]", runner.lastArgs, want)
	}
}

func TestTartStore_Measure_PropagatesParseError(t *testing.T) {
	runner := &fakeCommandRunner{output: []byte("not a number\n")}
	s := NewTartStore(runner, TartConfig{Enabled: true, Home: "/Volumes/FrankData/tart"})

	if _, err := s.Measure(context.Background()); err == nil {
		t.Error("Measure() error = nil, want a parse error for unparseable du output")
	}
}

func TestTartStore_Measure_PropagatesRunError(t *testing.T) {
	runner := &fakeCommandRunner{err: context.DeadlineExceeded}
	s := NewTartStore(runner, TartConfig{Enabled: true, Home: "/Volumes/FrankData/tart"})

	if _, err := s.Measure(context.Background()); err == nil {
		t.Error("Measure() error = nil, want the du command's error propagated")
	}
}
