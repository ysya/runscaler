package provider

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	dockerclient "github.com/moby/moby/client"
)

// cleanupDocker models the daemon's container inventory. A stopped runner
// stays discoverable until removal, including after a child cleanup failure.
type cleanupDocker struct {
	*mockDocker
	stopErr       error
	listErr       error
	volumeListErr error
	removeErrors  map[string]error
	volumeErrors  map[string]error
	stopOptions   []dockerclient.ContainerStopOptions
	removeOptions []dockerclient.ContainerRemoveOptions
	volumeOptions []dockerclient.VolumeRemoveOptions
	onStop        func()
	onRemove      func(string)
}

func (m *cleanupDocker) VolumeList(ctx context.Context, _ dockerclient.VolumeListOptions) (dockerclient.VolumeListResult, error) {
	if err := ctx.Err(); err != nil {
		return dockerclient.VolumeListResult{}, err
	}
	return dockerclient.VolumeListResult{Items: m.volumes}, m.volumeListErr
}

func (m *cleanupDocker) VolumeRemove(ctx context.Context, name string, options dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error) {
	m.volumesRemoved = append(m.volumesRemoved, name)
	m.volumeOptions = append(m.volumeOptions, options)
	if err := ctx.Err(); err != nil {
		return dockerclient.VolumeRemoveResult{}, err
	}
	return dockerclient.VolumeRemoveResult{}, m.volumeErrors[name]
}

func (m *cleanupDocker) ContainerStop(ctx context.Context, _ string, options dockerclient.ContainerStopOptions) (dockerclient.ContainerStopResult, error) {
	m.stopOptions = append(m.stopOptions, options)
	if err := ctx.Err(); err != nil {
		return dockerclient.ContainerStopResult{}, err
	}
	if m.onStop != nil {
		m.onStop()
	}
	return dockerclient.ContainerStopResult{}, m.stopErr
}

func (m *cleanupDocker) ContainerList(ctx context.Context, options dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	if err := ctx.Err(); err != nil {
		return dockerclient.ContainerListResult{}, err
	}
	if !options.All {
		return dockerclient.ContainerListResult{}, errors.New("cleanup must include stopped containers")
	}
	return dockerclient.ContainerListResult{Items: slices.Clone(m.containers)}, m.listErr
}

func (m *cleanupDocker) ContainerRemove(ctx context.Context, id string, options dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error) {
	m.removed = append(m.removed, id)
	m.removeOptions = append(m.removeOptions, options)
	if m.onRemove != nil {
		m.onRemove(id)
	}
	if err := ctx.Err(); err != nil {
		return dockerclient.ContainerRemoveResult{}, err
	}
	if err := m.removeErrors[id]; err != nil {
		return dockerclient.ContainerRemoveResult{}, err
	}
	for i, c := range m.containers {
		if c.ID == id {
			m.containers = slices.Delete(m.containers, i, i+1)
			return dockerclient.ContainerRemoveResult{}, nil
		}
	}
	return dockerclient.ContainerRemoveResult{}, cerrdefs.ErrNotFound
}

func newCleanupDockerProvider() (*DockerProvider, *cleanupDocker) {
	p, base := newTestDockerProvider("", true)
	m := &cleanupDocker{mockDocker: base}
	p.dockerClient = m
	return p, m
}

func networkContainer(id, network string) container.Summary {
	c := container.Summary{ID: id, State: container.StateRunning}
	c.HostConfig.NetworkMode = network
	return c
}

