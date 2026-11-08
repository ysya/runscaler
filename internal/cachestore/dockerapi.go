package cachestore

import (
	"context"

	dockerclient "github.com/moby/moby/client"
)

// DockerAPI is the daemon surface the Docker-backed stores need: helper
// containers for measuring and sweeping volumes, the three prune calls,
// buildx builder discovery and removal, and the data-root and disk-usage
// lookups. *dockerclient.Client satisfies it; tests use fakeDockerAPI.
type DockerAPI interface {
	ContainerCreate(ctx context.Context, options dockerclient.ContainerCreateOptions) (dockerclient.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options dockerclient.ContainerStartOptions) (dockerclient.ContainerStartResult, error)
	ContainerWait(ctx context.Context, containerID string, options dockerclient.ContainerWaitOptions) dockerclient.ContainerWaitResult
	ContainerLogs(ctx context.Context, containerID string, options dockerclient.ContainerLogsOptions) (dockerclient.ContainerLogsResult, error)
	ContainerRemove(ctx context.Context, containerID string, options dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error)
	ContainerList(ctx context.Context, options dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error)
	VolumeList(ctx context.Context, options dockerclient.VolumeListOptions) (dockerclient.VolumeListResult, error)
	VolumeRemove(ctx context.Context, volumeID string, options dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error)
	ContainerPrune(ctx context.Context, options dockerclient.ContainerPruneOptions) (dockerclient.ContainerPruneResult, error)
	ImagePrune(ctx context.Context, options dockerclient.ImagePruneOptions) (dockerclient.ImagePruneResult, error)
	BuildCachePrune(ctx context.Context, options dockerclient.BuildCachePruneOptions) (dockerclient.BuildCachePruneResult, error)
	Info(ctx context.Context, options dockerclient.InfoOptions) (dockerclient.SystemInfoResult, error)
	DiskUsage(ctx context.Context, options dockerclient.DiskUsageOptions) (dockerclient.DiskUsageResult, error)
}
