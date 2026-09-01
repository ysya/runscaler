package cachestore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	dockerclient "github.com/moby/moby/client"
)

// --- dockerGarbageStore ---

func TestDockerGarbageStore_Tier1PrunesContainersAndImages(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerGarbageStore(fake, DockerGarbageConfig{
		Enabled: true, PruneTTL: 24 * time.Hour,
	})

	if _, err := s.Reclaim(context.Background(), Tier1); err != nil {
		t.Fatalf("Reclaim(Tier1) error: %v", err)
	}
	if fake.containersPruneCalls != 1 || fake.imagesPruneCalls != 1 {
		t.Errorf("Tier1 should prune stopped containers and dangling images, got %d/%d",
			fake.containersPruneCalls, fake.imagesPruneCalls)
	}
}

func TestDockerGarbageStore_OnlyTier1(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerGarbageStore(fake, DockerGarbageConfig{Enabled: true, PruneTTL: 24 * time.Hour})

	for _, tier := range []Tier{Tier2, Tier3, Tier4} {
		freed, err := s.Reclaim(context.Background(), tier)
		if err != nil || freed != 0 {
			t.Errorf("Reclaim(%v) should be a no-op, got freed=%d err=%v", tier, freed, err)
		}
	}
	if fake.containersPruneCalls != 0 || fake.imagesPruneCalls != 0 {
		t.Error("Tier2/Tier3/Tier4 must not trigger any prune call — this store only owns Tier1")
	}
}

// TestDockerGarbageStore_ReclaimsRegardlessOfEnabled pins the 2026-08-14
// spec revision (see store.go's Enabled doc comment): Enabled()==false
// means the operator left the periodic prune sweep off, not "never touch
// this even under disk pressure" — the disk guard calls Reclaim on a
// disabled store exactly like an enabled one, so this method must not gate
// on cfg.Enabled. cmd/runner's sweeper is what still respects Enabled(): it
// never launches the goroutine that would call this method when disabled,
// a different code path from this one.
func TestDockerGarbageStore_ReclaimsRegardlessOfEnabled(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerGarbageStore(fake, DockerGarbageConfig{Enabled: false, PruneTTL: 24 * time.Hour})

	freed, err := s.Reclaim(context.Background(), Tier1)
	if err != nil {
		t.Fatalf("Reclaim error: %v", err)
	}
	_ = freed
	if fake.containersPruneCalls != 1 || fake.imagesPruneCalls != 1 {
		t.Errorf("a disabled store must still prune when Reclaim is called directly, got containers=%d images=%d",
			fake.containersPruneCalls, fake.imagesPruneCalls)
	}
}

// TestDockerGarbageStore_ContinuesAfterContainerPruneError pins that a
// ContainerPrune failure does not stop the ImagePrune attempt, and that the
// failure surfaces in the returned error — coverage that moved here from
// internal/provider's now-deleted TestPruneDockerRuntime_ContinuesAfterPruneErrors.
func TestDockerGarbageStore_ContinuesAfterContainerPruneError(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker", containerPruneErr: errors.New("containers boom")}
	s := NewDockerGarbageStore(fake, DockerGarbageConfig{Enabled: true, PruneTTL: 24 * time.Hour})

	_, err := s.Reclaim(context.Background(), Tier1)
	if err == nil || !strings.Contains(err.Error(), "containers boom") {
		t.Fatalf("Reclaim(Tier1) error = %v, want it to mention the container-prune failure", err)
	}
	if fake.imagesPruneCalls != 1 {
		t.Errorf("imagesPruneCalls = %d, want 1 — the image prune must still run after the container prune fails", fake.imagesPruneCalls)
	}
}

// TestDockerGarbageStore_NegativePruneTTLSkipsReclaim pins that a negative
// PruneTTL — the explicit "disable the container/image portion, keep
// build-cache retention" signal (see cmd/runner's dockerPruneSettingsFor
// and DockerConfig.PruneTTL's doc comment) — is treated the same as zero:
// both skip the prune entirely, regardless of Enabled().
func TestDockerGarbageStore_NegativePruneTTLSkipsReclaim(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerGarbageStore(fake, DockerGarbageConfig{Enabled: true, PruneTTL: -time.Hour})

	freed, err := s.Reclaim(context.Background(), Tier1)
	if err != nil {
		t.Fatalf("Reclaim(Tier1) error: %v", err)
	}
	if freed != 0 || fake.containersPruneCalls != 0 || fake.imagesPruneCalls != 0 {
		t.Error("a negative PruneTTL should skip the garbage prune entirely, same as zero")
	}
}

func TestDockerGarbageStore_ZeroPruneTTLSkipsReclaim(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerGarbageStore(fake, DockerGarbageConfig{Enabled: true}) // PruneTTL: 0

	freed, err := s.Reclaim(context.Background(), Tier1)
	if err != nil {
		t.Fatalf("Reclaim(Tier1) error: %v", err)
	}
	if freed != 0 || fake.containersPruneCalls != 0 || fake.imagesPruneCalls != 0 {
		t.Error("PruneTTL <= 0 should skip the garbage prune entirely")
	}
}

