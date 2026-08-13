package cachestore

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"
	dockerclient "github.com/moby/moby/client"
)

// fakeDockerAPI implements backend.DockerAPI for cachestore's own tests.
// Field names are relied on by later cachestore tasks' tests (shared-volume
// and cache-volume stores reuse this exact double) — see task-2-report.md
// and task-3-report.md.
type fakeDockerAPI struct {
	rootDir string // Info() 回傳的 DockerRootDir

	containersPruneCalls int
	imagesPruneCalls     int
	buildCachePruneCalls int
	buildCachePruneOpts  []dockerclient.BuildCachePruneOptions

	// Optional error injection for the three prune calls, so tests can pin
	// that dockerGarbageStore/dockerBuildCacheStore's errors.Join behavior
	// (one prune failing does not stop the others, and every failure
	// surfaces in the returned error) actually holds — coverage that moved
	// here from internal/backend's now-deleted PruneDockerRuntime tests.
	containerPruneErr  error
	imagePruneErr      error
	buildCachePruneErr error

	volumesRemoved    []string // VolumeRemove 收到的 volume 名稱
	createdContainers int      // ContainerCreate 次數(helper container 用)
	helperScripts     []string // 每個 helper container 的 sh -c 腳本
	diskUsage         dockerclient.DiskUsageResult

	// createCalls holds the full ContainerCreate args, in order, so tests
	// can assert on the mounted volume, image, and labels — not just the
	// script (helperScripts) or the call count (createdContainers).
	createCalls []dockerclient.ContainerCreateOptions
	// containersRemoved holds the IDs passed to ContainerRemove, so tests
	// can confirm a helper container is always cleaned up.
	containersRemoved []string

	// Optional overrides for ContainerWait, mirroring mockDocker's fields in
	// internal/backend/docker_test.go. Unset (both zero), the wait succeeds
	// immediately with status 0.
	waitStatus int64
	waitErr    error

	// containerLogsStdout is the canned stdout ContainerLogs() returns,
	// framed as a single stdcopy stdout frame (see stdcopyStdoutFrame) so
	// callers exercise the same demultiplexing path (stdcopy.StdCopy)
	// production code uses against a real daemon. The shared-volume and
	// cache-volume stores (Task 3) parse this to read `du -sb` output.
	containerLogsStdout string

	// containerList and volumeList are the canned results ContainerList and
	// VolumeList return (default empty Items, matching every prior test's
	// expectations). The buildx store's tests (Task 4) populate these to
	// exercise CleanupOrphanedBuildxBuilders' actual builder-matching and
	// dangling-volume-reap logic through fakeDockerAPI, not just call counts.
	containerList dockerclient.ContainerListResult
	volumeList    dockerclient.VolumeListResult
}

func (f *fakeDockerAPI) ContainerCreate(_ context.Context, options dockerclient.ContainerCreateOptions) (dockerclient.ContainerCreateResult, error) {
	f.createdContainers++
	f.createCalls = append(f.createCalls, options)
	// Helper containers (shared-volume/cache-volume stores, Task 3) run
	// `sh -c <script>`, matching CleanupSharedVolumeStale's shape in
	// internal/backend/docker.go. Capture the script when present so those
	// tests can assert on it.
	if options.Config != nil && len(options.Config.Cmd) == 3 &&
		options.Config.Cmd[0] == "sh" && options.Config.Cmd[1] == "-c" {
		f.helperScripts = append(f.helperScripts, options.Config.Cmd[2])
	}
	return dockerclient.ContainerCreateResult{ID: "fake-container-id"}, nil
}

func (f *fakeDockerAPI) ContainerStart(_ context.Context, _ string, _ dockerclient.ContainerStartOptions) (dockerclient.ContainerStartResult, error) {
	return dockerclient.ContainerStartResult{}, nil
}

func (f *fakeDockerAPI) ContainerRemove(_ context.Context, containerID string, _ dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error) {
	f.containersRemoved = append(f.containersRemoved, containerID)
	return dockerclient.ContainerRemoveResult{}, nil
}

