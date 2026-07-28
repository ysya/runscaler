package backend

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// --- Docker Mock ---

type createCall struct {
	name       string
	config     *container.Config
	hostConfig *container.HostConfig
}

type mockDocker struct {
	created        []string     // container names
	createCalls    []createCall // full create args
	started        []string     // container IDs
	removed        []string     // container IDs
	volumesRemoved []string     // volume names

	// Optional overrides for ContainerWait. When unset, the wait succeeds
	// immediately with status 0.
	waitStatus int64
	waitErr    error

	// Optional fixtures for ContainerList / VolumeList.
	containers []container.Summary
	volumes    []*volume.Volume

	// Recorded prune calls, in order, for PruneDockerRuntime /
	// CleanupSharedDocker assertions.
	containersPruneFilters []filters.Args
	imagesPruneFilters     []filters.Args
	buildCachePruneOpts    []build.CachePruneOptions

	// Optional error injection for the prune calls.
	containersPruneErr error
	imagesPruneErr     error
	buildCachePruneErr error
}

func (m *mockDocker) ContainerCreate(_ context.Context, cfg *container.Config, hcfg *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	id := "sha256-" + name
	m.created = append(m.created, name)
	m.createCalls = append(m.createCalls, createCall{name: name, config: cfg, hostConfig: hcfg})
	return container.CreateResponse{ID: id}, nil
}

func (m *mockDocker) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	m.started = append(m.started, id)
	return nil
}

func (m *mockDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	m.removed = append(m.removed, id)
	return nil
}

func (m *mockDocker) ContainersPrune(_ context.Context, pruneFilters filters.Args) (container.PruneReport, error) {
	m.containersPruneFilters = append(m.containersPruneFilters, pruneFilters)
	if m.containersPruneErr != nil {
		return container.PruneReport{}, m.containersPruneErr
	}
	return container.PruneReport{}, nil
}

func (m *mockDocker) ImagesPrune(_ context.Context, pruneFilters filters.Args) (image.PruneReport, error) {
	m.imagesPruneFilters = append(m.imagesPruneFilters, pruneFilters)
	if m.imagesPruneErr != nil {
		return image.PruneReport{}, m.imagesPruneErr
	}
	return image.PruneReport{}, nil
}

func (m *mockDocker) BuildCachePrune(_ context.Context, opts build.CachePruneOptions) (*build.CachePruneReport, error) {
	m.buildCachePruneOpts = append(m.buildCachePruneOpts, opts)
	if m.buildCachePruneErr != nil {
		return nil, m.buildCachePruneErr
	}
	return &build.CachePruneReport{}, nil
}

func (m *mockDocker) VolumeRemove(_ context.Context, volumeID string, _ bool) error {
	m.volumesRemoved = append(m.volumesRemoved, volumeID)
	return nil
}

func (m *mockDocker) ContainerList(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
	return m.containers, nil
}

func (m *mockDocker) VolumeList(_ context.Context, _ volume.ListOptions) (volume.ListResponse, error) {
	return volume.ListResponse{Volumes: m.volumes}, nil
}

func (m *mockDocker) ContainerWait(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	statusCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	if m.waitErr != nil {
		errCh <- m.waitErr
	} else {
		statusCh <- container.WaitResponse{StatusCode: m.waitStatus}
	}
	return statusCh, errCh
}

func newTestDockerBackend(sharedVolume string, dind bool) (*DockerBackend, *mockDocker) {
	md := &mockDocker{}
	b := &DockerBackend{
		dockerClient: md,
		runnerImage:  "test-image:latest",
		dockerSocket: "/var/run/docker.sock",
		dind:         dind,
		sharedVolume: sharedVolume,
		logger:       slog.New(slog.DiscardHandler),
	}
	return b, md
}

