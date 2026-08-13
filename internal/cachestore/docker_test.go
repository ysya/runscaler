package cachestore

import (
	"context"
	"reflect"
	"testing"
	"time"

	dockerclient "github.com/moby/moby/client"
)

func TestDockerDaemonStore_Tier1PrunesGarbageOnly(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{
		Enabled: true, PruneTTL: 24 * time.Hour,
	})

	if _, err := s.Reclaim(context.Background(), Tier1); err != nil {
		t.Fatalf("Reclaim(Tier1) error: %v", err)
	}
	if fake.containersPruneCalls != 1 || fake.imagesPruneCalls != 1 {
		t.Errorf("Tier1 should prune stopped containers and dangling images, got %d/%d",
			fake.containersPruneCalls, fake.imagesPruneCalls)
	}
	if fake.buildCachePruneCalls != 0 {
		t.Errorf("Tier1 must not touch build cache, got %d calls", fake.buildCachePruneCalls)
	}
}

func TestDockerDaemonStore_DisabledReclaimsNothing(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{Enabled: false, PruneTTL: 24 * time.Hour})

	freed, err := s.Reclaim(context.Background(), Tier1)
	if err != nil {
		t.Fatalf("Reclaim error: %v", err)
	}
	if freed != 0 || fake.containersPruneCalls != 0 {
		t.Error("a disabled store must not prune — the operator turned it off deliberately")
	}
}

// TestDockerDaemonStore_PathResolvesDockerRootDir pins that Path() comes
// from the daemon's Info(), not a config field — DockerDaemonConfig
// deliberately has no RootDir field (see task-2-report.md deviations).
func TestDockerDaemonStore_PathResolvesDockerRootDir(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/mnt/docker-data"}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{})

	if got := s.Path(); got != "/mnt/docker-data" {
		t.Errorf("Path() = %q, want %q (from client.Info().DockerRootDir)", got, "/mnt/docker-data")
	}
}

func TestDockerDaemonStore_NameKindEnabled(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{Enabled: true})

	if s.Name() != "docker-daemon" {
		t.Errorf("Name() = %q, want %q", s.Name(), "docker-daemon")
	}
	if s.Kind() != KindGarbage {
		t.Errorf("Kind() = %v, want KindGarbage — everything it removes is already unreferenced", s.Kind())
	}
	if !s.Enabled() {
		t.Error("Enabled() = false, want true")
	}
}

// TestDockerDaemonStore_Tier2PrunesBuildCacheByAgeAndBudget pins that age
// and budget are two separate BuildCachePrune calls, mirroring
// PruneDockerRuntime's shape exactly rather than combining both into one
// call's options.
func TestDockerDaemonStore_Tier2PrunesBuildCacheByAgeAndBudget(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{
		Enabled: true, BuildCacheMaxAge: 7 * 24 * time.Hour, BuildCacheBudgetGB: 20,
	})

	if _, err := s.Reclaim(context.Background(), Tier2); err != nil {
		t.Fatalf("Reclaim(Tier2) error: %v", err)
	}
	if fake.containersPruneCalls != 0 || fake.imagesPruneCalls != 0 {
		t.Errorf("Tier2 must not touch containers/images, got %d/%d",
			fake.containersPruneCalls, fake.imagesPruneCalls)
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

// TestDockerDaemonStore_ReclaimTiersItDoesNotOwn pins that a KindGarbage
// store (TiersFor(KindGarbage) == {Tier1}) is a strict no-op outside its own
// tiers, even when called directly with a tier the guard would never route
// to it.
func TestDockerDaemonStore_ReclaimTiersItDoesNotOwn(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{
		Enabled: true, PruneTTL: time.Hour, BuildCacheMaxAge: time.Hour, BuildCacheBudgetGB: 10,
	})

	for _, tier := range []Tier{Tier3, Tier4} {
		freed, err := s.Reclaim(context.Background(), tier)
		if err != nil || freed != 0 {
			t.Errorf("Reclaim(%v) should be a no-op, got freed=%d err=%v", tier, freed, err)
		}
	}
	if fake.containersPruneCalls != 0 || fake.imagesPruneCalls != 0 || fake.buildCachePruneCalls != 0 {
		t.Error("Tier3/Tier4 must not trigger any prune call")
	}
}

func TestDockerDaemonStore_ZeroPruneTTLSkipsGarbageReclaim(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{Enabled: true}) // PruneTTL: 0

	freed, err := s.Reclaim(context.Background(), Tier1)
	if err != nil {
		t.Fatalf("Reclaim(Tier1) error: %v", err)
	}
	if freed != 0 || fake.containersPruneCalls != 0 || fake.imagesPruneCalls != 0 {
		t.Error("PruneTTL <= 0 should skip the garbage prune entirely")
	}
}

func TestDockerDaemonStore_ZeroBuildCacheSettingsSkipsReclaim(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{Enabled: true}) // MaxAge/BudgetGB both 0

	freed, err := s.Reclaim(context.Background(), Tier2)
	if err != nil {
		t.Fatalf("Reclaim(Tier2) error: %v", err)
	}
	if freed != 0 || fake.buildCachePruneCalls != 0 {
		t.Error("BuildCacheMaxAge <= 0 and BuildCacheBudgetGB <= 0 should skip the build cache prune entirely")
	}
}

// TestDockerDaemonStore_Measure sums image and build-cache totals, and
// ignores a daemon-reported <= 0 total (Docker uses negative sizes as an
// "unknown" sentinel) rather than letting it wrap around as a huge uint64.
func TestDockerDaemonStore_Measure(t *testing.T) {
	fake := &fakeDockerAPI{
		rootDir: "/var/lib/docker",
		diskUsage: dockerclient.DiskUsageResult{
			Images:     dockerclient.ImagesDiskUsage{TotalSize: 5_000_000_000},
			BuildCache: dockerclient.BuildCacheDiskUsage{TotalSize: 2_000_000_000},
		},
	}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{})

	got, err := s.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure() error: %v", err)
	}
	if want := uint64(7_000_000_000); got != want {
		t.Errorf("Measure() = %d, want %d", got, want)
	}
}

func TestDockerDaemonStore_Measure_IgnoresUnknownSentinel(t *testing.T) {
	fake := &fakeDockerAPI{
		rootDir: "/var/lib/docker",
		diskUsage: dockerclient.DiskUsageResult{
			Images:     dockerclient.ImagesDiskUsage{TotalSize: -1},
			BuildCache: dockerclient.BuildCacheDiskUsage{TotalSize: 3_000_000_000},
		},
	}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{})

	got, err := s.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure() error: %v", err)
	}
	if want := uint64(3_000_000_000); got != want {
		t.Errorf("Measure() = %d, want %d (negative image total must not wrap around)", got, want)
	}
}
