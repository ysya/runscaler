package backend

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"syscall"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ysya/runscaler/internal/config"
)

// DockerAPI abstracts the Docker client methods used by DockerBackend,
// enabling dependency injection and testing.
type DockerAPI interface {
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ImagesPrune(ctx context.Context, pruneFilters filters.Args) (image.PruneReport, error)
	BuildCachePrune(ctx context.Context, opts build.CachePruneOptions) (*build.CachePruneReport, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
}

// DockerBackend runs GitHub Actions runners as Docker containers.
type DockerBackend struct {
	dockerClient DockerAPI
	runnerImage  string
	dockerSocket string
	dind         bool
	sharedVolume string
	logger       *slog.Logger
}

// NewDockerBackend creates a DockerBackend from scale set config.
func NewDockerBackend(ss config.ScaleSetConfig, client DockerAPI, logger *slog.Logger) *DockerBackend {
	return &DockerBackend{
		dockerClient: client,
		runnerImage:  ss.RunnerImage,
		dockerSocket: ss.Docker.Socket,
		dind:         ss.IsDinD(),
		sharedVolume: ss.Docker.SharedVolume,
		logger:       logger,
	}
}

// StartRunner creates and starts a new ephemeral Docker container runner.
func (b *DockerBackend) StartRunner(ctx context.Context, name string, jitConfig string) (string, error) {
	// Build mounts and group membership.
	var mounts []mount.Mount
	var groupAdd []string
	if b.dind {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   b.dockerSocket,
			Target:   "/var/run/docker.sock",
			ReadOnly: false,
		})
		if gid, err := socketGroupID(b.dockerSocket); err == nil {
			groupAdd = append(groupAdd, strconv.Itoa(gid))
		}
	}
	if b.sharedVolume != "" {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: "runscaler-shared",
			Target: b.sharedVolume,
		})
	}

	// Build command — fix shared volume ownership before starting runner.
	var cmd []string
	if b.sharedVolume != "" {
		cmd = []string{"sh", "-c",
			fmt.Sprintf("sudo chown -R 1001:123 %s && /home/runner/run.sh", b.sharedVolume),
		}
	} else {
		cmd = []string{"/home/runner/run.sh"}
	}

	c, err := b.dockerClient.ContainerCreate(
		ctx,
		&container.Config{
			Image: b.runnerImage,
			User:  "runner",
			Cmd:   cmd,
			Env:   b.buildContainerEnv(jitConfig),
		},
		&container.HostConfig{
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

	if err := b.dockerClient.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
		_ = b.dockerClient.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("failed to start runner container: %w", err)
	}

	b.logger.Info("Runner started",
		slog.String("name", name),
		slog.String("containerID", c.ID),
		slog.Int("mounts", len(mounts)),
		slog.String("sharedVolume", b.sharedVolume),
	)
	return c.ID, nil
}

// RemoveRunner force-removes a Docker container by ID.
func (b *DockerBackend) RemoveRunner(ctx context.Context, resourceID string) error {
	if err := b.dockerClient.ContainerRemove(ctx, resourceID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("failed to remove runner container: %w", err)
	}
	return nil
}

// Shutdown removes the shared Docker volume and prunes dangling resources.
func (b *DockerBackend) Shutdown(ctx context.Context) {
	if b.sharedVolume != "" {
		b.logger.Info("Removing shared volume", slog.String("volume", "runscaler-shared"))
		if err := b.dockerClient.VolumeRemove(ctx, "runscaler-shared", true); err != nil {
			b.logger.Error("Failed to remove shared volume", slog.String("error", err.Error()))
		}
	}
	b.pruneDocker(ctx)
}

// buildContainerEnv returns the environment variables for a runner container.
func (b *DockerBackend) buildContainerEnv(jitConfig string) []string {
	env := []string{
		fmt.Sprintf("ACTIONS_RUNNER_INPUT_JITCONFIG=%s", jitConfig),
	}
	if b.sharedVolume != "" {
		env = append(env, fmt.Sprintf("SHARED_DIR=%s", b.sharedVolume))
	}
	return env
}

// pruneDocker removes dangling images and build cache.
func (b *DockerBackend) pruneDocker(ctx context.Context) {
	b.logger.Info("Pruning Docker resources")

	pruneFilters := filters.NewArgs(filters.Arg("dangling", "true"))
	imagesReport, err := b.dockerClient.ImagesPrune(ctx, pruneFilters)
	if err != nil {
		b.logger.Error("Failed to prune images", slog.String("error", err.Error()))
	} else if imagesReport.SpaceReclaimed > 0 {
		b.logger.Info("Pruned dangling images",
			slog.Int("count", len(imagesReport.ImagesDeleted)),
			slog.String("reclaimed", FormatBytes(imagesReport.SpaceReclaimed)),
		)
	}

	buildReport, err := b.dockerClient.BuildCachePrune(ctx, build.CachePruneOptions{All: true})
	if err != nil {
		b.logger.Error("Failed to prune build cache", slog.String("error", err.Error()))
	} else if buildReport.SpaceReclaimed > 0 {
		b.logger.Info("Pruned build cache",
			slog.String("reclaimed", FormatBytes(buildReport.SpaceReclaimed)),
		)
	}
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

// FormatBytes formats a byte count into a human-readable string.
func FormatBytes(b uint64) string {
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