func newTestDockerBackendWithResources(memory int64, cpu int64) (*DockerBackend, *mockDocker) {
	md := &mockDocker{}
	b := &DockerBackend{
		dockerClient: md,
		runnerImage:  "test-image:latest",
		dockerSocket: "/var/run/docker.sock",
		dind:         false,
		memoryBytes:  memory,
		nanoCPUs:     cpu,
		logger:       slog.New(slog.DiscardHandler),
	}
	return b, md
}

// --- Docker Backend tests ---

// findMountByTarget returns the mount with the given target, or nil if not found.
func findMountByTarget(mounts []mount.Mount, target string) *mount.Mount {
	for i := range mounts {
		if mounts[i].Target == target {
			return &mounts[i]
		}
	}
	return nil
}

func TestDockerBackend_StartRunner_WithSharedVolume(t *testing.T) {
	b, md := newTestDockerBackend("/shared", true)
	ctx := context.Background()

	resourceID, err := b.StartRunner(ctx, "runner-1", "mock-jit-config")
	if err != nil {
		t.Fatalf("StartRunner() error: %v", err)
	}
	if resourceID != "sha256-runner-1" {
		t.Errorf("resourceID = %q, want %q", resourceID, "sha256-runner-1")
	}

	if len(md.createCalls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(md.createCalls))
	}
	call := md.createCalls[0]

	// Verify docker socket bind mount
	dockerMount := findMountByTarget(call.hostConfig.Mounts, "/var/run/docker.sock")
	if dockerMount == nil {
		t.Fatal("docker socket mount not found")
	}
	if dockerMount.Type != mount.TypeBind {
		t.Errorf("docker mount type = %v, want %v", dockerMount.Type, mount.TypeBind)
	}

	// Verify shared named volume mount
	sharedMount := findMountByTarget(call.hostConfig.Mounts, "/shared")
	if sharedMount == nil {
		t.Fatal("shared volume mount not found")
	}
	if sharedMount.Type != mount.TypeVolume {
		t.Errorf("mount type = %v, want %v", sharedMount.Type, mount.TypeVolume)
	}
	if sharedMount.Source != "runner-shared" {
		t.Errorf("mount source = %q, want %q", sharedMount.Source, "runner-shared")
	}

	// Verify command wraps with chown
	cmd := strings.Join(call.config.Cmd, " ")
	if !strings.Contains(cmd, "sudo chown") {
		t.Errorf("cmd should contain sudo chown, got: %v", call.config.Cmd)
	}
	if !strings.Contains(cmd, "/home/runner/run.sh") {
		t.Errorf("cmd should contain run.sh, got: %v", call.config.Cmd)
	}

	// Verify SHARED_DIR environment variable
	foundSharedDir := false
	for _, env := range call.config.Env {
		if env == "SHARED_DIR=/shared" {
			foundSharedDir = true
			break
		}
	}
	if !foundSharedDir {
		t.Errorf("env should contain SHARED_DIR=/shared, got: %v", call.config.Env)
	}

	// Verify managed-by label
	if label := call.config.Labels["managed-by"]; label != "runner" {
		t.Errorf("label managed-by = %q, want %q", label, "runner")
	}
}

func TestDockerBackend_StartRunner_WithoutSharedVolume(t *testing.T) {
	b, md := newTestDockerBackend("", true)
	ctx := context.Background()

	_, err := b.StartRunner(ctx, "runner-1", "mock-jit-config")
	if err != nil {
		t.Fatalf("StartRunner() error: %v", err)
	}

	call := md.createCalls[0]

	// Docker socket mount should still be present
	dockerMount := findMountByTarget(call.hostConfig.Mounts, "/var/run/docker.sock")
	if dockerMount == nil {
		t.Fatal("docker socket mount not found")
	}

	// No shared volume mount
	sharedMount := findMountByTarget(call.hostConfig.Mounts, "/shared")
	if sharedMount != nil {
		t.Errorf("shared volume mount should not be present")
	}

	// Direct run.sh command without chown wrapper
	if len(call.config.Cmd) != 1 || call.config.Cmd[0] != "/home/runner/run.sh" {
		t.Errorf("cmd = %v, want [/home/runner/run.sh]", call.config.Cmd)
	}

	// SHARED_DIR should not be set
	for _, env := range call.config.Env {
		if strings.HasPrefix(env, "SHARED_DIR=") {
			t.Errorf("env should not contain SHARED_DIR, got: %v", call.config.Env)
		}
	}
}

