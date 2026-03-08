package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/actions/scaleset"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// --- Mocks ---

type createCall struct {
	name       string
	config     *container.Config
	hostConfig *container.HostConfig
}

type mockDocker struct {
	created    []string     // container names
	createCalls []createCall // full create args
	started    []string     // container IDs
	removed    []string     // container IDs
	volumesRemoved []string // volume names
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

type mockScaleset struct {
	generated int
}

func (m *mockScaleset) GenerateJitRunnerConfig(_ context.Context, _ *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	m.generated++
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		EncodedJITConfig: "mock-jit-config",
	}, nil
}

func newTestScaler(minRunners, maxRunners int) (*Scaler, *mockDocker, *mockScaleset) {
	md := &mockDocker{}
	ms := &mockScaleset{}
	s := &Scaler{
		dockerClient:   md,
		scalesetClient: ms,
		runnerImage:    "test-image:latest",
		scaleSetID:     1,
		minRunners:     minRunners,
		maxRunners:     maxRunners,
		dockerSocket:   "/var/run/docker.sock",
		dind:           true,
		logger:         slog.New(slog.DiscardHandler),
		runners: runnerState{
			idle: make(map[string]string),
			busy: make(map[string]string),
		},
	}
	return s, md, ms
}

// --- runnerState tests ---

func TestRunnerStateLifecycle(t *testing.T) {
	rs := runnerState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	if rs.count() != 0 {
		t.Fatalf("initial count = %d, want 0", rs.count())
	}

	rs.addIdle("runner-1", "container-1")
	rs.addIdle("runner-2", "container-2")
	if rs.count() != 2 {
		t.Fatalf("count after addIdle = %d, want 2", rs.count())
	}

	rs.markBusy("runner-1")
	if rs.count() != 2 {
		t.Fatalf("count after markBusy = %d, want 2", rs.count())
	}
	if _, ok := rs.idle["runner-1"]; ok {
		t.Error("runner-1 should not be in idle after markBusy")
	}
	if _, ok := rs.busy["runner-1"]; !ok {
		t.Error("runner-1 should be in busy after markBusy")
	}

	containerID := rs.markDone("runner-1")
	if containerID != "container-1" {
		t.Errorf("markDone returned %q, want %q", containerID, "container-1")
	}
	if rs.count() != 1 {
		t.Fatalf("count after markDone = %d, want 1", rs.count())
	}

	// markDone on idle runner (no job started)
	containerID = rs.markDone("runner-2")
	if containerID != "container-2" {
		t.Errorf("markDone(idle) returned %q, want %q", containerID, "container-2")
	}
	if rs.count() != 0 {
		t.Fatalf("count after all done = %d, want 0", rs.count())
	}
}

func TestRunnerStateMarkBusyPanics(t *testing.T) {
	rs := runnerState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("markBusy on non-existent runner should panic")
		}
	}()
	rs.markBusy("nonexistent")
}

func TestRunnerStateMarkDonePanics(t *testing.T) {
	rs := runnerState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("markDone on non-existent runner should panic")
		}
	}()
	rs.markDone("nonexistent")
}

func TestRunnerStateConcurrency(t *testing.T) {
	rs := runnerState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("runner-%d", i)
			rs.addIdle(name, fmt.Sprintf("container-%d", i))
		}(i)
	}
	wg.Wait()

	if rs.count() != 100 {
		t.Errorf("concurrent addIdle count = %d, want 100", rs.count())
	}
}

// --- Scaler tests ---

func TestHandleDesiredRunnerCount_ScaleUp(t *testing.T) {
	s, md, ms := newTestScaler(0, 10)
	ctx := context.Background()

	got, err := s.HandleDesiredRunnerCount(ctx, 3)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}
	if got != 3 {
		t.Errorf("returned count = %d, want 3", got)
	}
	if len(md.created) != 3 {
		t.Errorf("containers created = %d, want 3", len(md.created))
	}
	if len(md.started) != 3 {
		t.Errorf("containers started = %d, want 3", len(md.started))
	}
	if ms.generated != 3 {
		t.Errorf("JIT configs generated = %d, want 3", ms.generated)
	}
}