func TestDockerGarbageStore_NameKindPath(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/mnt/docker-data"}
	s := NewDockerGarbageStore(fake, DockerGarbageConfig{Enabled: true})

	if s.Name() != "docker-garbage" {
		t.Errorf("Name() = %q, want %q", s.Name(), "docker-garbage")
	}
	if s.Kind() != KindGarbage {
		t.Errorf("Kind() = %v, want KindGarbage — everything it removes is already unreferenced", s.Kind())
	}
	if got := s.Path(); got != "/mnt/docker-data" {
		t.Errorf("Path() = %q, want %q (from client.Info().DockerRootDir)", got, "/mnt/docker-data")
	}
}

func TestDockerGarbageStore_Measure(t *testing.T) {
	fake := &fakeDockerAPI{
		rootDir:   "/var/lib/docker",
		diskUsage: dockerclient.DiskUsageResult{Images: dockerclient.ImagesDiskUsage{TotalSize: 5_000_000_000}},
	}
	s := NewDockerGarbageStore(fake, DockerGarbageConfig{})

	got, err := s.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure() error: %v", err)
	}
	if want := uint64(5_000_000_000); got != want {
		t.Errorf("Measure() = %d, want %d", got, want)
	}
}

func TestDockerGarbageStore_Measure_IgnoresUnknownSentinel(t *testing.T) {
	fake := &fakeDockerAPI{
		rootDir:   "/var/lib/docker",
		diskUsage: dockerclient.DiskUsageResult{Images: dockerclient.ImagesDiskUsage{TotalSize: -1}},
	}
	s := NewDockerGarbageStore(fake, DockerGarbageConfig{})

	got, err := s.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure() error: %v", err)
	}
	if got != 0 {
		t.Errorf("Measure() = %d, want 0 (negative total must not wrap around)", got)
	}
}

// --- dockerBuildCacheStore ---

func TestDockerBuildCacheStore_Kind(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{Enabled: true})

	if s.Kind() != KindCache {
		t.Errorf("Kind() = %v, want KindCache — this is what makes Tier4 reachable at all "+
			"(TiersFor(KindGarbage) excludes Tier4; a KindGarbage store's Tier2/4 branches "+
			"would be dead code from the guard's perspective)", s.Kind())
	}
	if s.Name() != "docker-build-cache" {
		t.Errorf("Name() = %q, want %q", s.Name(), "docker-build-cache")
	}
}

// TestDockerBuildCacheStore_Tier2PrunesByAgeAndBudget pins that age and
// budget are two separate BuildCachePrune calls, mirroring
// PruneDockerRuntime's shape exactly rather than combining both into one
// call's options.
func TestDockerBuildCacheStore_Tier2PrunesByAgeAndBudget(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{
		Enabled: true, MaxAge: 7 * 24 * time.Hour, BudgetGB: 20,
	})

	if _, err := s.Reclaim(context.Background(), Tier2); err != nil {
		t.Fatalf("Reclaim(Tier2) error: %v", err)
	}

	const gb = int64(1024 * 1024 * 1024)
	want := []dockerclient.BuildCachePruneOptions{
		{All: true, Filters: make(dockerclient.Filters).Add("until", "168h0m0s")},
		{All: true, MaxUsedSpace: 20 * gb},
	}
	if !reflect.DeepEqual(fake.buildCachePruneOpts, want) {
		t.Errorf("build cache prune opts = %+v, want %+v", fake.buildCachePruneOpts, want)
	}
}

// TestDockerBuildCacheStore_ContinuesAfterAgePruneError pins that Tier2's
// age-based BuildCachePrune call failing does not stop the budget-based one
// from still running, and that the failure surfaces in the returned error
// — coverage that moved here from internal/provider's now-deleted
// TestPruneDockerRuntime_ContinuesAfterPruneErrors. Both calls share one
// injected error (fakeDockerAPI has no per-call error injection), but each
// is wrapped with a distinct prefix in production code ("prune build cache
// by age" vs "...to budget"), so the joined error's content still proves
// both were attempted and both failures are visible, not just the first.
func TestDockerBuildCacheStore_ContinuesAfterAgePruneError(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker", buildCachePruneErr: errors.New("build cache boom")}
	s := NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{
		Enabled: true, MaxAge: 7 * 24 * time.Hour, BudgetGB: 20,
	})

	_, err := s.Reclaim(context.Background(), Tier2)
	if err == nil {
		t.Fatal("Reclaim(Tier2) error = nil, want a joined error from both failed prune calls")
	}
	if !strings.Contains(err.Error(), "prune build cache by age") || !strings.Contains(err.Error(), "prune build cache to budget") {
		t.Errorf("Reclaim(Tier2) error = %v, want it to mention both the age and budget prune failures", err)
	}
	if fake.buildCachePruneCalls != 2 {
		t.Errorf("buildCachePruneCalls = %d, want 2 — the budget-based prune must still run after the age-based one fails", fake.buildCachePruneCalls)
	}
}

