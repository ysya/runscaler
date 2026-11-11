package provider

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/shell"
)

// DockerAPI is the slice of the Docker client DockerProvider itself uses to
// run, watch and diagnose runner containers. internal/cachestore declares
// its own interface for the daemon-maintenance calls it needs.
type DockerAPI interface {
	RunnerCleanupAPI
	ContainerCreate(ctx context.Context, options dockerclient.ContainerCreateOptions) (dockerclient.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options dockerclient.ContainerStartOptions) (dockerclient.ContainerStartResult, error)
	ContainerWait(ctx context.Context, containerID string, options dockerclient.ContainerWaitOptions) dockerclient.ContainerWaitResult
	// ContainerLogs feeds readiness detection and failure diagnostics.
	ContainerLogs(ctx context.Context, containerID string, options dockerclient.ContainerLogsOptions) (dockerclient.ContainerLogsResult, error)
	// ContainerInspect reports whether a runner is still running and how it
	// exited.
	ContainerInspect(ctx context.Context, containerID string, options dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error)
}

// DockerProvider runs GitHub Actions runners as Docker containers.
type DockerProvider struct {
	dockerClient     DockerAPI
	actionsCacheURL  string
	runnerImage      string
	dockerSocket     string
	dind             bool
	workspaceRoot    string
	workspaceMu      sync.Mutex
	workspaces       map[string]*runnerWorkspace
	sharedVolume     string                    // container path of the shared volume ("" = disabled)
	sharedVolumeName string                    // named volume backing the shared-volume mount
	cacheVolumes     []config.CacheVolumeMount // persistent named cache volumes (never cleaned up)
	network          string                    // pre-existing network to attach containers to ("" = default bridge)
	memoryBytes      int64                     // container memory limit in bytes (0 = unlimited)
	nanoCPUs         int64                     // container CPU limit in nanoseconds (0 = unlimited)
	pidsLimit        int64                     // container pids limit (0 = unlimited)
	platform         *ocispec.Platform         // nil = use host default
	logger           *slog.Logger
	readiness        bool
	readyTimeout     time.Duration
	readySettle      time.Duration
	readyPoll        time.Duration
	captureLogs      bool
}

// NewDockerProvider creates a DockerProvider from scale set config.
func NewDockerProvider(ss config.ScaleSetConfig, client DockerAPI, logger *slog.Logger) *DockerProvider {
	p := &DockerProvider{
		dockerClient:     client,
		runnerImage:      ss.RunnerImage,
		actionsCacheURL:  ss.Docker.ActionsCacheURL,
		dockerSocket:     ss.Docker.Socket,
		dind:             ss.IsDinD(),
		workspaceRoot:    ss.Docker.WorkspaceRoot,
		sharedVolume:     ss.Docker.SharedVolume,
		sharedVolumeName: ss.SharedVolumeName(),
		network:          ss.Docker.Network,
		memoryBytes:      int64(ss.Docker.Memory) * 1024 * 1024, // MB → bytes
		nanoCPUs:         int64(ss.Docker.CPU) * 1_000_000_000,  // cores → nanoseconds
		pidsLimit:        ss.Docker.PidsLimit,
		logger:           logger,
		readiness:        ss.IsDockerReadinessEnabled(),
		readyTimeout:     ss.DockerReadyTimeout(),
		readySettle:      defaultReadySettle,
		readyPoll:        500 * time.Millisecond,
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
	workspace, err := p.prepareWorkspace(name)
	if err != nil {
		return "", err
	}
	// Build mounts and group membership.
	var mounts []mount.Mount
	var groupAdd []string
	if p.dind {
		socket := p.dockerSocket
		if workspace != nil {
			socket = workspace.socket
			mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: workspace.work, Target: runnerWorkPath})
		}
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   socket,
			Target:   "/var/run/docker.sock",
			ReadOnly: false,
		})
		if workspace == nil {
			// Add socket's owning group (works on native Linux where socket is root:docker).
			// Also add GID 0 for macOS/OrbStack where virtiofs maps the socket to root:root.
			if gid, err := socketGroupID(p.dockerSocket); err == nil && gid != 0 {
				groupAdd = append(groupAdd, strconv.Itoa(gid))
			}
			groupAdd = append(groupAdd, "0")
		}
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
			Cmd:    runnerCmd(volumeTargets, p.actionsCacheURL != ""),
			Env:    p.buildContainerEnv(jitConfig),
			Labels: map[string]string{ManagedByLabel: ManagedByValue},
		},
		HostConfig: hostConfig,
		Platform:   p.platform,
		Name:       name,
	})
	if err != nil {
		if cleanupErr := p.discardWorkspace(workspace); cleanupErr != nil {
			return "", fmt.Errorf("failed to create runner container: %w; workspace cleanup: %v", err, cleanupErr)
		}
		return "", fmt.Errorf("failed to create runner container: %w", err)
	}
	if workspace != nil {
		p.workspaceMu.Lock()
		if p.workspaces == nil {
			p.workspaces = make(map[string]*runnerWorkspace)
		}
		p.workspaces[c.ID] = workspace
		p.workspaceMu.Unlock()
	}

	if _, err := p.dockerClient.ContainerStart(ctx, c.ID, dockerclient.ContainerStartOptions{}); err != nil {
		if workspace != nil {
			if cleanupErr := p.RemoveInstance(ctx, c.ID); cleanupErr != nil {
				return "", fmt.Errorf("failed to start runner container: %w; cleanup: %v", err, cleanupErr)
			}
		} else {
			_, _ = p.dockerClient.ContainerRemove(ctx, c.ID, dockerclient.ContainerRemoveOptions{Force: true})
		}
		return "", fmt.Errorf("failed to start runner container: %w", err)
	}

	p.logger.Debug("Runner started",
		slog.String("name", name),
		slog.String("containerID", c.ID),
		slog.Int("mounts", len(mounts)),
	)
	return c.ID, nil
}

