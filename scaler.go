package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"syscall"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/google/uuid"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// dockerAPI abstracts the Docker client methods used by Scaler,
// enabling dependency injection and testing.
type dockerAPI interface {
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ImagesPrune(ctx context.Context, pruneFilters filters.Args) (image.PruneReport, error)
	BuildCachePrune(ctx context.Context, opts build.CachePruneOptions) (*build.CachePruneReport, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
}

// scalesetAPI abstracts the scaleset client methods used by Scaler.
type scalesetAPI interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

// Scaler implements listener.Scaler to handle scaling decisions
// and manage Docker container lifecycle for GitHub runners.
type Scaler struct {
	runners        runnerState
	runnerImage    string
	scaleSetID     int
	dockerClient   dockerAPI
	scalesetClient scalesetAPI
	minRunners     int
	maxRunners     int
	dockerSocket   string
	dind           bool
	sharedVolume   string
	workDirBase    string
	logger         *slog.Logger
}

// Compile-time check that Scaler implements listener.Scaler.
var _ listener.Scaler = (*Scaler)(nil)

// HandleDesiredRunnerCount scales runners up to match demand.
// Scale down is handled naturally via HandleJobCompleted.
func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	currentCount := s.runners.count()
	targetRunnerCount := min(s.maxRunners, s.minRunners+count)

	switch {
	case targetRunnerCount == currentCount:
		return currentCount, nil
	case targetRunnerCount > currentCount:
		scaleUp := targetRunnerCount - currentCount
		s.logger.Info(
			"Scaling up runners",
			slog.Int("currentCount", currentCount),
			slog.Int("desiredCount", targetRunnerCount),
			slog.Int("scaleUp", scaleUp),
		)
		for range scaleUp {
			if _, err := s.startRunner(ctx); err != nil {
				return 0, fmt.Errorf("failed to start runner: %w", err)
			}
		}
		return s.runners.count(), nil
	default:
		// Scale down is handled by HandleJobCompleted removing containers.
		return currentCount, nil
	}
}

// HandleJobStarted marks a runner as busy when a job is assigned.
func (s *Scaler) HandleJobStarted(ctx context.Context, jobInfo *scaleset.JobStarted) error {
	s.logger.Info(
		"Job started",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
		slog.String("runnerName", jobInfo.RunnerName),
	)
	s.runners.markBusy(jobInfo.RunnerName)
	return nil
}

// HandleJobCompleted removes the runner container after job finishes.
func (s *Scaler) HandleJobCompleted(ctx context.Context, jobInfo *scaleset.JobCompleted) error {
	s.logger.Info(
		"Job completed",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
		slog.String("runnerName", jobInfo.RunnerName),
	)

	containerID := s.runners.markDone(jobInfo.RunnerName)
	if err := s.dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("failed to remove runner container: %w", err)
	}

	s.cleanupWorkDir(jobInfo.RunnerName)
	return nil
}

// startRunner creates and starts a new ephemeral runner container.
func (s *Scaler) startRunner(ctx context.Context) (string, error) {
	name := fmt.Sprintf("runner-%s", uuid.NewString()[:8])

	jit, err := s.scalesetClient.GenerateJitRunnerConfig(
		ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{
			Name: name,
		},
		s.scaleSetID,
	)
	if err != nil {
		return "", fmt.Errorf("failed to generate JIT config: %w", err)
	}

	// Build volume binds and group membership
	var binds []string
	var groupAdd []string
	if s.dind {
		binds = append(binds, fmt.Sprintf("%s:/var/run/docker.sock", s.dockerSocket))
		// Add the docker.sock owning group so the runner user can access it
		if gid, err := socketGroupID(s.dockerSocket); err == nil {
			groupAdd = append(groupAdd, strconv.Itoa(gid))
		}
	}
	var mounts []mount.Mount
	if s.sharedVolume != "" {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: "runscaler-shared",
			Target: s.sharedVolume,
		})
	}
	if s.workDirBase != "" {
		workDir := fmt.Sprintf("%s/%s", s.workDirBase, name)
		binds = append(binds, fmt.Sprintf("%s:%s", workDir, workDir))
	}

	c, err := s.dockerClient.ContainerCreate(
		ctx,
		&container.Config{
			Image: s.runnerImage,
			User:  "runner",
			Cmd:   []string{"/home/runner/run.sh"},
			Env: []string{
				fmt.Sprintf("ACTIONS_RUNNER_INPUT_JITCONFIG=%s", jit.EncodedJITConfig),
			},
		},
		&container.HostConfig{
			Binds:       binds,
			Mounts:      mounts,
			GroupAdd:    groupAdd,
			SecurityOpt: []string{"label:disable"},
		},
		nil, nil,
		name,
	)
	if err != nil {
		return "", fmt.Errorf("failed to create runner container: %w", err)
	}

	if err := s.dockerClient.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
		// Clean up the created-but-not-started container
		_ = s.dockerClient.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("failed to start runner container: %w", err)
	}

	s.logger.Info("Runner started", slog.String("name", name), slog.String("containerID", c.ID))
	s.runners.addIdle(name, c.ID)
	return name, nil
}