func TestHandleDesiredRunnerCount_RespectsMax(t *testing.T) {
	s, md, _ := newTestScaler(0, 5)
	ctx := context.Background()

	got, err := s.HandleDesiredRunnerCount(ctx, 100)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}
	if got != 5 {
		t.Errorf("returned count = %d, want 5 (maxRunners)", got)
	}
	if len(md.created) != 5 {
		t.Errorf("containers created = %d, want 5", len(md.created))
	}
}

func TestHandleDesiredRunnerCount_WithMinRunners(t *testing.T) {
	s, md, _ := newTestScaler(2, 10)
	ctx := context.Background()

	// With 0 assigned jobs, target = min(10, 2+0) = 2
	got, err := s.HandleDesiredRunnerCount(ctx, 0)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount(0) error: %v", err)
	}
	if got != 2 {
		t.Errorf("returned count = %d, want 2 (minRunners)", got)
	}
	if len(md.created) != 2 {
		t.Errorf("containers created = %d, want 2", len(md.created))
	}
}

func TestHandleDesiredRunnerCount_NoScaleWhenEqual(t *testing.T) {
	s, md, _ := newTestScaler(0, 10)
	ctx := context.Background()

	// Pre-populate 3 idle runners
	s.runners.addIdle("runner-1", "c1")
	s.runners.addIdle("runner-2", "c2")
	s.runners.addIdle("runner-3", "c3")

	got, err := s.HandleDesiredRunnerCount(ctx, 3)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}
	if got != 3 {
		t.Errorf("returned count = %d, want 3", got)
	}
	if len(md.created) != 0 {
		t.Errorf("should not create containers when count matches, created = %d", len(md.created))
	}
}

func TestHandleDesiredRunnerCount_NoScaleDown(t *testing.T) {
	s, md, _ := newTestScaler(0, 10)
	ctx := context.Background()

	// Pre-populate 5 runners
	for i := range 5 {
		s.runners.addIdle(fmt.Sprintf("runner-%d", i), fmt.Sprintf("c%d", i))
	}

	// Desired is 2, but we don't scale down (ephemeral runners removed via HandleJobCompleted)
	got, err := s.HandleDesiredRunnerCount(ctx, 2)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}
	if got != 5 {
		t.Errorf("returned count = %d, want 5 (no scale down)", got)
	}
	if len(md.created) != 0 {
		t.Errorf("should not create containers, created = %d", len(md.created))
	}
	if len(md.removed) != 0 {
		t.Errorf("should not remove containers, removed = %d", len(md.removed))
	}
}

func TestHandleJobStarted(t *testing.T) {
	s, _, _ := newTestScaler(0, 10)
	ctx := context.Background()

	s.runners.addIdle("runner-abc", "container-abc")

	err := s.HandleJobStarted(ctx, &scaleset.JobStarted{
		JobMessageBase: scaleset.JobMessageBase{
			RunnerRequestID: 1,
			JobID:           "job-1",
		},
		RunnerName: "runner-abc",
	})
	if err != nil {
		t.Fatalf("HandleJobStarted() error: %v", err)
	}

	if _, ok := s.runners.idle["runner-abc"]; ok {
		t.Error("runner should not be idle after job started")
	}
	if _, ok := s.runners.busy["runner-abc"]; !ok {
		t.Error("runner should be busy after job started")
	}
}

func TestHandleJobCompleted(t *testing.T) {
	s, md, _ := newTestScaler(0, 10)
	ctx := context.Background()

	s.runners.addIdle("runner-abc", "container-abc")
	s.runners.markBusy("runner-abc")

	err := s.HandleJobCompleted(ctx, &scaleset.JobCompleted{
		JobMessageBase: scaleset.JobMessageBase{
			RunnerRequestID: 1,
			JobID:           "job-1",
		},
		RunnerName: "runner-abc",
	})
	if err != nil {
		t.Fatalf("HandleJobCompleted() error: %v", err)
	}

	if s.runners.count() != 0 {
		t.Errorf("runner count = %d, want 0 after job completed", s.runners.count())
	}
	if len(md.removed) != 1 {
		t.Errorf("containers removed = %d, want 1", len(md.removed))
	}
	if md.removed[0] != "container-abc" {
		t.Errorf("removed container = %q, want %q", md.removed[0], "container-abc")
	}
}

