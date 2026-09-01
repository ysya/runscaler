package provider

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ysya/runscaler/internal/config"
)

// DockerAPI abstracts the Docker client methods used by DockerProvider,
// enabling dependency injection and testing.
type DockerAPI interface {
	ContainerCreate(ctx context.Context, options dockerclient.ContainerCreateOptions) (dockerclient.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options dockerclient.ContainerStartOptions) (dockerclient.ContainerStartResult, error)
	ContainerRemove(ctx context.Context, containerID string, options dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error)
	ContainerWait(ctx context.Context, containerID string, options dockerclient.ContainerWaitOptions) dockerclient.ContainerWaitResult
	ContainerPrune(ctx context.Context, options dockerclient.ContainerPruneOptions) (dockerclient.ContainerPruneResult, error)
	ImagePrune(ctx context.Context, options dockerclient.ImagePruneOptions) (dockerclient.ImagePruneResult, error)
	BuildCachePrune(ctx context.Context, options dockerclient.BuildCachePruneOptions) (dockerclient.BuildCachePruneResult, error)
	VolumeRemove(ctx context.Context, volumeID string, options dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error)
	ContainerList(ctx context.Context, options dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error)
	VolumeList(ctx context.Context, options dockerclient.VolumeListOptions) (dockerclient.VolumeListResult, error)
	// ContainerLogs backs internal/cachestore's volume-backed stores: it
	// reads a finished helper container's stdout so Measure can parse
	// `du -sb` output and Reclaim can report bytes freed.
	ContainerLogs(ctx context.Context, containerID string, options dockerclient.ContainerLogsOptions) (dockerclient.ContainerLogsResult, error)
	// Info and DiskUsage back internal/cachestore's Docker daemon store:
	// Info resolves the daemon's data-root directory (for Path()), and
	// DiskUsage reports image and build-cache size totals (for Measure()).
	Info(ctx context.Context, options dockerclient.InfoOptions) (dockerclient.SystemInfoResult, error)
	DiskUsage(ctx context.Context, options dockerclient.DiskUsageOptions) (dockerclient.DiskUsageResult, error)
}

// DockerProvider runs GitHub Actions runners as Docker containers.
type DockerProvider struct {
	dockerClient     DockerAPI
	runnerImage      string
	dockerSocket     string
	dind             bool
	sharedVolume     string                    // container path of the shared volume ("" = disabled)
	sharedVolumeName string                    // named volume backing the shared-volume mount
	cacheVolumes     []config.CacheVolumeMount // persistent named cache volumes (never cleaned up)
	network          string                    // pre-existing network to attach containers to ("" = default bridge)
	memoryBytes      int64                     // container memory limit in bytes (0 = unlimited)
	nanoCPUs         int64                     // container CPU limit in nanoseconds (0 = unlimited)
	pidsLimit        int64                     // container pids limit (0 = unlimited)
	platform         *ocispec.Platform         // nil = use host default
	logger           *slog.Logger
}

// NewDockerProvider creates a DockerProvider from scale set config.
func NewDockerProvider(ss config.ScaleSetConfig, client DockerAPI, logger *slog.Logger) *DockerProvider {
	p := &DockerProvider{
		dockerClient:     client,
		runnerImage:      ss.RunnerImage,
		dockerSocket:     ss.Docker.Socket,
		dind:             ss.IsDinD(),
		sharedVolume:     ss.Docker.SharedVolume,
		sharedVolumeName: ss.SharedVolumeName(),
		network:          ss.Docker.Network,
		memoryBytes:      int64(ss.Docker.Memory) * 1024 * 1024, // MB → bytes
		nanoCPUs:         int64(ss.Docker.CPU) * 1_000_000_000,  // cores → nanoseconds
		pidsLimit:        ss.Docker.PidsLimit,
		logger:           logger,
	}
	// Validate() already surfaced parse errors before startup; on error no
	// cache volumes are mounted rather than a partial set.
	p.cacheVolumes, _ = ss.Docker.ParseCacheVolumes()
	if ss.Docker.Platform != "" {
		p.platform = parsePlatform(ss.Docker.Platform)
	}
	return p
}

// parsePlatform parses a platform string like "linux/amd64" into an OCI platform spec.
func parsePlatform(s string) *ocispec.Platform {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 2 {
		return nil
	}
	p := &ocispec.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	return p
}