// RemoveInstance removes a runner and its explicitly owned child resources.
func (p *DockerProvider) RemoveInstance(ctx context.Context, instanceID string) error {
	if err := RemoveRunnerContainer(ctx, p.dockerClient, instanceID, p.logger); err != nil {
		return err
	}
	p.workspaceMu.Lock()
	workspace := p.workspaces[instanceID]
	p.workspaceMu.Unlock()
	if workspace != nil {
		inUse, err := workspaceStillMounted(ctx, p.dockerClient, workspace.work)
		if err != nil {
			return err
		}
		if inUse {
			return fmt.Errorf("runner workspace is still mounted by another Docker container")
		}
	}
	if err := p.discardWorkspace(workspace); err != nil {
		return err
	}
	p.workspaceMu.Lock()
	delete(p.workspaces, instanceID)
	p.workspaceMu.Unlock()
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
// volumes or local cache guard are needed, otherwise a shell prelude that fixes the ownership of
// each volume mount point first. Freshly created named volumes are root-owned
// at the top level, so the chown is conditional on the mount point's owner:
// the first job on a fresh volume pays the recursive chown once and later
// jobs skip the IO storm entirely. (stat -c is GNU coreutils — the runner
// image is Ubuntu.)
func runnerCmd(volumeTargets []string, localActionsCache bool) []string {
	if len(volumeTargets) == 0 && !localActionsCache {
		return []string{"/home/runner/run.sh"}
	}
	var sb strings.Builder
	sb.WriteString(`fix_own() { [ "$(stat -c %u "$1")" = "1001" ] || sudo chown -R 1001:123 "$1"; };`)
	for _, target := range volumeTargets {
		fmt.Fprintf(&sb, " fix_own %s;", shell.Quote(target))
	}
	if localActionsCache {
		sb.WriteString(" /usr/local/bin/runner-local-cache-check &&")
	}
	sb.WriteString(" exec /home/runner/run.sh")
	return []string{"sh", "-c", sb.String()}
}

// buildContainerEnv returns the environment variables for a runner container.
func (p *DockerProvider) buildContainerEnv(jitConfig string) []string {
	env := []string{
		fmt.Sprintf("ACTIONS_RUNNER_INPUT_JITCONFIG=%s", jitConfig),
	}
	if p.sharedVolume != "" {
		env = append(env, fmt.Sprintf("SHARED_DIR=%s", p.sharedVolume))
	}
	if p.actionsCacheURL != "" {
		env = append(env, "CUSTOM_ACTIONS_RESULTS_URL="+p.actionsCacheURL)
	}
	return env
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