func TestDockerBackend_StartRunner_MultipleShareVolume(t *testing.T) {
	b, md := newTestDockerBackend("/shared", true)
	ctx := context.Background()

	for i := range 3 {
		name := "runner-" + string(rune('a'+i))
		if _, err := b.StartRunner(ctx, name, "jit"); err != nil {
			t.Fatalf("StartRunner(%s) error: %v", name, err)
		}
	}

	if len(md.createCalls) != 3 {
		t.Fatalf("expected 3 create calls, got %d", len(md.createCalls))
	}

	for i, call := range md.createCalls {
		m := findMountByTarget(call.hostConfig.Mounts, "/shared")
		if m == nil {
			t.Fatalf("runner %d: shared volume mount not found", i)
		}
		if m.Source != "runner-shared" {
			t.Errorf("runner %d: mount source = %q, want %q", i, m.Source, "runner-shared")
		}
		if m.Type != mount.TypeVolume {
			t.Errorf("runner %d: mount type = %v, want %v", i, m.Type, mount.TypeVolume)
		}
	}
}

func TestDockerBackend_RemoveRunner(t *testing.T) {
	b, md := newTestDockerBackend("", true)
	ctx := context.Background()

	if err := b.RemoveRunner(ctx, "sha256-runner-1"); err != nil {
		t.Fatalf("RemoveRunner() error: %v", err)
	}
	if len(md.removed) != 1 || md.removed[0] != "sha256-runner-1" {
		t.Errorf("removed = %v, want [sha256-runner-1]", md.removed)
	}
}

func TestDockerBackend_Shutdown_IsNoOp(t *testing.T) {
	// Shared resources (volume, prune) are cleaned up once at process exit
	// via CleanupSharedDocker, not per backend. The per-backend Shutdown
	// must not touch shared state to avoid races between scale sets.
	b, md := newTestDockerBackend("/shared", true)
	ctx := context.Background()

	b.Shutdown(ctx)

	if len(md.volumesRemoved) != 0 {
		t.Errorf("Shutdown should not remove volumes, removed %d", len(md.volumesRemoved))
	}
}

func TestCleanupSharedDocker_RemovesVolume(t *testing.T) {
	md := &mockDocker{}
	ctx := context.Background()

	CleanupSharedDocker(ctx, md, true, true, slog.New(slog.DiscardHandler))

	if len(md.volumesRemoved) != 1 {
		t.Fatalf("expected 1 volume removed, got %d", len(md.volumesRemoved))
	}
	if md.volumesRemoved[0] != "runner-shared" {
		t.Errorf("volume removed = %q, want %q", md.volumesRemoved[0], "runner-shared")
	}
}

func TestCleanupSharedDocker_SkipsVolumeWhenDisabled(t *testing.T) {
	md := &mockDocker{}
	ctx := context.Background()

	CleanupSharedDocker(ctx, md, false, true, slog.New(slog.DiscardHandler))

	if len(md.volumesRemoved) != 0 {
		t.Errorf("should not remove volume when disabled, removed %d", len(md.volumesRemoved))
	}
}

func TestCleanupSharedDocker_WipesBuildCacheWhenRequested(t *testing.T) {
	md := &mockDocker{}

	CleanupSharedDocker(context.Background(), md, false, true, slog.New(slog.DiscardHandler))

	// Dangling images are always pruned; the full build-cache wipe runs too.
	if len(md.imagesPruneFilters) != 1 {
		t.Fatalf("expected 1 images prune, got %d", len(md.imagesPruneFilters))
	}
	if len(md.buildCachePruneOpts) != 1 {
		t.Fatalf("expected 1 build cache prune, got %d", len(md.buildCachePruneOpts))
	}
	got := md.buildCachePruneOpts[0]
	if !got.All || got.ReservedSpace != 0 || got.Filters.Len() != 0 {
		t.Errorf("build cache prune opts = %+v, want unfiltered All:true wipe", got)
	}
}

