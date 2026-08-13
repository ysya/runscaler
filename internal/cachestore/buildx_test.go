package cachestore

import (
	"context"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
)

func TestBuildxStore_OnlyTier1(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewBuildxStore(fake, BuildxConfig{Enabled: true, MaxAge: 24 * time.Hour})
	if s.Kind() != KindGarbage {
		t.Errorf("Kind() = %v, want KindGarbage", s.Kind())
	}
	for _, tier := range []Tier{Tier2, Tier3, Tier4} {
		freed, err := s.Reclaim(context.Background(), tier)
		if err != nil || freed != 0 {
			t.Errorf("Reclaim(%v) should be a no-op, got freed=%d err=%v", tier, freed, err)
		}
	}
}

// TestBuildxStore_Tier1RemovesOrphanedBuildersAndStateVolumes verifies the
// store is actually wired to backend.CleanupOrphanedBuildxBuilders — not
// just that other tiers no-op: an old buildx builder container and its
// `_state` volume both get removed, and young ones are left alone.
func TestBuildxStore_Tier1RemovesOrphanedBuildersAndStateVolumes(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour).Unix()
	young := time.Now().Add(-time.Minute).Unix()
	fake := &fakeDockerAPI{
		rootDir: "/var/lib/docker",
		containerList: dockerclient.ContainerListResult{
			Items: []container.Summary{
				{ID: "c1", Names: []string{"/buildx_buildkit_mybuilder0"}, Created: old},
				{ID: "c2", Names: []string{"/buildx_buildkit_freshbuilder0"}, Created: young},
				{ID: "c3", Names: []string{"/unrelated-container"}, Created: old},
			},
		},
	}
	s := NewBuildxStore(fake, BuildxConfig{Enabled: true, MaxAge: 24 * time.Hour})

	freed, err := s.Reclaim(context.Background(), Tier1)
	if err != nil {
		t.Fatalf("Reclaim(Tier1) error: %v", err)
	}
	if freed != 0 {
		t.Errorf("Reclaim(Tier1) freed = %d, want 0 (CleanupOrphanedBuildxBuilders reports no total — see comment)", freed)
	}
	if len(fake.containersRemoved) != 1 || fake.containersRemoved[0] != "c1" {
		t.Errorf("containersRemoved = %v, want [c1] (only the old buildx builder)", fake.containersRemoved)
	}
	if len(fake.volumesRemoved) != 1 || fake.volumesRemoved[0] != "buildx_buildkit_mybuilder0_state" {
		t.Errorf("volumesRemoved = %v, want [buildx_buildkit_mybuilder0_state]", fake.volumesRemoved)
	}
}

// TestBuildxStore_ReclaimsRegardlessOfEnabled pins the 2026-08-14 spec
// revision (see store.go's Enabled doc comment): Enabled()==false means the
// operator left buildx-cleanup off cmd/runner's own periodic schedule, not
// "never touch this even under disk pressure" — the disk guard calls
// Reclaim on a disabled store exactly like an enabled one, so this method
// must not gate on cfg.Enabled itself. cmd/runner's sweeper is what still
// respects Enabled(): it simply never launches the goroutine that would
// call this method when disabled, which is a different (and unchanged)
// code path from this one.
func TestBuildxStore_ReclaimsRegardlessOfEnabled(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour).Unix()
	fake := &fakeDockerAPI{
		containerList: dockerclient.ContainerListResult{
			Items: []container.Summary{
				{ID: "c1", Names: []string{"/buildx_buildkit_mybuilder0"}, Created: old},
			},
		},
	}
	s := NewBuildxStore(fake, BuildxConfig{Enabled: false, MaxAge: 24 * time.Hour})

	freed, err := s.Reclaim(context.Background(), Tier1)
	if err != nil {
		t.Fatalf("Reclaim(Tier1) error: %v", err)
	}
	if freed != 0 {
		t.Errorf("Reclaim(Tier1) freed = %d, want 0 (CleanupOrphanedBuildxBuilders reports no total — see comment on the enabled-path test)", freed)
	}
	if len(fake.containersRemoved) != 1 || fake.containersRemoved[0] != "c1" {
		t.Errorf("containersRemoved = %v, want [c1] — a disabled store must still reclaim when Reclaim is called directly", fake.containersRemoved)
	}
}

func TestBuildxStore_ZeroMaxAgeSkipsReclaim(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour).Unix()
	fake := &fakeDockerAPI{
		containerList: dockerclient.ContainerListResult{
			Items: []container.Summary{
				{ID: "c1", Names: []string{"/buildx_buildkit_mybuilder0"}, Created: old},
			},
		},
	}
	s := NewBuildxStore(fake, BuildxConfig{Enabled: true}) // MaxAge: 0

	freed, err := s.Reclaim(context.Background(), Tier1)
	if err != nil {
		t.Fatalf("Reclaim(Tier1) error: %v", err)
	}
	if freed != 0 || len(fake.containersRemoved) != 0 {
		t.Error("MaxAge <= 0 should skip the sweep entirely (CleanupOrphanedBuildxBuilders' own no-op)")
	}
}

func TestBuildxStore_NameKindPathEnabled(t *testing.T) {
	fake := &fakeDockerAPI{}
	s := NewBuildxStore(fake, BuildxConfig{Enabled: true, RootDir: "/var/lib/docker"})

	if s.Name() != "buildx" {
		t.Errorf("Name() = %q, want %q", s.Name(), "buildx")
	}
	if got := s.Path(); got != "/var/lib/docker" {
		t.Errorf("Path() = %q, want %q (from cfg.RootDir directly, no Info() round trip)", got, "/var/lib/docker")
	}
	if !s.Enabled() {
		t.Error("Enabled() = false, want true")
	}
}

func TestBuildxStore_Measure(t *testing.T) {
	s := NewBuildxStore(&fakeDockerAPI{}, BuildxConfig{Enabled: true})

	got, err := s.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure() error: %v", err)
	}
	if got != 0 {
		t.Errorf("Measure() = %d, want 0 (see comment: no cheap way to isolate just this store's usage)", got)
	}
}