// StartInstance creates and starts a new ephemeral Docker container runner.
func (p *DockerProvider) StartInstance(ctx context.Context, name string, jitConfig string) (string, error) {
	// Build mounts and group membership.
	var mounts []mount.Mount
	var groupAdd []string
	if p.dind {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   p.dockerSocket,
			Target:   "/var/run/docker.sock",
			ReadOnly: false,
		})
		// Add socket's owning group (works on native Linux where socket is root:docker).
		// Also add GID 0 for macOS/OrbStack where virtiofs maps the socket to root:root.
		if gid, err := socketGroupID(p.dockerSocket); err == nil && gid != 0 {
			groupAdd = append(groupAdd, strconv.Itoa(gid))
		}
		groupAdd = append(groupAdd, "0")
	}
	// Named volume mounts (shared + cache); their targets need ownership
	// fixed before the runner starts.
	var volumeTargets []string
	if p.sharedVolume != "" {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: p.sharedVolumeName,
			Target: p.sharedVolume,
		})
		volumeTargets = append(volumeTargets, p.sharedVolume)
	}
	for _, cv := range p.cacheVolumes {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: cv.Volume,
			Target: cv.Path,
		})
		volumeTargets = append(volumeTargets, cv.Path)
	}

	hostConfig := &container.HostConfig{
		Mounts:      mounts,
		GroupAdd:    groupAdd,
		SecurityOpt: []string{"label:disable"},
		Resources:   p.containerResources(),
	}
	if p.network != "" {
		hostConfig.NetworkMode = container.NetworkMode(p.network)
	}

	c, err := p.dockerClient.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Config: &container.Config{
			Image:  p.runnerImage,
			User:   "runner",
			Cmd:    runnerCmd(volumeTargets),
			Env:    p.buildContainerEnv(jitConfig),
			Labels: map[string]string{"managed-by": "runner"},
		},
		HostConfig: hostConfig,
		Platform:   p.platform,
		Name:       name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create runner container: %w", err)
	}

	if _, err := p.dockerClient.ContainerStart(ctx, c.ID, dockerclient.ContainerStartOptions{}); err != nil {
		_, _ = p.dockerClient.ContainerRemove(ctx, c.ID, dockerclient.ContainerRemoveOptions{Force: true})
		return "", fmt.Errorf("failed to start runner container: %w", err)
	}

	p.logger.Debug("Runner started",
		slog.String("name", name),
		slog.String("containerID", c.ID),
		slog.Int("mounts", len(mounts)),
	)
	return c.ID, nil
}

// RemoveInstance force-removes a Docker container by ID.
func (p *DockerProvider) RemoveInstance(ctx context.Context, instanceID string) error {
	if _, err := p.dockerClient.ContainerRemove(ctx, instanceID, dockerclient.ContainerRemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("failed to remove runner container: %w", err)
	}
	return nil
}