func TestCleanupSharedDocker_SkipsBuildCacheWipeWhenRuntimePruneOwnsIt(t *testing.T) {
	md := &mockDocker{}

	CleanupSharedDocker(context.Background(), md, false, false, slog.New(slog.DiscardHandler))

	// Dangling images are still pruned unconditionally...
	if len(md.imagesPruneFilters) != 1 {
		t.Fatalf("expected 1 images prune, got %d", len(md.imagesPruneFilters))
	}
	// ...but the build cache survives the restart for the next process run.
	if len(md.buildCachePruneOpts) != 0 {
		t.Errorf("expected no build cache prune, got %d", len(md.buildCachePruneOpts))
	}
}

func TestDockerBackend_BuildContainerEnv(t *testing.T) {
	tests := []struct {
		name         string
		sharedVolume string
		wantShared   bool
		wantPath     string
	}{
		{
			name:         "with shared volume",
			sharedVolume: "/shared",
			wantShared:   true,
			wantPath:     "/shared",
		},
		{
			name:         "with custom shared volume path",
			sharedVolume: "/data/shared",
			wantShared:   true,
			wantPath:     "/data/shared",
		},
		{
			name:         "without shared volume",
			sharedVolume: "",
			wantShared:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newTestDockerBackend(tt.sharedVolume, true)
			env := b.buildContainerEnv("test-jit-config")

			// Always contains JIT config
			foundJIT := false
			for _, e := range env {
				if e == "ACTIONS_RUNNER_INPUT_JITCONFIG=test-jit-config" {
					foundJIT = true
				}
			}
			if !foundJIT {
				t.Errorf("env should contain ACTIONS_RUNNER_INPUT_JITCONFIG, got: %v", env)
			}

			// Check SHARED_DIR presence
			foundShared := ""
			for _, e := range env {
				if strings.HasPrefix(e, "SHARED_DIR=") {
					foundShared = e
				}
			}
			if tt.wantShared {
				want := "SHARED_DIR=" + tt.wantPath
				if foundShared != want {
					t.Errorf("env SHARED_DIR = %q, want %q", foundShared, want)
				}
			} else if foundShared != "" {
				t.Errorf("env should not contain SHARED_DIR, got: %v", env)
			}
		})
	}
}

func TestDockerBackend_StartRunner_WithResourceLimits(t *testing.T) {
	memoryBytes := int64(8192) * 1024 * 1024 // 8GB
	nanoCPUs := int64(4) * 1_000_000_000     // 4 cores
	b, md := newTestDockerBackendWithResources(memoryBytes, nanoCPUs)
	ctx := context.Background()

	_, err := b.StartRunner(ctx, "runner-1", "mock-jit-config")
	if err != nil {
		t.Fatalf("StartRunner() error: %v", err)
	}

	call := md.createCalls[0]
	if call.hostConfig.Resources.Memory != memoryBytes {
		t.Errorf("Memory = %d, want %d", call.hostConfig.Resources.Memory, memoryBytes)
	}
	if call.hostConfig.Resources.NanoCPUs != nanoCPUs {
		t.Errorf("NanoCPUs = %d, want %d", call.hostConfig.Resources.NanoCPUs, nanoCPUs)
	}
}

