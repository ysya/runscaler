package cachestore

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/volume"
	dockerclient "github.com/moby/moby/client"
)

func TestCleanupOrphanedBuildxBuilders_NoOpWhenMaxAgeZero(t *testing.T) {
	fake := &fakeDockerAPI{containerList: dockerclient.ContainerListResult{Items: []container.Summary{
		{ID: "old", Names: []string{"/buildx_buildkit_builder-abc0"}, Created: 0},
	}}}
	if err := cleanupOrphanedBuildxBuilders(context.Background(), fake, 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(fake.containersRemoved) != 0 {
		t.Errorf("expected nothing removed, got %v", fake.containersRemoved)
	}
}

func TestCleanupOrphanedBuildxBuilders_RemovesOldKeepsYoungAndOthers(t *testing.T) {
	now := time.Now()
	fake := &fakeDockerAPI{containerList: dockerclient.ContainerListResult{Items: []container.Summary{
		{ID: "old", Names: []string{"/buildx_buildkit_builder-old0"}, Created: now.Add(-48 * time.Hour).Unix()},
		{ID: "young", Names: []string{"/buildx_buildkit_builder-young0"}, Created: now.Add(-1 * time.Hour).Unix()},
		{ID: "runner", Names: []string{"/runner-deadbeef"}, Created: now.Add(-72 * time.Hour).Unix()},
	}}}
	if err := cleanupOrphanedBuildxBuilders(context.Background(), fake, 24*time.Hour, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(fake.containersRemoved) != 1 || fake.containersRemoved[0] != "old" {
		t.Errorf("expected only 'old' removed, got %v", fake.containersRemoved)
	}
	if len(fake.volumesRemoved) != 1 || fake.volumesRemoved[0] != "buildx_buildkit_builder-old0_state" {
		t.Errorf("expected old builder's _state volume removed, got %v", fake.volumesRemoved)
	}
}

func TestCleanupOrphanedBuildxBuilders_ReapsDanglingVolumes(t *testing.T) {
	fake := &fakeDockerAPI{volumeList: dockerclient.VolumeListResult{Items: []volume.Volume{
		{Name: "buildx_buildkit_builder-gone0_state"},
		{Name: "runner-shared"}, // must be left untouched
	}}}
	if err := cleanupOrphanedBuildxBuilders(context.Background(), fake, 24*time.Hour, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(fake.volumesRemoved) != 1 || fake.volumesRemoved[0] != "buildx_buildkit_builder-gone0_state" {
		t.Errorf("expected only dangling buildx volume removed, got %v", fake.volumesRemoved)
	}
}
