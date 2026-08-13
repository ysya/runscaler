package backend

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/api/types/volume"
	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/config"
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
	volumes    []volume.Volume

	// Recorded prune calls, in order, for CleanupSharedDocker assertions.
	// containersPruneFilters is unused since PruneDockerRuntime's removal
	// (its reclaim logic now lives in internal/cachestore's
	// dockerGarbageStore, covered by that package's own tests) — kept for
	// symmetry with imagesPruneFilters/buildCachePruneOpts, which
	// CleanupSharedDocker's tests still exercise.
	containersPruneFilters []dockerclient.Filters
	imagesPruneFilters     []dockerclient.Filters
	buildCachePruneOpts    []dockerclient.BuildCachePruneOptions

	// Optional error injection for the prune calls.
	containersPruneErr error
	imagesPruneErr     error
	buildCachePruneErr error

	// When true, VolumeRemove blocks until its context is done, simulating a
	// wedged daemon that never answers.
	volumeRemoveBlocks bool
}

func (m *mockDocker) ContainerCreate(_ context.Context, options dockerclient.ContainerCreateOptions) (dockerclient.ContainerCreateResult, error) {
	id := "sha256-" + options.Name
	m.created = append(m.created, options.Name)
	m.createCalls = append(m.createCalls, createCall{name: options.Name, config: options.Config, hostConfig: options.HostConfig})
	return dockerclient.ContainerCreateResult{ID: id}, nil
}

func (m *mockDocker) ContainerStart(_ context.Context, id string, _ dockerclient.ContainerStartOptions) (dockerclient.ContainerStartResult, error) {
	m.started = append(m.started, id)
	return dockerclient.ContainerStartResult{}, nil
}

func (m *mockDocker) ContainerRemove(_ context.Context, id string, _ dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error) {
	m.removed = append(m.removed, id)
	return dockerclient.ContainerRemoveResult{}, nil
}

func (m *mockDocker) ContainerPrune(_ context.Context, options dockerclient.ContainerPruneOptions) (dockerclient.ContainerPruneResult, error) {
	m.containersPruneFilters = append(m.containersPruneFilters, options.Filters)
	if m.containersPruneErr != nil {
		return dockerclient.ContainerPruneResult{}, m.containersPruneErr
	}
	return dockerclient.ContainerPruneResult{}, nil
}

func (m *mockDocker) ImagePrune(_ context.Context, options dockerclient.ImagePruneOptions) (dockerclient.ImagePruneResult, error) {
	m.imagesPruneFilters = append(m.imagesPruneFilters, options.Filters)
	if m.imagesPruneErr != nil {
		return dockerclient.ImagePruneResult{}, m.imagesPruneErr
	}
	return dockerclient.ImagePruneResult{}, nil
}

func (m *mockDocker) BuildCachePrune(_ context.Context, opts dockerclient.BuildCachePruneOptions) (dockerclient.BuildCachePruneResult, error) {
	m.buildCachePruneOpts = append(m.buildCachePruneOpts, opts)
	if m.buildCachePruneErr != nil {
		return dockerclient.BuildCachePruneResult{}, m.buildCachePruneErr
	}
	return dockerclient.BuildCachePruneResult{}, nil
}

func (m *mockDocker) VolumeRemove(ctx context.Context, volumeID string, _ dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error) {
	m.volumesRemoved = append(m.volumesRemoved, volumeID)
	if m.volumeRemoveBlocks {
		<-ctx.Done()
		return dockerclient.VolumeRemoveResult{}, ctx.Err()
	}
	return dockerclient.VolumeRemoveResult{}, nil
}

func (m *mockDocker) ContainerList(_ context.Context, _ dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	return dockerclient.ContainerListResult{Items: m.containers}, nil
}

func (m *mockDocker) VolumeList(_ context.Context, _ dockerclient.VolumeListOptions) (dockerclient.VolumeListResult, error) {
	return dockerclient.VolumeListResult{Items: m.volumes}, nil
}

func (m *mockDocker) Info(_ context.Context, _ dockerclient.InfoOptions) (dockerclient.SystemInfoResult, error) {
	return dockerclient.SystemInfoResult{Info: system.Info{DockerRootDir: "/var/lib/docker"}}, nil
}

func (m *mockDocker) DiskUsage(_ context.Context, _ dockerclient.DiskUsageOptions) (dockerclient.DiskUsageResult, error) {
	return dockerclient.DiskUsageResult{}, nil
}