func TestDockerBackend_StartRunner_WithoutResourceLimits(t *testing.T) {
	b, md := newTestDockerBackendWithResources(0, 0)
	ctx := context.Background()

	_, err := b.StartRunner(ctx, "runner-1", "mock-jit-config")
	if err != nil {
		t.Fatalf("StartRunner() error: %v", err)
	}

	call := md.createCalls[0]
	if call.hostConfig.Resources.Memory != 0 {
		t.Errorf("Memory = %d, want 0 (unlimited)", call.hostConfig.Resources.Memory)
	}
	if call.hostConfig.Resources.NanoCPUs != 0 {
		t.Errorf("NanoCPUs = %d, want 0 (unlimited)", call.hostConfig.Resources.NanoCPUs)
	}
}

func TestCleanupSharedVolumeStale_NoOpWhenTTLZero(t *testing.T) {
	md := &mockDocker{}
	if err := CleanupSharedVolumeStale(context.Background(), md, "img", "/shared", 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("CleanupSharedVolumeStale() error: %v", err)
	}
	if len(md.created) != 0 {
		t.Errorf("expected no container created, got %d", len(md.created))
	}
}

func TestCleanupSharedVolumeStale_RunsHelperContainer(t *testing.T) {
	md := &mockDocker{}
	logger := slog.New(slog.DiscardHandler)

	if err := CleanupSharedVolumeStale(context.Background(), md, "runner-img", "/shared", 7*24*time.Hour, logger); err != nil {
		t.Fatalf("CleanupSharedVolumeStale() error: %v", err)
	}

	if len(md.createCalls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(md.createCalls))
	}
	call := md.createCalls[0]

	// Helper container must use the runner image and run as root for delete perms.
	if call.config.Image != "runner-img" {
		t.Errorf("image = %q, want runner-img", call.config.Image)
	}
	if call.config.User != "root" {
		t.Errorf("user = %q, want root", call.config.User)
	}

	// Mount the shared named volume at the configured path.
	m := findMountByTarget(call.hostConfig.Mounts, "/shared")
	if m == nil {
		t.Fatal("shared volume mount not found")
	}
	if m.Type != mount.TypeVolume || m.Source != "runner-shared" {
		t.Errorf("mount = %+v, want named volume runner-shared", m)
	}

	// Script must reference the configured TTL in days and the mount path.
	cmd := strings.Join(call.config.Cmd, " ")
	if !strings.Contains(cmd, "-mtime +7") {
		t.Errorf("cmd should use -mtime +7, got: %q", cmd)
	}
	if !strings.Contains(cmd, "/shared") {
		t.Errorf("cmd should reference /shared, got: %q", cmd)
	}

	// Labels mark the helper for doctor / observability.
	if call.config.Labels["managed-by"] != "runner" {
		t.Errorf("missing managed-by label, got: %v", call.config.Labels)
	}
	if call.config.Labels["purpose"] != "shared-volume-cleanup" {
		t.Errorf("missing purpose label, got: %v", call.config.Labels)
	}

	// Container should be started and removed even on success.
	if len(md.started) != 1 {
		t.Errorf("expected 1 start, got %d", len(md.started))
	}
	if len(md.removed) != 1 {
		t.Errorf("expected 1 remove, got %d", len(md.removed))
	}
}

func TestCleanupSharedVolumeStale_RoundsSubDayTTLUp(t *testing.T) {
	md := &mockDocker{}
	if err := CleanupSharedVolumeStale(context.Background(), md, "img", "/shared", 6*time.Hour, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("CleanupSharedVolumeStale() error: %v", err)
	}
	cmd := strings.Join(md.createCalls[0].config.Cmd, " ")
	if !strings.Contains(cmd, "-mtime +1") {
		t.Errorf("sub-day TTL should round up to -mtime +1, got: %q", cmd)
	}
}

func TestCleanupSharedVolumeStale_PropagatesNonZeroExit(t *testing.T) {
	md := &mockDocker{waitStatus: 2}
	err := CleanupSharedVolumeStale(context.Background(), md, "img", "/shared", time.Hour, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "status 2") {
		t.Errorf("error should mention status 2, got: %v", err)
	}
	// Container must still be cleaned up on failure.
	if len(md.removed) != 1 {
		t.Errorf("expected container removed after failure, got %d removes", len(md.removed))
	}
}

