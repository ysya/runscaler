package cachestore

import (
	"context"
	"reflect"
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

func TestDockerGarbageStore_DisabledReclaimsNothing(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerGarbageStore(fake, DockerGarbageConfig{Enabled: false, PruneTTL: 24 * time.Hour})

	freed, err := s.Reclaim(context.Background(), Tier1)
	if err != nil {
		t.Fatalf("Reclaim error: %v", err)
	}
	if freed != 0 || fake.containersPruneCalls != 0 {
		t.Error("a disabled store must not prune — the operator turned it off deliberately")
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

func TestDockerBuildCacheStore_DisabledReclaimsNothing(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{
		Enabled: false, MaxAge: time.Hour, BudgetGB: 10,
	})

	for _, tier := range []Tier{Tier2, Tier4} {
		freed, err := s.Reclaim(context.Background(), tier)
		if err != nil {
			t.Fatalf("Reclaim(%v) error: %v", tier, err)
		}
		if freed != 0 {
			t.Errorf("Reclaim(%v) = %d, want 0", tier, freed)
		}
	}
	if fake.buildCachePruneCalls != 0 {
		t.Error("a disabled store must not prune — the operator turned it off deliberately")
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