// ContainerLogs is unused by this package's own tests (only
// internal/cachestore's volume helper reads logs) — a minimal stub keeps
// mockDocker satisfying DockerAPI.
func (m *mockDocker) ContainerLogs(_ context.Context, _ string, _ dockerclient.ContainerLogsOptions) (dockerclient.ContainerLogsResult, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (m *mockDocker) ContainerWait(_ context.Context, _ string, _ dockerclient.ContainerWaitOptions) dockerclient.ContainerWaitResult {
	statusCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	if m.waitErr != nil {
		errCh <- m.waitErr
	} else {
		statusCh <- container.WaitResponse{StatusCode: m.waitStatus}
	}
	return dockerclient.ContainerWaitResult{Result: statusCh, Error: errCh}
}

func newTestDockerBackend(sharedVolume string, dind bool) (*DockerBackend, *mockDocker) {
	md := &mockDocker{}
	b := &DockerBackend{
		dockerClient:     md,
		runnerImage:      "test-image:latest",
		dockerSocket:     "/var/run/docker.sock",
		dind:             dind,
		sharedVolume:     sharedVolume,
		sharedVolumeName: "runner-shared",
		logger:           slog.New(slog.DiscardHandler),
	}
	return b, md
}

// newConfigDockerBackend builds a backend through NewDockerBackend so the
// constructor path (volume-name default, cache-volume parsing) is exercised.
// DinD is disabled to keep tests hermetic (no host socket stat).
func newConfigDockerBackend(dc config.DockerConfig) (*DockerBackend, *mockDocker) {
	md := &mockDocker{}
	dind := false
	dc.DinD = &dind
	ss := config.ScaleSetConfig{
		RunnerImage: "test-image:latest",
		Docker:      dc,
	}
	return NewDockerBackend(ss, md, slog.New(slog.DiscardHandler)), md
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

// assertFixOwnCmd checks that cmd is a shell wrapper whose conditional-chown
// prelude covers exactly the given volume mount targets before exec'ing the
// runner. The chown must be gated on the mount point's owner so warm volumes
// skip the recursive IO storm.
func assertFixOwnCmd(t *testing.T, cmd []string, targets ...string) {
	t.Helper()
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("cmd = %v, want [sh -c <script>]", cmd)
	}
	script := cmd[2]
	if !strings.Contains(script, `[ "$(stat -c %u "$1")" = "1001" ] || sudo chown -R 1001:123 "$1"`) {
		t.Errorf("script should chown only when the owner is wrong, got: %q", script)
	}
	for _, target := range targets {
		if !strings.Contains(script, "fix_own "+shellQuote(target)+";") {
			t.Errorf("script should fix ownership of %s, got: %q", target, script)
		}
	}
	// The definition is "fix_own()" (no space), so "fix_own " counts calls only.
	if got := strings.Count(script, "fix_own "); got != len(targets) {
		t.Errorf("script has %d fix_own calls, want %d: %q", got, len(targets), script)
	}
	if !strings.HasSuffix(script, "exec /home/runner/run.sh") {
		t.Errorf("script should end with exec run.sh, got: %q", script)
	}
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

	// Verify command wraps with the conditional-chown prelude
	assertFixOwnCmd(t, call.config.Cmd, "/shared")

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

func TestDockerBackend_StartRunner_WithCacheVolumes(t *testing.T) {
	b, md := newConfigDockerBackend(config.DockerConfig{
		SharedVolume: "/shared",
		CacheVolumes: []string{
			"gradle-cache:/home/runner/.gradle",
			"pnpm-store:/home/runner/.local/share/pnpm/store",
		},
	})

	if _, err := b.StartRunner(context.Background(), "runner-1", "jit"); err != nil {
		t.Fatalf("StartRunner() error: %v", err)
	}
	call := md.createCalls[0]

	// Shared volume mount uses the default volume name.
	shared := findMountByTarget(call.hostConfig.Mounts, "/shared")
	if shared == nil {
		t.Fatal("shared volume mount not found")
	}
	if shared.Type != mount.TypeVolume || shared.Source != "runner-shared" {
		t.Errorf("shared mount = %+v, want named volume runner-shared", shared)
	}

	// Each cache volume is mounted as a named volume at its path.
	wantCaches := map[string]string{
		"/home/runner/.gradle":                 "gradle-cache",
		"/home/runner/.local/share/pnpm/store": "pnpm-store",
	}
	for target, source := range wantCaches {
		m := findMountByTarget(call.hostConfig.Mounts, target)
		if m == nil {
			t.Fatalf("cache volume mount %s not found", target)
		}
		if m.Type != mount.TypeVolume || m.Source != source {
			t.Errorf("cache mount %s = %+v, want named volume %s", target, m, source)
		}
	}
	// No DinD → shared + 2 cache mounts and nothing else.
	if len(call.hostConfig.Mounts) != 3 {
		t.Errorf("mounts = %d, want 3", len(call.hostConfig.Mounts))
	}

	// The fix_own prelude must cover every volume mount target.
	assertFixOwnCmd(t, call.config.Cmd,
		"/shared", "/home/runner/.gradle", "/home/runner/.local/share/pnpm/store")
}

func TestDockerBackend_StartRunner_CacheVolumesWithoutSharedVolume(t *testing.T) {
	b, md := newConfigDockerBackend(config.DockerConfig{
		CacheVolumes: []string{"go-build-cache:/home/runner/.cache/go-build"},
	})

	if _, err := b.StartRunner(context.Background(), "runner-1", "jit"); err != nil {
		t.Fatalf("StartRunner() error: %v", err)
	}
	call := md.createCalls[0]

	m := findMountByTarget(call.hostConfig.Mounts, "/home/runner/.cache/go-build")
	if m == nil {
		t.Fatal("cache volume mount not found")
	}
	if m.Type != mount.TypeVolume || m.Source != "go-build-cache" {
		t.Errorf("cache mount = %+v, want named volume go-build-cache", m)
	}

	// The prelude covers the cache path even without a shared volume.
	assertFixOwnCmd(t, call.config.Cmd, "/home/runner/.cache/go-build")

	// SHARED_DIR stays unset without a shared volume.
	for _, env := range call.config.Env {
		if strings.HasPrefix(env, "SHARED_DIR=") {
			t.Errorf("env should not contain SHARED_DIR, got: %v", call.config.Env)
		}
	}
}

func TestNewDockerBackend_InvalidCacheVolumesMountsNone(t *testing.T) {
	// Validate() rejects such a config before startup; if a backend is built
	// anyway, the bad list must yield no cache mounts rather than a partial set.
	b, md := newConfigDockerBackend(config.DockerConfig{
		CacheVolumes: []string{"gradle-cache:/home/runner/.gradle", "not-an-entry"},
	})

	if _, err := b.StartRunner(context.Background(), "runner-1", "jit"); err != nil {
		t.Fatalf("StartRunner() error: %v", err)
	}
	call := md.createCalls[0]

	if len(call.hostConfig.Mounts) != 0 {
		t.Errorf("mounts = %+v, want none for an invalid cache-volumes list", call.hostConfig.Mounts)
	}
	// No volume mounts → the bare cmd, no shell wrapper.
	if len(call.config.Cmd) != 1 || call.config.Cmd[0] != "/home/runner/run.sh" {
		t.Errorf("cmd = %v, want [/home/runner/run.sh]", call.config.Cmd)
	}
}

func TestDockerBackend_StartRunner_CustomSharedVolumeName(t *testing.T) {
	b, md := newConfigDockerBackend(config.DockerConfig{
		SharedVolume:     "/shared",
		SharedVolumeName: "team-a-shared",
	})

	if _, err := b.StartRunner(context.Background(), "runner-1", "jit"); err != nil {
		t.Fatalf("StartRunner() error: %v", err)
	}

	m := findMountByTarget(md.createCalls[0].hostConfig.Mounts, "/shared")
	if m == nil {
		t.Fatal("shared volume mount not found")
	}
	if m.Source != "team-a-shared" {
		t.Errorf("mount source = %q, want %q", m.Source, "team-a-shared")
	}
}

func TestDockerBackend_StartRunner_NetworkMode(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		b, md := newConfigDockerBackend(config.DockerConfig{Network: "runners"})
		if _, err := b.StartRunner(context.Background(), "runner-1", "jit"); err != nil {
			t.Fatalf("StartRunner() error: %v", err)
		}
		if got := md.createCalls[0].hostConfig.NetworkMode; got != "runners" {
			t.Errorf("NetworkMode = %q, want %q", got, "runners")
		}
	})

	t.Run("unset keeps daemon default", func(t *testing.T) {
		b, md := newConfigDockerBackend(config.DockerConfig{})
		if _, err := b.StartRunner(context.Background(), "runner-1", "jit"); err != nil {
			t.Fatalf("StartRunner() error: %v", err)
		}
		if got := md.createCalls[0].hostConfig.NetworkMode; got != "" {
			t.Errorf("NetworkMode = %q, want empty (daemon default)", got)
		}
	})
}