func TestCleanupSharedVolumeStale_PropagatesWaitError(t *testing.T) {
	md := &mockDocker{waitErr: errors.New("docker died")}
	err := CleanupSharedVolumeStale(context.Background(), md, "img", "/shared", time.Hour, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "docker died") {
		t.Errorf("expected wait error to propagate, got: %v", err)
	}
}

func TestCleanupOrphanedBuildxBuilders_NoOpWhenMaxAgeZero(t *testing.T) {
	md := &mockDocker{containers: []container.Summary{
		{ID: "old", Names: []string{"/buildx_buildkit_builder-abc0"}, Created: 0},
	}}
	if err := CleanupOrphanedBuildxBuilders(context.Background(), md, 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(md.removed) != 0 {
		t.Errorf("expected nothing removed, got %d", len(md.removed))
	}
}

func TestCleanupOrphanedBuildxBuilders_RemovesOldKeepsYoungAndOthers(t *testing.T) {
	now := time.Now()
	md := &mockDocker{containers: []container.Summary{
		{ID: "old", Names: []string{"/buildx_buildkit_builder-old0"}, Created: now.Add(-48 * time.Hour).Unix()},
		{ID: "young", Names: []string{"/buildx_buildkit_builder-young0"}, Created: now.Add(-1 * time.Hour).Unix()},
		{ID: "runner", Names: []string{"/runner-deadbeef"}, Created: now.Add(-72 * time.Hour).Unix()},
	}}

	if err := CleanupOrphanedBuildxBuilders(context.Background(), md, 24*time.Hour, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("error: %v", err)
	}

	// Only the old buildx builder container is removed.
	if len(md.removed) != 1 || md.removed[0] != "old" {
		t.Errorf("expected only 'old' removed, got %v", md.removed)
	}
	// Its state volume is removed explicitly.
	if len(md.volumesRemoved) != 1 || md.volumesRemoved[0] != "buildx_buildkit_builder-old0_state" {
		t.Errorf("expected old builder's _state volume removed, got %v", md.volumesRemoved)
	}
}

func TestCleanupOrphanedBuildxBuilders_ReapsDanglingVolumes(t *testing.T) {
	md := &mockDocker{volumes: []*volume.Volume{
		{Name: "buildx_buildkit_builder-gone0_state"},
		{Name: "runner-shared"}, // must be left untouched
	}}

	if err := CleanupOrphanedBuildxBuilders(context.Background(), md, 24*time.Hour, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("error: %v", err)
	}

	if len(md.volumesRemoved) != 1 || md.volumesRemoved[0] != "buildx_buildkit_builder-gone0_state" {
		t.Errorf("expected only dangling buildx volume removed, got %v", md.volumesRemoved)
	}
}

func TestPruneDockerRuntime(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	tests := []struct {
		name        string
		ttl         time.Duration
		cacheMaxAge time.Duration
		budgetGB    int

		// Expected recorded calls, in order; nil means the prune must not run.
		wantContainersFilters []filters.Args
		wantImagesFilters     []filters.Args
		wantCacheOpts         []build.CachePruneOptions
	}{
		{
			name:        "all portions enabled",
			ttl:         24 * time.Hour,
			cacheMaxAge: 7 * 24 * time.Hour,
			budgetGB:    20,
			wantContainersFilters: []filters.Args{
				filters.NewArgs(filters.Arg("until", "24h0m0s")),
			},
			wantImagesFilters: []filters.Args{
				filters.NewArgs(filters.Arg("dangling", "true"), filters.Arg("until", "24h0m0s")),
			},
			wantCacheOpts: []build.CachePruneOptions{
				{All: true, Filters: filters.NewArgs(filters.Arg("until", "168h0m0s"))},
				{All: true, ReservedSpace: 20 * gb},
			},
		},
		{
			name:        "ttl zero skips containers and images",
			ttl:         0,
			cacheMaxAge: 48 * time.Hour,
			wantCacheOpts: []build.CachePruneOptions{
				{All: true, Filters: filters.NewArgs(filters.Arg("until", "48h0m0s"))},
			},
		},
		{
			name:        "negative ttl skips containers and images",
			ttl:         -time.Hour,
			cacheMaxAge: 48 * time.Hour,
			wantCacheOpts: []build.CachePruneOptions{
				{All: true, Filters: filters.NewArgs(filters.Arg("until", "48h0m0s"))},
			},
		},
		{
			name:     "cacheMaxAge zero skips age-based cache prune",
			ttl:      time.Hour,
			budgetGB: 5,
			wantContainersFilters: []filters.Args{
				filters.NewArgs(filters.Arg("until", "1h0m0s")),
			},
			wantImagesFilters: []filters.Args{
				filters.NewArgs(filters.Arg("dangling", "true"), filters.Arg("until", "1h0m0s")),
			},
			wantCacheOpts: []build.CachePruneOptions{
				{All: true, ReservedSpace: 5 * gb},
			},
		},
		{
			name:        "budget zero skips budget prune",
			ttl:         time.Hour,
			cacheMaxAge: time.Hour,
			budgetGB:    0,
			wantContainersFilters: []filters.Args{
				filters.NewArgs(filters.Arg("until", "1h0m0s")),
			},
			wantImagesFilters: []filters.Args{
				filters.NewArgs(filters.Arg("dangling", "true"), filters.Arg("until", "1h0m0s")),
			},
			wantCacheOpts: []build.CachePruneOptions{
				{All: true, Filters: filters.NewArgs(filters.Arg("until", "1h0m0s"))},
			},
		},
		{
			name: "everything disabled prunes nothing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := &mockDocker{}

			err := PruneDockerRuntime(context.Background(), md, tt.ttl, tt.cacheMaxAge, tt.budgetGB, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("PruneDockerRuntime() error: %v", err)
			}

			if !reflect.DeepEqual(md.containersPruneFilters, tt.wantContainersFilters) {
				t.Errorf("containers prune filters = %+v, want %+v", md.containersPruneFilters, tt.wantContainersFilters)
			}
			if !reflect.DeepEqual(md.imagesPruneFilters, tt.wantImagesFilters) {
				t.Errorf("images prune filters = %+v, want %+v", md.imagesPruneFilters, tt.wantImagesFilters)
			}
			if !reflect.DeepEqual(md.buildCachePruneOpts, tt.wantCacheOpts) {
				t.Errorf("build cache prune opts = %+v, want %+v", md.buildCachePruneOpts, tt.wantCacheOpts)
			}
		})
	}
}

func TestPruneDockerRuntime_ContinuesAfterPruneErrors(t *testing.T) {
	md := &mockDocker{
		containersPruneErr: errors.New("containers boom"),
		imagesPruneErr:     errors.New("images boom"),
	}

	err := PruneDockerRuntime(context.Background(), md, time.Hour, time.Hour, 1, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected joined error, got nil")
	}
	// Both failures surface in the joined error...
	if !strings.Contains(err.Error(), "containers boom") || !strings.Contains(err.Error(), "images boom") {
		t.Errorf("error should include both prune failures, got: %v", err)
	}
	// ...and the failures did not stop the later prunes: the images prune ran
	// after the containers failure, and both build cache prunes still ran.
	if len(md.imagesPruneFilters) != 1 {
		t.Errorf("expected images prune to run after containers failure, got %d calls", len(md.imagesPruneFilters))
	}
	if len(md.buildCachePruneOpts) != 2 {
		t.Errorf("expected both build cache prunes to run, got %d calls", len(md.buildCachePruneOpts))
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input uint64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{1073741824, "1.0 GiB"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatBytes(tt.input)
			if got != tt.want {
				t.Errorf("FormatBytes(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
