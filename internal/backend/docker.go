package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ysya/runscaler/internal/config"
)

// DockerAPI abstracts the Docker client methods used by DockerBackend,
// enabling dependency injection and testing.
type DockerAPI interface {
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainersPrune(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error)
	ImagesPrune(ctx context.Context, pruneFilters filters.Args) (image.PruneReport, error)
	BuildCachePrune(ctx context.Context, opts build.CachePruneOptions) (*build.CachePruneReport, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
}

// DockerBackend runs GitHub Actions runners as Docker containers.
type DockerBackend struct {
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

// NewDockerBackend creates a DockerBackend from scale set config.
func NewDockerBackend(ss config.ScaleSetConfig, client DockerAPI, logger *slog.Logger) *DockerBackend {
	b := &DockerBackend{
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
	b.cacheVolumes, _ = ss.Docker.ParseCacheVolumes()
	if ss.Docker.Platform != "" {
		b.platform = parsePlatform(ss.Docker.Platform)
	}
	return b
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
		// Add socket's owning group (works on native Linux where socket is root:docker).
		// Also add GID 0 for macOS/OrbStack where virtiofs maps the socket to root:root.
		if gid, err := socketGroupID(b.dockerSocket); err == nil && gid != 0 {
			groupAdd = append(groupAdd, strconv.Itoa(gid))
		}
		groupAdd = append(groupAdd, "0")
	}
	// Named volume mounts (shared + cache); their targets need ownership
	// fixed before the runner starts.
	var volumeTargets []string
	if b.sharedVolume != "" {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: b.sharedVolumeName,
			Target: b.sharedVolume,
		})
		volumeTargets = append(volumeTargets, b.sharedVolume)
	}
	for _, cv := range b.cacheVolumes {
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
		Resources:   b.containerResources(),
	}
	if b.network != "" {
		hostConfig.NetworkMode = container.NetworkMode(b.network)
	}

	c, err := b.dockerClient.ContainerCreate(
		ctx,
		&container.Config{
			Image:  b.runnerImage,
			User:   "runner",
			Cmd:    runnerCmd(volumeTargets),
			Env:    b.buildContainerEnv(jitConfig),
			Labels: map[string]string{"managed-by": "runner"},
		},
		hostConfig,
		nil, b.platform,
		name,
	)
	if err != nil {
		return "", fmt.Errorf("failed to create runner container: %w", err)
	}

	if err := b.dockerClient.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
		_ = b.dockerClient.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("failed to start runner container: %w", err)
	}

	b.logger.Debug("Runner started",
		slog.String("name", name),
		slog.String("containerID", c.ID),
		slog.Int("mounts", len(mounts)),
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

// Shutdown is a no-op for DockerBackend — shared Docker resources
// (volume, image/build caches) are cleaned up once at process exit via
// CleanupSharedDocker to avoid races when multiple scale sets share the
// same Docker client and volume.
func (b *DockerBackend) Shutdown(_ context.Context) {}

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
		fmt.Fprintf(&sb, " fix_own %s;", target)
	}
	sb.WriteString(" exec /home/runner/run.sh")
	return []string{"sh", "-c", sb.String()}
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

// CleanupSharedDocker removes the shared Docker volumes named in volumeNames
// (empty slice = nothing to remove) and prunes dangling images. Cache volumes
// are deliberately not touched — they are persistent caches. The full
// build-cache wipe only runs when wipeBuildCache is true: when the runtime
// prune sweep is enabled it owns build-cache retention (age/budget via
// PruneDockerRuntime), so exit no longer wipes a cache the next process run
// would reuse — self-update restarts previously destroyed all build cache.
// It is safe to call once after all Docker-backed scale sets have finished
// shutting down; calling it concurrently or per-backend will race with
// container removal and other prune operations.
//
// The whole sweep is bounded by cleanupSharedDockerTimeout so an unresponsive
// daemon cannot hang shutdown.
func CleanupSharedDocker(ctx context.Context, client DockerAPI, volumeNames []string, wipeBuildCache bool, logger *slog.Logger) {
	cleanupSharedDockerWith(ctx, client, volumeNames, wipeBuildCache, cleanupSharedDockerTimeout, logger)
}