// WaitInstance returns when a runner container is no longer running. The controller uses
// this to evict containers that crash before GitHub can send JobCompleted.
func (p *DockerProvider) WaitInstance(ctx context.Context, instanceID string) error {
	wait := p.dockerClient.ContainerWait(ctx, instanceID, dockerclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-wait.Error:
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				return nil // externally removed is equivalent to stopped
			}
			return fmt.Errorf("wait for runner container: %w", err)
		}
		return nil
	case status := <-wait.Result:
		if status.Error != nil {
			return fmt.Errorf("wait for runner container: %s", status.Error.Message)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown is a no-op for DockerProvider. Nothing shared is reclaimed at
// exit: the shared volume carries handoff data for runs still in flight
// across a restart, and images and build cache belong to the daemon, not to
// this process. Every one of them is reclaimed on its own schedule instead,
// through internal/cachestore — the periodic sweepers in cmd/runner and the
// disk guard. A per-provider Shutdown that touched any of it would also race
// the other scale sets sharing this Docker client.
func (p *DockerProvider) Shutdown(_ context.Context) {}

// runnerCmd returns the container command: plain run.sh when no named
// volumes are mounted, otherwise a shell prelude that fixes the ownership of
// each volume mount point first. Freshly created named volumes are root-owned
// at the top level, so the chown is conditional on the mount point's owner:
// the first job on a fresh volume pays the recursive chown once and later
// jobs skip the IO storm entirely. (stat -c is GNU coreutils — the runner
// image is Ubuntu.)
func runnerCmd(volumeTargets []string) []string {
	if len(volumeTargets) == 0 {
		return []string{"/home/runner/run.sh"}
	}
	var sb strings.Builder
	sb.WriteString(`fix_own() { [ "$(stat -c %u "$1")" = "1001" ] || sudo chown -R 1001:123 "$1"; };`)
	for _, target := range volumeTargets {
		fmt.Fprintf(&sb, " fix_own %s;", shellQuote(target))
	}
	sb.WriteString(" exec /home/runner/run.sh")
	return []string{"sh", "-c", sb.String()}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// buildContainerEnv returns the environment variables for a runner container.
func (p *DockerProvider) buildContainerEnv(jitConfig string) []string {
	env := []string{
		fmt.Sprintf("ACTIONS_RUNNER_INPUT_JITCONFIG=%s", jitConfig),
	}
	if p.sharedVolume != "" {
		env = append(env, fmt.Sprintf("SHARED_DIR=%s", p.sharedVolume))
	}
	return env
}

// buildxBuilderPrefix is the name prefix Docker gives to BuildKit builder
// containers and their state volumes (e.g. buildx_buildkit_builder-<uuid>0).
const buildxBuilderPrefix = "buildx_buildkit_"

// buildxContainerName returns the unprefixed name of a buildx BuildKit builder
// container, or "" if the container is not one.
func buildxContainerName(c container.Summary) string {
	for _, n := range c.Names {
		n = strings.TrimPrefix(n, "/")
		if strings.HasPrefix(n, buildxBuilderPrefix) {
			return n
		}
	}
	return ""
}

// CleanupOrphanedBuildxBuilders removes buildx BuildKit builder containers
// (named buildx_buildkit_*) older than maxAge, along with their named `_state`
// volumes. Such builders are created by `docker buildx create` (e.g. via
// docker/setup-buildx-action); on a persistent host sharing one Docker daemon
// they accumulate when per-job cleanup never runs, each leaving behind a
// multi-GB state volume. maxAge is kept well above any realistic build so a
// sweep never disrupts an in-progress build. A no-op when maxAge <= 0.
func CleanupOrphanedBuildxBuilders(ctx context.Context, client DockerAPI, maxAge time.Duration, logger *slog.Logger) error {
	if maxAge <= 0 {
		return nil
	}

	containerResult, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}

	cutoff := time.Now().Add(-maxAge)
	var removedContainers, removedVolumes int

	for _, c := range containerResult.Items {
		name := buildxContainerName(c)
		if name == "" {
			continue
		}
		// Created is unix seconds; skip young builders that may back an
		// in-progress build.
		if time.Unix(c.Created, 0).After(cutoff) {
			continue
		}

		if _, err := client.ContainerRemove(ctx, c.ID, dockerclient.ContainerRemoveOptions{Force: true}); err != nil {
			logger.Warn("Failed to remove orphaned buildx builder",
				slog.String("container", name), slog.Any("error", err))
			continue
		}
		removedContainers++

		// `docker rm` leaves named volumes intact; the builder's state lives in
		// "<container-name>_state", so remove it explicitly.
		stateVol := name + "_state"
		if _, err := client.VolumeRemove(ctx, stateVol, dockerclient.VolumeRemoveOptions{Force: true}); err != nil {
			logger.Debug("Failed to remove buildx state volume",
				slog.String("volume", stateVol), slog.Any("error", err))
		} else {
			removedVolumes++
		}
	}

	// Also reap dangling buildx state volumes whose containers were already
	// gone (e.g. from a partial manual cleanup). Dangling means unreferenced,
	// so removal is safe regardless of age.
	danglingFilter := make(dockerclient.Filters).Add("dangling", "true")
	if volList, err := client.VolumeList(ctx, dockerclient.VolumeListOptions{Filters: danglingFilter}); err != nil {
		logger.Debug("Failed to list volumes for buildx cleanup", slog.Any("error", err))
	} else {
		for _, v := range volList.Items {
			if !strings.HasPrefix(v.Name, buildxBuilderPrefix) {
				continue
			}
			if _, err := client.VolumeRemove(ctx, v.Name, dockerclient.VolumeRemoveOptions{Force: true}); err != nil {
				logger.Debug("Failed to remove dangling buildx volume",
					slog.String("volume", v.Name), slog.Any("error", err))
			} else {
				removedVolumes++
			}
		}
	}

	if removedContainers > 0 || removedVolumes > 0 {
		logger.Info("Removed orphaned buildx builders",
			slog.Int("containers", removedContainers),
			slog.Int("volumes", removedVolumes),
			slog.Duration("max_age", maxAge),
		)
	}
	return nil
}

// containerResources builds the resource constraints for a runner container.
func (p *DockerProvider) containerResources() container.Resources {
	var r container.Resources
	if p.memoryBytes > 0 {
		r.Memory = p.memoryBytes
	}
	if p.nanoCPUs > 0 {
		r.NanoCPUs = p.nanoCPUs
	}
	if p.pidsLimit > 0 {
		// Pids cgroup limit counts threads too — protects the host from
		// fork bombs inside a job.
		limit := p.pidsLimit
		r.PidsLimit = &limit
	}
	return r
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
