package backend

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
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

func (m *mockDocker) ImagesPrune(_ context.Context, _ filters.Args) (image.PruneReport, error) {
	return image.PruneReport{}, nil
}

func (m *mockDocker) BuildCachePrune(_ context.Context, _ build.CachePruneOptions) (*build.CachePruneReport, error) {
	return &build.CachePruneReport{}, nil
}

func (m *mockDocker) VolumeRemove(_ context.Context, volumeID string, _ bool) error {
	m.volumesRemoved = append(m.volumesRemoved, volumeID)
	return nil
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
	if sharedMount.Source != "runscaler-shared" {
		t.Errorf("mount source = %q, want %q", sharedMount.Source, "runscaler-shared")
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
		if m.Source != "runscaler-shared" {
			t.Errorf("runner %d: mount source = %q, want %q", i, m.Source, "runscaler-shared")
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

func TestDockerBackend_Shutdown_RemovesSharedVolume(t *testing.T) {
	b, md := newTestDockerBackend("/shared", true)
	ctx := context.Background()

	b.Shutdown(ctx)

	if len(md.volumesRemoved) != 1 {
		t.Fatalf("expected 1 volume removed, got %d", len(md.volumesRemoved))
	}
	if md.volumesRemoved[0] != "runscaler-shared" {
		t.Errorf("volume removed = %q, want %q", md.volumesRemoved[0], "runscaler-shared")
	}
}

func TestDockerBackend_Shutdown_SkipsVolumeWhenNotConfigured(t *testing.T) {
	b, md := newTestDockerBackend("", true)
	ctx := context.Background()

	b.Shutdown(ctx)

	if len(md.volumesRemoved) != 0 {
		t.Errorf("should not remove volumes when not configured, removed %d", len(md.volumesRemoved))
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