func TestDockerBackend_StartRunner_PidsLimit(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		b, md := newConfigDockerBackend(config.DockerConfig{PidsLimit: 4096})
		if _, err := b.StartRunner(context.Background(), "runner-1", "jit"); err != nil {
			t.Fatalf("StartRunner() error: %v", err)
		}
		got := md.createCalls[0].hostConfig.Resources.PidsLimit
		if got == nil || *got != 4096 {
			t.Errorf("PidsLimit = %v, want 4096", got)
		}
	})

	t.Run("unset leaves limit nil", func(t *testing.T) {
		b, md := newConfigDockerBackend(config.DockerConfig{})
		if _, err := b.StartRunner(context.Background(), "runner-1", "jit"); err != nil {
			t.Fatalf("StartRunner() error: %v", err)
		}
		if got := md.createCalls[0].hostConfig.Resources.PidsLimit; got != nil {
			t.Errorf("PidsLimit = %v, want nil (unlimited)", *got)
		}
	})
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

	CleanupSharedDocker(ctx, md, []string{"runner-shared"}, true, slog.New(slog.DiscardHandler))

	if len(md.volumesRemoved) != 1 {
		t.Fatalf("expected 1 volume removed, got %d", len(md.volumesRemoved))
	}
	if md.volumesRemoved[0] != "runner-shared" {
		t.Errorf("volume removed = %q, want %q", md.volumesRemoved[0], "runner-shared")
	}
}