// cleanupSharedDockerTimeout bounds the exit-time cleanup. The Docker API
// calls below carry no deadline of their own, so a slow or wedged daemon
// would otherwise block shutdown indefinitely — a large dangling-image
// backlog has taken about a minute in practice, and the process looks hung
// while it works. Kept under systemd's default 90s TimeoutStopSec (minus the
// ~40s the scale set and scaler shutdowns may already have used) so runner
// still exits on its own terms instead of being SIGKILLed.
const cleanupSharedDockerTimeout = 45 * time.Second

// cleanupSharedDockerWith is the testable core: it performs the sweep under
// the supplied timeout. The exported wrapper above supplies the default.
func cleanupSharedDockerWith(ctx context.Context, client DockerAPI, volumeNames []string, wipeBuildCache bool, timeout time.Duration, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Anything the deadline cuts short is reclaimed by the next run's prune
	// sweep, so report it as a warning rather than an error.
	defer func() {
		if ctx.Err() != nil {
			logger.Warn("Exit cleanup timed out — remaining garbage will be reclaimed on the next run",
				slog.Duration("timeout", timeout))
		}
	}()

	for _, name := range volumeNames {
		logger.Debug("Removing shared volume", slog.String("volume", name))
		if err := client.VolumeRemove(ctx, name, true); err != nil {
			logger.Error("Failed to remove shared volume",
				slog.String("volume", name), slog.Any("error", err))
		}
	}

	logger.Debug("Pruning Docker resources")

	pruneFilters := filters.NewArgs(filters.Arg("dangling", "true"))
	imagesReport, err := client.ImagesPrune(ctx, pruneFilters)
	if err != nil {
		logger.Error("Failed to prune images", slog.Any("error", err))
	} else if imagesReport.SpaceReclaimed > 0 {
		logger.Debug("Pruned dangling images",
			slog.Int("count", len(imagesReport.ImagesDeleted)),
			slog.String("reclaimed", FormatBytes(imagesReport.SpaceReclaimed)),
		)
	}

	if wipeBuildCache {
		buildReport, err := client.BuildCachePrune(ctx, build.CachePruneOptions{All: true})
		if err != nil {
			logger.Error("Failed to prune build cache", slog.Any("error", err))
		} else if buildReport.SpaceReclaimed > 0 {
			logger.Debug("Pruned build cache",
				slog.String("reclaimed", FormatBytes(buildReport.SpaceReclaimed)),
			)
		}
	}
}