// shutdown force-removes all managed runner containers.
func (s *Scaler) shutdown(ctx context.Context) {
	s.logger.Info("Shutting down runners")
	s.runners.mu.Lock()
	defer s.runners.mu.Unlock()

	for name, containerID := range s.runners.idle {
		s.logger.Info("Removing idle runner", slog.String("name", name))
		if err := s.dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
			s.logger.Error("Failed to remove idle runner", slog.String("name", name), slog.String("error", err.Error()))
		}
		s.cleanupWorkDir(name)
	}
	clear(s.runners.idle)

	for name, containerID := range s.runners.busy {
		s.logger.Info("Removing busy runner", slog.String("name", name))
		if err := s.dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
			s.logger.Error("Failed to remove busy runner", slog.String("name", name), slog.String("error", err.Error()))
		}
		s.cleanupWorkDir(name)
	}
	clear(s.runners.busy)

	// Remove shared Docker volume
	if s.sharedVolume != "" {
		s.logger.Info("Removing shared volume", slog.String("volume", "runscaler-shared"))
		if err := s.dockerClient.VolumeRemove(ctx, "runscaler-shared", true); err != nil {
			s.logger.Error("Failed to remove shared volume", slog.String("error", err.Error()))
		}
	}

	s.pruneDocker(ctx)
}

// cleanupWorkDir removes the host-side work directory for a runner.
func (s *Scaler) cleanupWorkDir(runnerName string) {
	if s.workDirBase == "" {
		return
	}
	workDir := fmt.Sprintf("%s/%s", s.workDirBase, runnerName)
	if err := os.RemoveAll(workDir); err != nil {
		s.logger.Error("Failed to clean work directory", slog.String("dir", workDir), slog.String("error", err.Error()))
	}
}

// pruneDocker removes dangling images, stopped containers, and build cache
// left behind by workflow jobs that ran docker build inside runners.
func (s *Scaler) pruneDocker(ctx context.Context) {
	s.logger.Info("Pruning Docker resources")

	// Remove dangling images (untagged intermediate layers)
	pruneFilters := filters.NewArgs(filters.Arg("dangling", "true"))
	imagesReport, err := s.dockerClient.ImagesPrune(ctx, pruneFilters)
	if err != nil {
		s.logger.Error("Failed to prune images", slog.String("error", err.Error()))
	} else if imagesReport.SpaceReclaimed > 0 {
		s.logger.Info("Pruned dangling images",
			slog.Int("count", len(imagesReport.ImagesDeleted)),
			slog.String("reclaimed", formatBytes(imagesReport.SpaceReclaimed)),
		)
	}

	// Remove build cache
	buildReport, err := s.dockerClient.BuildCachePrune(ctx, build.CachePruneOptions{All: true})
	if err != nil {
		s.logger.Error("Failed to prune build cache", slog.String("error", err.Error()))
	} else if buildReport.SpaceReclaimed > 0 {
		s.logger.Info("Pruned build cache",
			slog.String("reclaimed", formatBytes(buildReport.SpaceReclaimed)),
		)
	}
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// socketGroupID returns the owning group ID of a Unix socket file.
func socketGroupID(path string) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unsupported platform")
	}
	return int(stat.Gid), nil
}

// --- Runner State ---

// runnerState tracks active runner containers with thread-safe access.
type runnerState struct {
	mu   sync.Mutex
	idle map[string]string // name -> containerID
	busy map[string]string // name -> containerID
}

func (r *runnerState) count() int {
	r.mu.Lock()
	count := len(r.idle) + len(r.busy)
	r.mu.Unlock()
	return count
}

func (r *runnerState) addIdle(name, containerID string) {
	r.mu.Lock()
	r.idle[name] = containerID
	r.mu.Unlock()
}

func (r *runnerState) markBusy(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	containerID, ok := r.idle[name]
	if !ok {
		panic("marking non-existent runner busy")
	}
	delete(r.idle, name)
	r.busy[name] = containerID
}

func (r *runnerState) markDone(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if containerID, ok := r.busy[name]; ok {
		delete(r.busy, name)
		return containerID
	}
	if containerID, ok := r.idle[name]; ok {
		delete(r.idle, name)
		return containerID
	}
	panic("marking non-existent runner done")
}