func TestCleanupSharedDocker_RemovesMultipleVolumes(t *testing.T) {
	md := &mockDocker{}
	ctx := context.Background()

	CleanupSharedDocker(ctx, md, []string{"runner-shared", "team-a-shared"}, true, slog.New(slog.DiscardHandler))

	want := []string{"runner-shared", "team-a-shared"}
	if !reflect.DeepEqual(md.volumesRemoved, want) {
		t.Errorf("volumes removed = %v, want %v", md.volumesRemoved, want)
	}
}

func TestCleanupSharedDocker_SkipsVolumesWhenNoneNamed(t *testing.T) {
	md := &mockDocker{}
	ctx := context.Background()

	CleanupSharedDocker(ctx, md, nil, true, slog.New(slog.DiscardHandler))

	if len(md.volumesRemoved) != 0 {
		t.Errorf("should not remove volumes when none are named, removed %d", len(md.volumesRemoved))
	}
}

func TestCleanupSharedDocker_WipesBuildCacheWhenRequested(t *testing.T) {
	md := &mockDocker{}

	CleanupSharedDocker(context.Background(), md, nil, true, slog.New(slog.DiscardHandler))

	// Dangling images are always pruned; the full build-cache wipe runs too.
	if len(md.imagesPruneFilters) != 1 {
		t.Fatalf("expected 1 images prune, got %d", len(md.imagesPruneFilters))
	}
	if len(md.buildCachePruneOpts) != 1 {
		t.Fatalf("expected 1 build cache prune, got %d", len(md.buildCachePruneOpts))
	}
	got := md.buildCachePruneOpts[0]
	if !got.All || got.MaxUsedSpace != 0 || len(got.Filters) != 0 {
		t.Errorf("build cache prune opts = %+v, want unfiltered All:true wipe", got)
	}
}

func TestCleanupSharedDocker_SkipsDaemonPruneWhenNotRequested(t *testing.T) {
	md := &mockDocker{}

	CleanupSharedDocker(context.Background(), md, nil, false, slog.New(slog.DiscardHandler))

	if len(md.imagesPruneFilters) != 0 {
		t.Fatalf("expected no images prune, got %d", len(md.imagesPruneFilters))
	}
	if len(md.buildCachePruneOpts) != 0 {
		t.Errorf("expected no build cache prune, got %d", len(md.buildCachePruneOpts))
	}
}

func TestCleanupSharedDocker_BoundedWhenDaemonWedges(t *testing.T) {
	// A daemon that never answers VolumeRemove used to hang shutdown forever:
	// the exit-time cleanup ran on a context with no deadline.
	md := &mockDocker{volumeRemoveBlocks: true}

	done := make(chan struct{})
	go func() {
		defer close(done)
		cleanupSharedDockerWith(context.Background(), md, []string{"runner-shared"},
			true, 50*time.Millisecond, slog.New(slog.DiscardHandler))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup never returned — a wedged daemon still hangs shutdown")
	}
}

func TestCleanupSharedDockerTimeout_FitsSystemdStopWindow(t *testing.T) {
	// systemd's default TimeoutStopSec is 90s and the scale set + scaler
	// shutdowns may already have spent ~40s before cleanup starts. Raising
	// this constant past the remainder means systemd SIGKILLs runner mid
	// cleanup instead of letting it exit on its own terms.
	if cleanupSharedDockerTimeout > 45*time.Second {
		t.Errorf("cleanupSharedDockerTimeout = %s, want <= 45s so shutdown fits systemd's 90s stop window",
			cleanupSharedDockerTimeout)
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
	md := &mockDocker{volumes: []volume.Volume{
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