// PruneDockerRuntime reclaims disk on the shared daemon while runner is up:
// stopped containers and dangling images older than ttl, build cache entries
// not used within cacheMaxAge, and — when cacheBudgetGB > 0 — build cache
// beyond the budget (least-recently-used entries are evicted down to the
// cap). With DooD, job-created garbage lands directly on the host daemon, so
// an exit-time prune alone never reclaims disk on a long-running process.
// Like buildx cleanup, this assumes the daemon is dedicated to runners:
// matching objects are treated as garbage regardless of what created them.
//
// ttl <= 0 skips the container/image prunes and cacheMaxAge <= 0 skips the
// age-based cache prune. A failed prune does not stop the remaining ones;
// the errors are joined and returned.
func PruneDockerRuntime(ctx context.Context, client DockerAPI, ttl, cacheMaxAge time.Duration, cacheBudgetGB int, logger *slog.Logger) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	var (
		errs              []error
		containersRemoved int
		imagesRemoved     int
		reclaimed         uint64
	)

	if ttl > 0 {
		// The daemon parses Go duration strings for `until` and prunes
		// objects created before now-ttl (see getUntilFromPruneFilters
		// in moby's daemon/prune.go).
		untilFilter := filters.NewArgs(filters.Arg("until", ttl.String()))
		if report, err := client.ContainersPrune(timeoutCtx, untilFilter); err != nil {
			errs = append(errs, fmt.Errorf("prune stopped containers: %w", err))
		} else {
			containersRemoved = len(report.ContainersDeleted)
			reclaimed += report.SpaceReclaimed
		}

		imageFilters := filters.NewArgs(
			filters.Arg("dangling", "true"),
			filters.Arg("until", ttl.String()),
		)
		if report, err := client.ImagesPrune(timeoutCtx, imageFilters); err != nil {
			errs = append(errs, fmt.Errorf("prune dangling images: %w", err))
		} else {
			imagesRemoved = len(report.ImagesDeleted)
			reclaimed += report.SpaceReclaimed
		}
	}

	if cacheMaxAge > 0 {
		// BuildKit maps `until` to KeepDuration: cache entries not used
		// within the window are removed (`unused-for` is its deprecated
		// synonym; v28+ daemons validate both, `until` is canonical).
		opts := build.CachePruneOptions{
			All:     true,
			Filters: filters.NewArgs(filters.Arg("until", cacheMaxAge.String())),
		}
		if report, err := client.BuildCachePrune(timeoutCtx, opts); err != nil {
			errs = append(errs, fmt.Errorf("prune build cache by age: %w", err))
		} else {
			reclaimed += report.SpaceReclaimed
		}
	}

	if cacheBudgetGB > 0 {
		// ReservedSpace is BuildKit's retention cap: least-recently-used
		// cache is evicted until the total fits within the budget.
		opts := build.CachePruneOptions{
			All:           true,
			ReservedSpace: int64(cacheBudgetGB) * 1024 * 1024 * 1024,
		}
		if report, err := client.BuildCachePrune(timeoutCtx, opts); err != nil {
			errs = append(errs, fmt.Errorf("prune build cache to budget: %w", err))
		} else {
			reclaimed += report.SpaceReclaimed
		}
	}

	summary := []any{
		slog.Int("containers", containersRemoved),
		slog.Int("images", imagesRemoved),
		slog.String("reclaimed", FormatBytes(reclaimed)),
	}
	if containersRemoved > 0 || imagesRemoved > 0 || reclaimed > 0 {
		logger.Info("Docker runtime prune reclaimed disk", summary...)
	} else {
		logger.Debug("Docker runtime prune found nothing to reclaim", summary...)
	}
	return errors.Join(errs...)
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

	containers, err := client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}

	cutoff := time.Now().Add(-maxAge)
	var removedContainers, removedVolumes int

	for _, c := range containers {
		name := buildxContainerName(c)
		if name == "" {
			continue
		}
		// Created is unix seconds; skip young builders that may back an
		// in-progress build.
		if time.Unix(c.Created, 0).After(cutoff) {
			continue
		}

		if err := client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			logger.Warn("Failed to remove orphaned buildx builder",
				slog.String("container", name), slog.Any("error", err))
			continue
		}
		removedContainers++

		// `docker rm` leaves named volumes intact; the builder's state lives in
		// "<container-name>_state", so remove it explicitly.
		stateVol := name + "_state"
		if err := client.VolumeRemove(ctx, stateVol, true); err != nil {
			logger.Debug("Failed to remove buildx state volume",
				slog.String("volume", stateVol), slog.Any("error", err))
		} else {
			removedVolumes++
		}
	}

	// Also reap dangling buildx state volumes whose containers were already
	// gone (e.g. from a partial manual cleanup). Dangling means unreferenced,
	// so removal is safe regardless of age.
	danglingFilter := filters.NewArgs(filters.Arg("dangling", "true"))
	if volList, err := client.VolumeList(ctx, volume.ListOptions{Filters: danglingFilter}); err != nil {
		logger.Debug("Failed to list volumes for buildx cleanup", slog.Any("error", err))
	} else {
		for _, v := range volList.Volumes {
			if v == nil || !strings.HasPrefix(v.Name, buildxBuilderPrefix) {
				continue
			}
			if err := client.VolumeRemove(ctx, v.Name, true); err != nil {
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
func (b *DockerBackend) containerResources() container.Resources {
	var r container.Resources
	if b.memoryBytes > 0 {
		r.Memory = b.memoryBytes
	}
	if b.nanoCPUs > 0 {
		r.NanoCPUs = b.nanoCPUs
	}
	if b.pidsLimit > 0 {
		// Pids cgroup limit counts threads too — protects the host from
		// fork bombs inside a job.
		limit := b.pidsLimit
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

// CleanupSharedVolumeStale runs an ephemeral helper container that mounts the
// named shared volume at mountPath and deletes files whose mtime is older
// than ttl. Empty directories left behind are also pruned. The helper image
// must already be available locally; the runner image is reused so no
// additional pull is required. A no-op when ttl <= 0.
func CleanupSharedVolumeStale(ctx context.Context, client DockerAPI, helperImage, volumeName, mountPath string, ttl time.Duration, logger *slog.Logger) error {
	if ttl <= 0 {
		return nil
	}
	if helperImage == "" {
		return fmt.Errorf("helper image is required for shared-volume cleanup")
	}
	if volumeName == "" {
		return fmt.Errorf("volume name is required for shared-volume cleanup")
	}
	if mountPath == "" {
		return fmt.Errorf("mount path is required for shared-volume cleanup")
	}

	// `find -mtime` works in 24h units; round up so sub-day TTLs still sweep.
	days := int(ttl / (24 * time.Hour))
	if days < 1 {
		days = 1
	}

	// Two-phase delete: stale files/symlinks first, then any newly empty dirs.
	// Errors from inside find (e.g. file vanished mid-walk) are swallowed via
	// `|| true` so the container always exits 0.
	script := fmt.Sprintf(
		"set -e; "+
			"find %[1]s -mindepth 1 -mtime +%[2]d \\( -type f -o -type l \\) -print -delete 2>/dev/null | wc -l; "+
			"find %[1]s -mindepth 1 -type d -empty -delete 2>/dev/null || true",
		mountPath, days,
	)

	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	name := fmt.Sprintf("runner-cleanup-%d", time.Now().UnixNano())
	c, err := client.ContainerCreate(
		timeoutCtx,
		&container.Config{
			Image: helperImage,
			User:  "root",
			Cmd:   []string{"sh", "-c", script},
			Labels: map[string]string{
				"managed-by": "runner",
				"purpose":    "shared-volume-cleanup",
			},
		},
		&container.HostConfig{
			Mounts: []mount.Mount{{
				Type:   mount.TypeVolume,
				Source: volumeName,
				Target: mountPath,
			}},
		},
		nil, nil,
		name,
	)
	if err != nil {
		return fmt.Errorf("create cleanup container: %w", err)
	}

	// Always remove the container, even if start/wait fails.
	defer func() {
		_ = client.ContainerRemove(context.WithoutCancel(timeoutCtx), c.ID, container.RemoveOptions{Force: true})
	}()

	if err := client.ContainerStart(timeoutCtx, c.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start cleanup container: %w", err)
	}

	statusCh, errCh := client.ContainerWait(timeoutCtx, c.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("wait cleanup container: %w", err)
		}
	case status := <-statusCh:
		if status.Error != nil {
			return fmt.Errorf("cleanup container error: %s", status.Error.Message)
		}
		if status.StatusCode != 0 {
			return fmt.Errorf("cleanup container exited with status %d", status.StatusCode)
		}
	case <-timeoutCtx.Done():
		return fmt.Errorf("cleanup timed out: %w", timeoutCtx.Err())
	}

	logger.Info("Shared volume cleanup completed",
		slog.Int("ttl_days", days),
		slog.String("volume", volumeName),
		slog.String("path", mountPath),
	)
	return nil
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