func TestDockerProvider_RemoveInstanceCleansOnlyOwnedContainers(t *testing.T) {
	p, m := newCleanupDockerProvider()
	runnerID := strings.Repeat("a", 64)
	m.containers = []container.Summary{
		{ID: runnerID, Names: []string{"/runner-one"}, Labels: map[string]string{"managed-by": "runner"}},
		networkContainer("by-id", "container:"+runnerID),
		networkContainer("by-short-id", "container:"+runnerID[:12]),
		networkContainer("by-name", "container:runner-one"),
		{ID: "by-label", Labels: map[string]string{"runscaler.runner-id": runnerID}, State: container.StateExited},
		{ID: "by-name-label", Labels: map[string]string{"runscaler.runner-id": "runner-one"}},
		{ID: "by-short-label", Labels: map[string]string{"runscaler.runner-id": runnerID[:12]}},
		{ID: "other-runner", Names: []string{"/runner-two"}, Labels: map[string]string{"managed-by": "runner"}},
		networkContainer("other-child", "container:runner-two"),
		networkContainer("shared-network", "runners"),
		networkContainer("similar-name", "container:runner-one-extra"),
		{ID: "other-label", Labels: map[string]string{"runscaler.runner-id": "other-runner"}},
	}
	// A managed runner keeps its own lifecycle even if it deliberately
	// shares this runner's network namespace.
	m.containers[7].HostConfig.NetworkMode = "container:" + runnerID
	if err := p.RemoveInstance(context.Background(), runnerID); err != nil {
		t.Fatal(err)
	}
	want := []string{"by-id", "by-short-id", "by-name", "by-label", "by-name-label", "by-short-label", runnerID}
	if !slices.Equal(m.removed, want) {
		t.Fatalf("removed = %v, want %v", m.removed, want)
	}
	if len(m.stopOptions) != 1 || m.stopOptions[0].Timeout == nil || *m.stopOptions[0].Timeout != 0 {
		t.Fatalf("runner must stop before scanning for children, got options %v", m.stopOptions)
	}
	for _, options := range m.removeOptions {
		if !options.Force || !options.RemoveVolumes {
			t.Errorf("removal must force-remove containers and their anonymous volumes: %+v", options)
		}
	}
	if len(m.volumesRemoved) != 0 {
		t.Fatalf("removed persistent volumes: %v", m.volumesRemoved)
	}
}

func TestDockerProvider_RemoveInstanceSkipsAmbiguousReferences(t *testing.T) {
	p, m := newCleanupDockerProvider()
	runnerID := strings.Repeat("a", 64)
	m.containers = []container.Summary{
		{ID: runnerID},
		{ID: runnerID[:12] + strings.Repeat("b", 52)},
		networkContainer("ambiguous-network", "container:"+runnerID[:12]),
		{ID: "ambiguous-label", Labels: map[string]string{"runscaler.runner-id": runnerID[:12]}},
		networkContainer("too-short", "container:a"),
	}
	m.volumes = []volume.Volume{{Name: "ambiguous-volume", Labels: map[string]string{"runscaler.runner-id": runnerID[:12]}}}
	if err := p.RemoveInstance(context.Background(), runnerID); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.removed, []string{runnerID}) {
		t.Fatalf("ambiguous ownership must not delete containers: %v", m.removed)
	}
	if len(m.volumesRemoved) != 0 {
		t.Fatalf("ambiguous ownership must not delete volumes: %v", m.volumesRemoved)
	}
}

func TestDockerProvider_RemoveInstanceScansAfterStopping(t *testing.T) {
	p, m := newCleanupDockerProvider()
	m.containers = []container.Summary{{ID: "runner-id"}}
	m.onStop = func() {
		m.containers = append(m.containers, networkContainer("last-child", "container:runner-id"))
	}
	if err := p.RemoveInstance(context.Background(), "runner-id"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.removed, []string{"last-child", "runner-id"}) {
		t.Fatalf("missed a child created before runner stopped: %v", m.removed)
	}
}

