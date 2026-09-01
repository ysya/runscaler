package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
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
	removeErr  error

	// Optional fixtures for ContainerList / VolumeList.
	containers []container.Summary
	volumes    []volume.Volume
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
	return dockerclient.ContainerRemoveResult{}, m.removeErr
}

// The three prune methods exist only to satisfy DockerAPI. Nothing in this
// package prunes any more: reclamation moved to internal/cachestore, whose
// own fakeDockerAPI records and injects errors into these calls.

func (m *mockDocker) ContainerPrune(_ context.Context, _ dockerclient.ContainerPruneOptions) (dockerclient.ContainerPruneResult, error) {
	return dockerclient.ContainerPruneResult{}, nil
}

func (m *mockDocker) ImagePrune(_ context.Context, _ dockerclient.ImagePruneOptions) (dockerclient.ImagePruneResult, error) {
	return dockerclient.ImagePruneResult{}, nil
}

func (m *mockDocker) BuildCachePrune(_ context.Context, _ dockerclient.BuildCachePruneOptions) (dockerclient.BuildCachePruneResult, error) {
	return dockerclient.BuildCachePruneResult{}, nil
}

func (m *mockDocker) VolumeRemove(_ context.Context, volumeID string, _ dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error) {
	m.volumesRemoved = append(m.volumesRemoved, volumeID)
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

func newTestDockerProvider(sharedVolume string, dind bool) (*DockerProvider, *mockDocker) {
	md := &mockDocker{}
	b := &DockerProvider{
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

// newConfigDockerProvider builds a provider through NewDockerProvider so the
// constructor path (volume-name default, cache-volume parsing) is exercised.
// DinD is disabled to keep tests hermetic (no host socket stat).
func newConfigDockerProvider(dc config.DockerConfig) (*DockerProvider, *mockDocker) {
	md := &mockDocker{}
	dind := false
	dc.DinD = &dind
	ss := config.ScaleSetConfig{
		RunnerImage: "test-image:latest",
		Docker:      dc,
	}
	return NewDockerProvider(ss, md, slog.New(slog.DiscardHandler)), md
}

func newTestDockerProviderWithResources(memory int64, cpu int64) (*DockerProvider, *mockDocker) {
	md := &mockDocker{}
	b := &DockerProvider{
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

// --- Docker provider tests ---

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

func TestDockerProvider_StartInstance_WithSharedVolume(t *testing.T) {
	b, md := newTestDockerProvider("/shared", true)
	ctx := context.Background()

	instanceID, err := b.StartInstance(ctx, "runner-1", "mock-jit-config")
	if err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}
	if instanceID != "sha256-runner-1" {
		t.Errorf("instanceID = %q, want %q", instanceID, "sha256-runner-1")
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

func TestDockerProvider_StartInstance_WithoutSharedVolume(t *testing.T) {
	b, md := newTestDockerProvider("", true)
	ctx := context.Background()

	_, err := b.StartInstance(ctx, "runner-1", "mock-jit-config")
	if err != nil {
		t.Fatalf("StartInstance() error: %v", err)
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

func TestDockerProvider_StartInstance_MultipleShareVolume(t *testing.T) {
	b, md := newTestDockerProvider("/shared", true)
	ctx := context.Background()

	for i := range 3 {
		name := "runner-" + string(rune('a'+i))
		if _, err := b.StartInstance(ctx, name, "jit"); err != nil {
			t.Fatalf("StartInstance(%s) error: %v", name, err)
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

func TestDockerProvider_StartInstance_WithCacheVolumes(t *testing.T) {
	b, md := newConfigDockerProvider(config.DockerConfig{
		SharedVolume: "/shared",
		CacheVolumes: []string{
			"gradle-cache:/home/runner/.gradle",
			"pnpm-store:/home/runner/.local/share/pnpm/store",
		},
	})

	if _, err := b.StartInstance(context.Background(), "runner-1", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
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

func TestDockerProvider_StartInstance_CacheVolumesWithoutSharedVolume(t *testing.T) {
	b, md := newConfigDockerProvider(config.DockerConfig{
		CacheVolumes: []string{"go-build-cache:/home/runner/.cache/go-build"},
	})

	if _, err := b.StartInstance(context.Background(), "runner-1", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
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

func TestNewDockerProvider_InvalidCacheVolumesMountsNone(t *testing.T) {
	// Validate() rejects such a config before startup; if a provider is built
	// anyway, the bad list must yield no cache mounts rather than a partial set.
	b, md := newConfigDockerProvider(config.DockerConfig{
		CacheVolumes: []string{"gradle-cache:/home/runner/.gradle", "not-an-entry"},
	})

	if _, err := b.StartInstance(context.Background(), "runner-1", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
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

func TestDockerProvider_StartInstance_CustomSharedVolumeName(t *testing.T) {
	b, md := newConfigDockerProvider(config.DockerConfig{
		SharedVolume:     "/shared",
		SharedVolumeName: "team-a-shared",
	})

	if _, err := b.StartInstance(context.Background(), "runner-1", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}

	m := findMountByTarget(md.createCalls[0].hostConfig.Mounts, "/shared")
	if m == nil {
		t.Fatal("shared volume mount not found")
	}
	if m.Source != "team-a-shared" {
		t.Errorf("mount source = %q, want %q", m.Source, "team-a-shared")
	}
}

func TestDockerProvider_StartInstance_NetworkMode(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		b, md := newConfigDockerProvider(config.DockerConfig{Network: "runners"})
		if _, err := b.StartInstance(context.Background(), "runner-1", "jit"); err != nil {
			t.Fatalf("StartInstance() error: %v", err)
		}
		if got := md.createCalls[0].hostConfig.NetworkMode; got != "runners" {
			t.Errorf("NetworkMode = %q, want %q", got, "runners")
		}
	})

	t.Run("unset keeps daemon default", func(t *testing.T) {
		b, md := newConfigDockerProvider(config.DockerConfig{})
		if _, err := b.StartInstance(context.Background(), "runner-1", "jit"); err != nil {
			t.Fatalf("StartInstance() error: %v", err)
		}
		if got := md.createCalls[0].hostConfig.NetworkMode; got != "" {
			t.Errorf("NetworkMode = %q, want empty (daemon default)", got)
		}
	})
}

func TestDockerProvider_StartInstance_PidsLimit(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		b, md := newConfigDockerProvider(config.DockerConfig{PidsLimit: 4096})
		if _, err := b.StartInstance(context.Background(), "runner-1", "jit"); err != nil {
			t.Fatalf("StartInstance() error: %v", err)
		}
		got := md.createCalls[0].hostConfig.Resources.PidsLimit
		if got == nil || *got != 4096 {
			t.Errorf("PidsLimit = %v, want 4096", got)
		}
	})

	t.Run("unset leaves limit nil", func(t *testing.T) {
		b, md := newConfigDockerProvider(config.DockerConfig{})
		if _, err := b.StartInstance(context.Background(), "runner-1", "jit"); err != nil {
			t.Fatalf("StartInstance() error: %v", err)
		}
		if got := md.createCalls[0].hostConfig.Resources.PidsLimit; got != nil {
			t.Errorf("PidsLimit = %v, want nil (unlimited)", *got)
		}
	})
}

func TestDockerProvider_RemoveInstance(t *testing.T) {
	b, md := newTestDockerProvider("", true)
	ctx := context.Background()

	if err := b.RemoveInstance(ctx, "sha256-runner-1"); err != nil {
		t.Fatalf("RemoveInstance() error: %v", err)
	}
	if len(md.removed) != 1 || md.removed[0] != "sha256-runner-1" {
		t.Errorf("removed = %v, want [sha256-runner-1]", md.removed)
	}
}

func TestDockerProvider_RemoveInstanceAlreadyAbsent(t *testing.T) {
	b, md := newTestDockerProvider("", false)
	md.removeErr = fmt.Errorf("container gone: %w", cerrdefs.ErrNotFound)
	if err := b.RemoveInstance(context.Background(), "missing"); err != nil {
		t.Fatalf("RemoveInstance() error for absent container: %v", err)
	}
	if !errors.Is(md.removeErr, cerrdefs.ErrNotFound) {
		t.Fatal("test fixture is not a containerd not-found error")
	}
}

func TestDockerProvider_Shutdown_IsNoOp(t *testing.T) {
	// Nothing shared (the volume, the daemon's images and build cache) is
	// reclaimed at exit at all — see Shutdown's doc comment. Reclaiming any
	// of it per provider would also race the other scale sets sharing this
	// Docker client.
	b, md := newTestDockerProvider("/shared", true)
	ctx := context.Background()

	b.Shutdown(ctx)

	if len(md.volumesRemoved) != 0 {
		t.Errorf("Shutdown should not remove volumes, removed %d", len(md.volumesRemoved))
	}
}

func TestDockerProvider_BuildContainerEnv(t *testing.T) {
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
			b, _ := newTestDockerProvider(tt.sharedVolume, true)
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

func TestDockerProvider_StartInstance_WithResourceLimits(t *testing.T) {
	memoryBytes := int64(8192) * 1024 * 1024 // 8GB
	nanoCPUs := int64(4) * 1_000_000_000     // 4 cores
	b, md := newTestDockerProviderWithResources(memoryBytes, nanoCPUs)
	ctx := context.Background()

	_, err := b.StartInstance(ctx, "runner-1", "mock-jit-config")
	if err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}

	call := md.createCalls[0]
	if call.hostConfig.Resources.Memory != memoryBytes {
		t.Errorf("Memory = %d, want %d", call.hostConfig.Resources.Memory, memoryBytes)
	}
	if call.hostConfig.Resources.NanoCPUs != nanoCPUs {
		t.Errorf("NanoCPUs = %d, want %d", call.hostConfig.Resources.NanoCPUs, nanoCPUs)
	}
}

func TestDockerProvider_StartInstance_WithoutResourceLimits(t *testing.T) {
	b, md := newTestDockerProviderWithResources(0, 0)
	ctx := context.Background()

	_, err := b.StartInstance(ctx, "runner-1", "mock-jit-config")
	if err != nil {
		t.Fatalf("StartInstance() error: %v", err)
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