// TestDockerBuildCacheStore_Tier4WipesUnconditionally is the distinction
// the whole store split exists to make reachable: Tier4 passes neither a
// Filters value nor MaxUsedSpace, regardless of MaxAge/BudgetGB — it is the
// guard's emergency full wipe, not a stricter version of the Tier2 trim.
func TestDockerBuildCacheStore_Tier4WipesUnconditionally(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{
		Enabled: true, MaxAge: 7 * 24 * time.Hour, BudgetGB: 20,
	})

	if _, err := s.Reclaim(context.Background(), Tier4); err != nil {
		t.Fatalf("Reclaim(Tier4) error: %v", err)
	}
	if fake.buildCachePruneCalls != 1 {
		t.Fatalf("Tier4 should issue exactly one BuildCachePrune call, got %d", fake.buildCachePruneCalls)
	}
	opts := fake.buildCachePruneOpts[0]
	if !opts.All {
		t.Error("Tier4 wipe must set All: true")
	}
	if len(opts.Filters) != 0 {
		t.Errorf("Tier4 wipe must pass no Filters, got %+v", opts.Filters)
	}
	if opts.MaxUsedSpace != 0 {
		t.Errorf("Tier4 wipe must pass no MaxUsedSpace, got %d", opts.MaxUsedSpace)
	}
}

func TestDockerBuildCacheStore_Tier1AndTier3AreNoOps(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{
		Enabled: true, MaxAge: time.Hour, BudgetGB: 10,
	})

	for _, tier := range []Tier{Tier1, Tier3} {
		freed, err := s.Reclaim(context.Background(), tier)
		if err != nil || freed != 0 {
			t.Errorf("Reclaim(%v) should be a no-op, got freed=%d err=%v", tier, freed, err)
		}
	}
	if fake.containersPruneCalls != 0 || fake.imagesPruneCalls != 0 || fake.buildCachePruneCalls != 0 {
		t.Error("Tier1/Tier3 must not trigger any prune call — this store only owns Tier2/Tier4")
	}
}

// TestDockerBuildCacheStore_ReclaimsRegardlessOfEnabled pins the 2026-08-14
// spec revision — see TestDockerGarbageStore_ReclaimsRegardlessOfEnabled's
// identical rationale, which applies here too (both stores are gated by the
// same [docker] prune switch).
func TestDockerBuildCacheStore_ReclaimsRegardlessOfEnabled(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{
		Enabled: false, MaxAge: time.Hour, BudgetGB: 10,
	})

	for _, tier := range []Tier{Tier2, Tier4} {
		if _, err := s.Reclaim(context.Background(), tier); err != nil {
			t.Fatalf("Reclaim(%v) error: %v", tier, err)
		}
	}
	// Tier2 issues two calls (age + budget, see
	// TestDockerBuildCacheStore_Tier2PrunesByAgeAndBudget) and Tier4 issues
	// one more (the unconditional wipe) — three total, none skipped for
	// being disabled.
	if fake.buildCachePruneCalls != 3 {
		t.Errorf("buildCachePruneCalls = %d, want 3 — a disabled store must still prune when Reclaim is called directly", fake.buildCachePruneCalls)
	}
}

func TestDockerBuildCacheStore_ZeroAgeAndBudgetSkipsTier2(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{Enabled: true}) // MaxAge/BudgetGB both 0

	freed, err := s.Reclaim(context.Background(), Tier2)
	if err != nil {
		t.Fatalf("Reclaim(Tier2) error: %v", err)
	}
	if freed != 0 || fake.buildCachePruneCalls != 0 {
		t.Error("MaxAge <= 0 and BudgetGB <= 0 should skip the Tier2 trim entirely")
	}
}

func TestDockerBuildCacheStore_Measure(t *testing.T) {
	fake := &fakeDockerAPI{
		rootDir:   "/var/lib/docker",
		diskUsage: dockerclient.DiskUsageResult{BuildCache: dockerclient.BuildCacheDiskUsage{TotalSize: 3_000_000_000}},
	}
	s := NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{})

	got, err := s.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure() error: %v", err)
	}
	if want := uint64(3_000_000_000); got != want {
		t.Errorf("Measure() = %d, want %d", got, want)
	}
}

func TestDockerBuildCacheStore_PathResolvesDockerRootDir(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/mnt/docker-data"}
	s := NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{})

	if got := s.Path(); got != "/mnt/docker-data" {
		t.Errorf("Path() = %q, want %q (from client.Info().DockerRootDir)", got, "/mnt/docker-data")
	}
}