func TestDockerProvider_RemoveInstanceRetainsRunnerForChildCleanupRetry(t *testing.T) {
	p, m := newCleanupDockerProvider()
	failure := errors.New("child removal failed")
	m.removeErrors = map[string]error{"failed-child": failure}
	m.containers = []container.Summary{
		{ID: "runner-id", Names: []string{"/runner-name"}},
		networkContainer("failed-child", "container:runner-name"),
		networkContainer("other-child", "container:runner-id"),
	}
	m.containers[1].Names = []string{"/buildx_buildkit_retry0"}
	m.containers[1].Mounts = []container.MountPoint{volumeMount("buildx_buildkit_retry0_state")}
	if err := p.RemoveInstance(context.Background(), "runner-id"); !errors.Is(err, failure) {
		t.Fatalf("error = %v, want child failure", err)
	}
	if !slices.Equal(m.removed, []string{"failed-child", "other-child"}) {
		t.Fatalf("must attempt other children and retain runner on failure: %v", m.removed)
	}
	if len(m.volumesRemoved) != 0 {
		t.Fatalf("removed volumes still mounted by failed child: %v", m.volumesRemoved)
	}
	m.removeErrors = nil
	m.stopErr = cerrdefs.ErrNotModified // already stopped after the first attempt
	if err := p.RemoveInstance(context.Background(), "runner-id"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(m.containers) != 0 {
		t.Fatalf("retry left containers: %v", m.containers)
	}
	if !slices.Equal(m.volumesRemoved, []string{"buildx_buildkit_retry0_state"}) {
		t.Fatalf("retry left builder state volume: %v", m.volumesRemoved)
	}
}

func TestDockerProvider_RemoveInstanceCleansChildrenOfAbsentRunner(t *testing.T) {
	p, m := newCleanupDockerProvider()
	m.stopErr = cerrdefs.ErrNotFound
	m.containers = []container.Summary{
		networkContainer("leftover", "container:runner-id"),
		{ID: "labeled-leftover", Labels: map[string]string{"runscaler.runner-id": "runner-id"}},
	}
	if err := p.RemoveInstance(context.Background(), "runner-id"); err != nil {
		t.Fatal(err)
	}
	if len(m.containers) != 0 {
		t.Fatalf("left containers after externally removed runner: %v", m.containers)
	}
}

func TestDockerProvider_RemoveInstancePreservesRunnerOnStopOrListFailure(t *testing.T) {
	for _, stage := range []string{"stop", "list"} {
		t.Run(stage, func(t *testing.T) {
			p, m := newCleanupDockerProvider()
			failure := errors.New(stage + " failed")
			if stage == "stop" {
				m.stopErr = failure
			} else {
				m.listErr = failure
			}
			m.containers = []container.Summary{{ID: "runner-id"}}
			if err := p.RemoveInstance(context.Background(), "runner-id"); !errors.Is(err, failure) {
				t.Fatalf("error = %v, want %v", err, failure)
			}
			if len(m.removed) != 0 {
				t.Fatalf("removed containers without a complete ownership scan: %v", m.removed)
			}
		})
	}
}

func TestDockerProvider_RemoveInstanceHonorsCanceledContext(t *testing.T) {
	p, m := newCleanupDockerProvider()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.RemoveInstance(ctx, "runner-id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled context", err)
	}
	if len(m.removed) != 0 {
		t.Fatalf("attempted removal after cancellation: %v", m.removed)
	}
}