func TestShutdown(t *testing.T) {
	s, md, _ := newTestScaler(0, 10)
	ctx := context.Background()

	s.runners.addIdle("idle-1", "c-idle-1")
	s.runners.addIdle("busy-1", "c-busy-1")
	s.runners.markBusy("busy-1")

	s.shutdown(ctx)

	if s.runners.count() != 0 {
		t.Errorf("runner count = %d after shutdown, want 0", s.runners.count())
	}
	if len(md.removed) != 2 {
		t.Errorf("containers removed = %d, want 2", len(md.removed))
	}
}

// --- Shared volume tests ---

// findMountByTarget returns the mount with the given target, or nil if not found.
func findMountByTarget(mounts []mount.Mount, target string) *mount.Mount {
	for i := range mounts {
		if mounts[i].Target == target {
			return &mounts[i]
		}
	}
	return nil
}

func TestStartRunner_WithSharedVolume(t *testing.T) {
	s, md, _ := newTestScaler(0, 10)
	s.sharedVolume = "/shared"
	ctx := context.Background()

	_, err := s.HandleDesiredRunnerCount(ctx, 1)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}

	if len(md.createCalls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(md.createCalls))
	}
	call := md.createCalls[0]

	// Verify docker socket bind mount (dind=true by default)
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

	// Verify SHARED_DIR environment variable is set
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

func TestStartRunner_WithoutSharedVolume(t *testing.T) {
	s, md, _ := newTestScaler(0, 10)
	ctx := context.Background()

	_, err := s.HandleDesiredRunnerCount(ctx, 1)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}

	call := md.createCalls[0]

	// Docker socket mount should still be present (dind=true by default)
	dockerMount := findMountByTarget(call.hostConfig.Mounts, "/var/run/docker.sock")
	if dockerMount == nil {
		t.Fatal("docker socket mount not found")
	}

	// No shared volume mount
	sharedMount := findMountByTarget(call.hostConfig.Mounts, "/shared")
	if sharedMount != nil {
		t.Errorf("shared volume mount should not be present when shared volume is not configured")
	}

	// Direct run.sh command without chown wrapper
	if len(call.config.Cmd) != 1 || call.config.Cmd[0] != "/home/runner/run.sh" {
		t.Errorf("cmd = %v, want [/home/runner/run.sh]", call.config.Cmd)
	}

	// SHARED_DIR should not be set when shared volume is not configured
	for _, env := range call.config.Env {
		if strings.HasPrefix(env, "SHARED_DIR=") {
			t.Errorf("env should not contain SHARED_DIR when shared volume is not configured, got: %v", call.config.Env)
		}
	}
}

func TestStartRunner_MultipleRunnersShareVolume(t *testing.T) {
	s, md, _ := newTestScaler(0, 10)
	s.sharedVolume = "/shared"
	ctx := context.Background()

	// Start 3 runners
	_, err := s.HandleDesiredRunnerCount(ctx, 3)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}

	if len(md.createCalls) != 3 {
		t.Fatalf("expected 3 create calls, got %d", len(md.createCalls))
	}

	// Verify all runners share the same named volume
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

func TestBuildContainerEnv(t *testing.T) {
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
			s, _, _ := newTestScaler(0, 10)
			s.sharedVolume = tt.sharedVolume

			env := s.buildContainerEnv("test-jit-config")

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
				want := fmt.Sprintf("SHARED_DIR=%s", tt.wantPath)
				if foundShared != want {
					t.Errorf("env SHARED_DIR = %q, want %q", foundShared, want)
				}
			} else if foundShared != "" {
				t.Errorf("env should not contain SHARED_DIR, got: %v", env)
			}
		})
	}
}

func TestShutdown_RemovesSharedVolume(t *testing.T) {
	s, md, _ := newTestScaler(0, 10)
	s.sharedVolume = "/shared"
	ctx := context.Background()

	s.shutdown(ctx)

	if len(md.volumesRemoved) != 1 {
		t.Fatalf("expected 1 volume removed, got %d", len(md.volumesRemoved))
	}
	if md.volumesRemoved[0] != "runscaler-shared" {
		t.Errorf("volume removed = %q, want %q", md.volumesRemoved[0], "runscaler-shared")
	}
}

func TestShutdown_SkipsVolumeRemoveWhenNotConfigured(t *testing.T) {
	s, md, _ := newTestScaler(0, 10)
	ctx := context.Background()

	s.shutdown(ctx)

	if len(md.volumesRemoved) != 0 {
		t.Errorf("should not remove volumes when shared-volume not configured, removed %d", len(md.volumesRemoved))
	}
}
