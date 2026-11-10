package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/volume"
	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/provider"
)

// fakeContainer is a minimal fixture for driving orphanFake.ContainerList in
// tests: just the fields findOrphanContainers' matching logic reads.
type fakeContainer struct {
	id     string
	names  []string
	labels map[string]string
}

// orphanFake supports orphan lifecycle cleanup and doctor's volume inspection.
// ContainerList reads containers; ContainerRemove records IDs and can fail one.
// VolumeInspect uses volumes; cleanup's VolumeList uses cleanupVolumes so tests
// can distinguish explicitly owned temporary volumes from persistent data.
type orphanFake struct {
	containers     []fakeContainer
	removed        []string
	removeErrOn    string
	stopped        []string
	cleanupVolumes []volume.Volume

	volumes          []string
	volumesRemoved   []string
	volumeInspectErr error
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

func (f *orphanFake) VolumeInspect(_ context.Context, id string, _ dockerclient.VolumeInspectOptions) (dockerclient.VolumeInspectResult, error) {
	if slices.Contains(f.volumes, id) {
		return dockerclient.VolumeInspectResult{}, nil
	}
	if f.volumeInspectErr != nil {
		return dockerclient.VolumeInspectResult{}, f.volumeInspectErr
	}
	return dockerclient.VolumeInspectResult{}, cerrdefs.ErrNotFound
}

func (f *orphanFake) ContainerStop(_ context.Context, id string, _ dockerclient.ContainerStopOptions) (dockerclient.ContainerStopResult, error) {
	f.stopped = append(f.stopped, id)
	return dockerclient.ContainerStopResult{}, nil
}
func (f *orphanFake) VolumeList(_ context.Context, _ dockerclient.VolumeListOptions) (dockerclient.VolumeListResult, error) {
	return dockerclient.VolumeListResult{Items: f.cleanupVolumes}, nil
}
func (f *orphanFake) VolumeRemove(_ context.Context, name string, _ dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error) {
	f.volumesRemoved = append(f.volumesRemoved, name)
	return dockerclient.VolumeRemoveResult{}, nil
}

func TestFindOrphanContainers_MatchesLabelAndNamePattern(t *testing.T) {
	md := &orphanFake{containers: []fakeContainer{
		{id: "a1", names: []string{"/runner-deadbeef"}, labels: map[string]string{"managed-by": "runner"}},
		{id: "b2", names: []string{"/runner-0011aabb"}, labels: nil}, // matched by name pattern, not label
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

type mockCommandRunner struct {
	results map[string][]byte
	errs    map[string]error
	calls   []string
}

func (m *mockCommandRunner) setResult(key, out string) {
	if m.results == nil {
		m.results = make(map[string][]byte)
	}
	m.results[key] = []byte(out)
}

func (m *mockCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	m.calls = append(m.calls, key)
	if err := m.errs[key]; err != nil {
		return nil, err
	}
	return m.results[key], nil
}

func (m *mockCommandRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	_, err := m.Run(ctx, name, args...)
	return err
}

func TestFindOrphanTartVMs_MatchesRunnerAndPoolNames(t *testing.T) {
	mc := &mockCommandRunner{}
	mc.setResult("tart list --format json", `[
	  {"Name":"runner-deadbeef","Source":"local"},
	  {"Name":"pool-0-1778066579017","Source":"local"},
	  {"Name":"runner-nothex00","Source":"local"},
	  {"Name":"pool-x-1778066579017","Source":"local"},
	  {"Name":"runner-keep-0a1b2c3d","Source":"local"},
	  {"Name":"runner-keep-golden","Source":"local"},
	  {"Name":"macos-tahoe-base","Source":"local"},
	  {"Name":"runner-0011aabb","Source":"OCI"},
	  {"Name":"ghcr.io/cirruslabs/macos-tahoe-xcode:latest","Source":"OCI"}
	]`)

	got, err := findOrphanTartVMs(context.Background(), mc)
	if err != nil {
		t.Fatalf("findOrphanTartVMs: %v", err)
	}
	// runner-keep-<8 hex> is the throwaway clone a cache prune leaves behind
	// if runner dies between cloning and deleting it.
	want := map[string]bool{"runner-deadbeef": true, "pool-0-1778066579017": true, "runner-keep-0a1b2c3d": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v — base images and user VMs must not be claimed", got, want)
	}
	for _, vm := range got {
		if !want[vm.Name] {
			t.Errorf("claimed %q, which runner did not create", vm.Name)
		}
	}
}

func TestRemoveOrphanTartVMs_ContinuesAfterDeleteFailure(t *testing.T) {
	mc := &mockCommandRunner{errs: map[string]error{
		"tart stop runner-deadbeef":   errors.New("already stopped"),
		"tart delete runner-deadbeef": errors.New("simulated delete failure"),
	}}

	removed := removeOrphanTartVMs(context.Background(), mc,
		[]provider.TartVM{{Name: "runner-deadbeef"}, {Name: "pool-0-1778066579017"}}, slog.New(slog.DiscardHandler))

	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	for _, want := range []string{
		"tart stop runner-deadbeef",
		"tart delete runner-deadbeef",
		"tart stop pool-0-1778066579017",
		"tart delete pool-0-1778066579017",
	} {
		if !slices.Contains(mc.calls, want) {
			t.Errorf("missing command %q in %v", want, mc.calls)
		}
	}
}

func TestOrphanCleanupPreservesActionsCacheServiceAndData(t *testing.T) {
	fake := &orphanFake{
		containers: []fakeContainer{
			{id: "root", names: []string{"/runner-deadbeef"}, labels: map[string]string{"managed-by": "runner"}},
			{id: "child", names: []string{"/job-child"}, labels: map[string]string{"runscaler.runner-id": "root"}},
			{id: "cache", names: []string{"/runscaler-actions-cache"}, labels: map[string]string{"managed-by": "runscaler-cache"}},
		},
		cleanupVolumes: []volume.Volume{{Name: "runscaler-actions-cache-data", Labels: map[string]string{"managed-by": "runscaler-cache"}}},
	}
	orphans, err := findOrphanContainers(context.Background(), fake)
	if err != nil || len(orphans) != 1 || orphans[0].ID != "root" {
		t.Fatalf("orphans=%v err=%v", orphans, err)
	}
	if len(fake.stopped) != 0 || len(fake.removed) != 0 {
		t.Fatal("orphan discovery must remain read-only")
	}
	if removed := removeOrphanContainers(context.Background(), fake, orphans, slog.New(slog.DiscardHandler)); removed != 1 {
		t.Fatalf("removed %d roots", removed)
	}
	if !slices.Equal(fake.removed, []string{"child", "root"}) || len(fake.volumesRemoved) != 0 {
		t.Fatalf("cache service/data affected: containers=%v volumes=%v", fake.removed, fake.volumesRemoved)
	}
}