func TestDockerProvider_RemoveInstanceStopsCleanupOnCancellation(t *testing.T) {
	p, m := newCleanupDockerProvider()
	m.containers = []container.Summary{
		{ID: "runner-id"},
		networkContainer("first-child", "container:runner-id"),
		networkContainer("second-child", "container:runner-id"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.onRemove = func(string) { cancel() }
	if err := p.RemoveInstance(ctx, "runner-id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled context", err)
	}
	if !slices.Equal(m.removed, []string{"first-child"}) {
		t.Fatalf("continued cleanup after cancellation: %v", m.removed)
	}
}

func TestDockerProvider_RemoveInstanceIgnoresDisappearedChild(t *testing.T) {
	p, m := newCleanupDockerProvider()
	m.containers = []container.Summary{
		{ID: "runner-id"},
		networkContainer("gone-child", "container:runner-id"),
	}
	m.removeErrors = map[string]error{"gone-child": cerrdefs.ErrNotFound}
	if err := p.RemoveInstance(context.Background(), "runner-id"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.removed, []string{"gone-child", "runner-id"}) {
		t.Fatalf("disappeared child prevented runner removal: %v", m.removed)
	}
}

func volumeMount(name string) container.MountPoint {
	return container.MountPoint{Type: mount.TypeVolume, Name: name}
}

func TestDockerProvider_RemoveInstanceCleansOnlyEphemeralVolumes(t *testing.T) {
	p, m := newCleanupDockerProvider()
	builder := networkContainer("builder", "container:runner-id")
	builder.Names = []string{"/buildx_buildkit_owned0"}
	builder.Mounts = []container.MountPoint{
		volumeMount("buildx_buildkit_owned0_state"),
		volumeMount("unmarked-data"),
		volumeMount("gradle-cache"),
	}
	sharedBuilder := networkContainer("shared-builder", "container:runner-id")
	sharedBuilder.Names = []string{"/buildx_buildkit_shared0"}
	sharedBuilder.Mounts = []container.MountPoint{volumeMount("buildx_buildkit_shared0_state")}
	m.containers = []container.Summary{
		{ID: "runner-id", Names: []string{"/runner-name"}, Mounts: []container.MountPoint{
			volumeMount("runner-shared"), volumeMount("gradle-cache"),
		}},
		builder,
		sharedBuilder,
		{ID: "unrelated", Mounts: []container.MountPoint{
			volumeMount("still-shared"), volumeMount("buildx_buildkit_shared0_state"),
		}},
	}
	m.volumes = []volume.Volume{
		{Name: "owned-temp", Labels: map[string]string{"runscaler.runner-id": "runner-id"}},
		{Name: "owned-alias", Labels: map[string]string{"runscaler.runner-id": "runner-name"}},
		{Name: "runner-shared", Labels: map[string]string{"runscaler.runner-id": "runner-id"}},
		{Name: "gradle-cache", Labels: map[string]string{"runscaler.runner-id": "runner-id"}},
		{Name: "still-shared", Labels: map[string]string{"runscaler.runner-id": "runner-id"}},
		{Name: "other-owner", Labels: map[string]string{"runscaler.runner-id": "other-runner"}},
		{Name: "unmarked-data"},
	}
	if err := p.RemoveInstance(context.Background(), "runner-id"); err != nil {
		t.Fatal(err)
	}
	slices.Sort(m.volumesRemoved)
	want := []string{"buildx_buildkit_owned0_state", "owned-alias", "owned-temp"}
	if !slices.Equal(m.volumesRemoved, want) {
		t.Fatalf("removed volumes = %v, want only %v", m.volumesRemoved, want)
	}
	for _, options := range m.volumeOptions {
		if options.Force {
			t.Fatal("named volume removal must not force past daemon safeguards")
		}
	}
}

func TestDockerProvider_RemoveInstancePreservesOwnershipOnVolumeListFailure(t *testing.T) {
	p, m := newCleanupDockerProvider()
	failure := errors.New("volume inventory unavailable")
	m.volumeListErr = failure
	m.containers = []container.Summary{{ID: "runner-id"}, networkContainer("child", "container:runner-id")}
	if err := p.RemoveInstance(context.Background(), "runner-id"); !errors.Is(err, failure) {
		t.Fatalf("error = %v, want volume list failure", err)
	}
	if len(m.removed) != 0 {
		t.Fatalf("lost container ownership before volume inventory succeeded: %v", m.removed)
	}
}

func TestDockerProvider_RemoveInstanceLogsVolumeFailureAndContinues(t *testing.T) {
	p, m := newCleanupDockerProvider()
	var logs bytes.Buffer
	p.logger = slog.New(slog.NewTextHandler(&logs, nil))
	m.containers = []container.Summary{{ID: "runner-id"}}
	m.volumes = []volume.Volume{
		{Name: "failed-volume", Labels: map[string]string{"runscaler.runner-id": "runner-id"}},
		{Name: "other-volume", Labels: map[string]string{"runscaler.runner-id": "runner-id"}},
	}
	m.volumeErrors = map[string]error{"failed-volume": errors.New("volume driver unavailable")}
	if err := p.RemoveInstance(context.Background(), "runner-id"); err != nil {
		t.Fatal(err)
	}
	slices.Sort(m.volumesRemoved)
	if !slices.Equal(m.volumesRemoved, []string{"failed-volume", "other-volume"}) || !slices.Equal(m.removed, []string{"runner-id"}) {
		t.Fatalf("cleanup did not continue: volumes %v, containers %v", m.volumesRemoved, m.removed)
	}
	if !strings.Contains(logs.String(), "failed-volume") || !strings.Contains(logs.String(), "volume driver unavailable") {
		t.Fatalf("volume cleanup failure was not reported: %s", logs.String())
	}
}

func TestDockerProvider_RemoveInstancePreservesActionsCacheServiceAndData(t *testing.T) {
	p, m := newCleanupDockerProvider()
	m.containers = []container.Summary{
		{ID: "root", Names: []string{"/runner-deadbeef"}, Labels: map[string]string{"managed-by": "runner"}},
		{ID: "child", Labels: map[string]string{"runscaler.runner-id": "root"}},
		{ID: "cache", Names: []string{"/runscaler-actions-cache"}, Labels: map[string]string{"managed-by": "runscaler-cache"}, Mounts: []container.MountPoint{volumeMount("runscaler-actions-cache-data")}},
	}
	m.volumes = []volume.Volume{{Name: "runscaler-actions-cache-data", Labels: map[string]string{"managed-by": "runscaler-cache"}}}
	if err := p.RemoveInstance(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.removed, []string{"child", "root"}) || len(m.volumesRemoved) != 0 {
		t.Fatalf("cache service/data affected: containers=%v volumes=%v", m.removed, m.volumesRemoved)
	}
}
