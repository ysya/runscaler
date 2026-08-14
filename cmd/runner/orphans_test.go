package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
)

// fakeContainer is a minimal fixture for driving orphanFake.ContainerList in
// tests: just the fields findOrphanContainers' matching logic reads.
type fakeContainer struct {
	id     string
	names  []string
	labels map[string]string
}

// orphanFake implements backend.DockerAPI with just enough behavior to
// exercise findOrphanContainers and removeOrphanContainers. ContainerList is
// driven by containers; ContainerRemove records every ID it is called with
// to removed, failing when the ID equals removeErrOn; VolumeRemove records
// to volumesRemoved. Every other method returns its zero value — orphans.go
// never calls them.
//
// Field names here are relied on by later tasks' tests (Tasks 2 and 3 build
// on this double, and Task 2 additionally extends it to satisfy
// cmd_doctor.go's volumeAPI interface) — see task-2-report.md and
// task-3-report.md.
type orphanFake struct {
	containers  []fakeContainer
	removed     []string
	removeErrOn string

	volumesRemoved []string
}

func (f *orphanFake) ContainerList(_ context.Context, _ dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	items := make([]container.Summary, len(f.containers))
	for i, c := range f.containers {
		items[i] = container.Summary{ID: c.id, Names: c.names, Labels: c.labels}
	}
	return dockerclient.ContainerListResult{Items: items}, nil
}

func (f *orphanFake) ContainerRemove(_ context.Context, containerID string, _ dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error) {
	f.removed = append(f.removed, containerID)
	if f.removeErrOn != "" && containerID == f.removeErrOn {
		return dockerclient.ContainerRemoveResult{}, errors.New("simulated remove failure")
	}
	return dockerclient.ContainerRemoveResult{}, nil
}

func (f *orphanFake) VolumeRemove(_ context.Context, volumeID string, _ dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error) {
	f.volumesRemoved = append(f.volumesRemoved, volumeID)
	return dockerclient.VolumeRemoveResult{}, nil
}

// The remaining backend.DockerAPI methods are unused by orphans.go; each
// returns its zero value.

func (f *orphanFake) ContainerCreate(_ context.Context, _ dockerclient.ContainerCreateOptions) (dockerclient.ContainerCreateResult, error) {
	return dockerclient.ContainerCreateResult{}, nil
}

func (f *orphanFake) ContainerStart(_ context.Context, _ string, _ dockerclient.ContainerStartOptions) (dockerclient.ContainerStartResult, error) {
	return dockerclient.ContainerStartResult{}, nil
}

func (f *orphanFake) ContainerWait(_ context.Context, _ string, _ dockerclient.ContainerWaitOptions) dockerclient.ContainerWaitResult {
	return dockerclient.ContainerWaitResult{}
}

func (f *orphanFake) ContainerPrune(_ context.Context, _ dockerclient.ContainerPruneOptions) (dockerclient.ContainerPruneResult, error) {
	return dockerclient.ContainerPruneResult{}, nil
}

func (f *orphanFake) ImagePrune(_ context.Context, _ dockerclient.ImagePruneOptions) (dockerclient.ImagePruneResult, error) {
	return dockerclient.ImagePruneResult{}, nil
}

func (f *orphanFake) BuildCachePrune(_ context.Context, _ dockerclient.BuildCachePruneOptions) (dockerclient.BuildCachePruneResult, error) {
	return dockerclient.BuildCachePruneResult{}, nil
}

func (f *orphanFake) VolumeList(_ context.Context, _ dockerclient.VolumeListOptions) (dockerclient.VolumeListResult, error) {
	return dockerclient.VolumeListResult{}, nil
}

func (f *orphanFake) ContainerLogs(_ context.Context, _ string, _ dockerclient.ContainerLogsOptions) (dockerclient.ContainerLogsResult, error) {
	return nil, nil
}

func (f *orphanFake) Info(_ context.Context, _ dockerclient.InfoOptions) (dockerclient.SystemInfoResult, error) {
	return dockerclient.SystemInfoResult{}, nil
}

func (f *orphanFake) DiskUsage(_ context.Context, _ dockerclient.DiskUsageOptions) (dockerclient.DiskUsageResult, error) {
	return dockerclient.DiskUsageResult{}, nil
}

func TestFindOrphanContainers_MatchesLabelAndNamePattern(t *testing.T) {
	md := &orphanFake{containers: []fakeContainer{
		{id: "a1", names: []string{"/runner-deadbeef"}, labels: map[string]string{"managed-by": "runner"}},
		{id: "b2", names: []string{"/runner-0011aabb"}, labels: nil}, // 名稱樣式
		{id: "c3", names: []string{"/postgres"}, labels: map[string]string{"app": "db"}},
		{id: "d4", names: []string{"/buildx_buildkit_x0"}, labels: nil},
	}}

	got, err := findOrphanContainers(context.Background(), md)
	if err != nil {
		t.Fatalf("findOrphanContainers: %v", err)
	}
	ids := map[string]bool{}
	for _, o := range got {
		ids[o.ID] = true
	}
	if !ids["a1"] || !ids["b2"] {
		t.Errorf("expected runner-owned containers a1 and b2, got %v", ids)
	}
	if ids["c3"] || ids["d4"] {
		t.Errorf("must not claim containers runner did not create, got %v", ids)
	}
}

func TestRemoveOrphanContainers_ContinuesAfterFailure(t *testing.T) {
	md := &orphanFake{removeErrOn: "a1"}
	orphans := []OrphanContainer{{ID: "a1", Name: "runner-1"}, {ID: "b2", Name: "runner-2"}}

	removed := removeOrphanContainers(context.Background(), md, orphans, slog.New(slog.DiscardHandler))

	if removed != 1 {
		t.Errorf("removed = %d, want 1 (b2 succeeds even though a1 failed)", removed)
	}
	if len(md.removed) != 2 {
		t.Errorf("both removals should be attempted, got attempts on %v", md.removed)
	}
}

// TestRemoveOrphanContainers_NeverTouchesVolumes pins the invariant that
// protects in-flight workflow handoff data: reconciliation removes
// containers only. The shared volume is designed to outlive the process.
func TestRemoveOrphanContainers_NeverTouchesVolumes(t *testing.T) {
	md := &orphanFake{}
	removeOrphanContainers(context.Background(), md,
		[]OrphanContainer{{ID: "a1", Name: "runner-1"}}, slog.New(slog.DiscardHandler))

	if len(md.volumesRemoved) != 0 {
		t.Errorf("reconciliation removed volumes %v — the shared volume carries "+
			"handoff data between jobs of one workflow run", md.volumesRemoved)
	}
}