// ContainerWait completes immediately with waitStatus (default 0), or sends
// waitErr on the error channel instead when set.
func (f *fakeDockerAPI) ContainerWait(_ context.Context, _ string, _ dockerclient.ContainerWaitOptions) dockerclient.ContainerWaitResult {
	statusCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	if f.waitErr != nil {
		errCh <- f.waitErr
	} else {
		statusCh <- container.WaitResponse{StatusCode: f.waitStatus}
	}
	return dockerclient.ContainerWaitResult{Result: statusCh, Error: errCh}
}

// ContainerLogs returns containerLogsStdout wrapped in a single stdout-typed
// stdcopy frame — the wire format non-TTY containers' logs use (see
// ContainerLogs' doc comment in github.com/moby/moby/client) — so
// production's stdcopy.StdCopy demux path is exercised the same way it
// would be against a real daemon, rather than going untested.
func (f *fakeDockerAPI) ContainerLogs(_ context.Context, _ string, _ dockerclient.ContainerLogsOptions) (dockerclient.ContainerLogsResult, error) {
	return io.NopCloser(bytes.NewReader(stdcopyStdoutFrame(f.containerLogsStdout))), nil
}

// stdcopyStdoutFrame wraps payload in a single stdout-typed stdcopy frame:
// one byte of stream type, three unused bytes, a big-endian uint32 length,
// then the payload itself.
func stdcopyStdoutFrame(payload string) []byte {
	buf := make([]byte, 8+len(payload))
	buf[0] = byte(stdcopy.Stdout)
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(payload)))
	copy(buf[8:], payload)
	return buf
}

func (f *fakeDockerAPI) ContainerPrune(_ context.Context, _ dockerclient.ContainerPruneOptions) (dockerclient.ContainerPruneResult, error) {
	f.containersPruneCalls++
	if f.containerPruneErr != nil {
		return dockerclient.ContainerPruneResult{}, f.containerPruneErr
	}
	return dockerclient.ContainerPruneResult{}, nil
}

func (f *fakeDockerAPI) ImagePrune(_ context.Context, _ dockerclient.ImagePruneOptions) (dockerclient.ImagePruneResult, error) {
	f.imagesPruneCalls++
	if f.imagePruneErr != nil {
		return dockerclient.ImagePruneResult{}, f.imagePruneErr
	}
	return dockerclient.ImagePruneResult{}, nil
}

func (f *fakeDockerAPI) BuildCachePrune(_ context.Context, opts dockerclient.BuildCachePruneOptions) (dockerclient.BuildCachePruneResult, error) {
	f.buildCachePruneCalls++
	f.buildCachePruneOpts = append(f.buildCachePruneOpts, opts)
	if f.buildCachePruneErr != nil {
		return dockerclient.BuildCachePruneResult{}, f.buildCachePruneErr
	}
	return dockerclient.BuildCachePruneResult{}, nil
}

func (f *fakeDockerAPI) VolumeRemove(_ context.Context, volumeID string, _ dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error) {
	f.volumesRemoved = append(f.volumesRemoved, volumeID)
	return dockerclient.VolumeRemoveResult{}, nil
}

func (f *fakeDockerAPI) ContainerList(_ context.Context, _ dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	return f.containerList, nil
}

func (f *fakeDockerAPI) VolumeList(_ context.Context, _ dockerclient.VolumeListOptions) (dockerclient.VolumeListResult, error) {
	return f.volumeList, nil
}

func (f *fakeDockerAPI) Info(_ context.Context, _ dockerclient.InfoOptions) (dockerclient.SystemInfoResult, error) {
	return dockerclient.SystemInfoResult{Info: system.Info{DockerRootDir: f.rootDir}}, nil
}

func (f *fakeDockerAPI) DiskUsage(_ context.Context, _ dockerclient.DiskUsageOptions) (dockerclient.DiskUsageResult, error) {
	return f.diskUsage, nil
}
