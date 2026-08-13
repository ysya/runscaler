package cachestore

import (
	"context"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"
	dockerclient "github.com/moby/moby/client"
)

// fakeDockerAPI implements backend.DockerAPI for cachestore's own tests.
// Field names are relied on by later cachestore tasks' tests (shared-volume
// and cache-volume stores reuse this exact double) — see task-2-report.md.
type fakeDockerAPI struct {
	rootDir string // Info() 回傳的 DockerRootDir

	containersPruneCalls int
	imagesPruneCalls     int
	buildCachePruneCalls int
	buildCachePruneOpts  []dockerclient.BuildCachePruneOptions

	volumesRemoved    []string // VolumeRemove 收到的 volume 名稱
	createdContainers int      // ContainerCreate 次數(helper container 用)
	helperScripts     []string // 每個 helper container 的 sh -c 腳本
	diskUsage         dockerclient.DiskUsageResult
}

func (f *fakeDockerAPI) ContainerCreate(_ context.Context, options dockerclient.ContainerCreateOptions) (dockerclient.ContainerCreateResult, error) {
	f.createdContainers++
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

func (f *fakeDockerAPI) ContainerRemove(_ context.Context, _ string, _ dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error) {
	return dockerclient.ContainerRemoveResult{}, nil
}

// ContainerWait always completes immediately with status 0 — no fake test in
// this package (so far) needs a non-trivial exit path.
func (f *fakeDockerAPI) ContainerWait(_ context.Context, _ string, _ dockerclient.ContainerWaitOptions) dockerclient.ContainerWaitResult {
	statusCh := make(chan container.WaitResponse, 1)
	statusCh <- container.WaitResponse{StatusCode: 0}
	return dockerclient.ContainerWaitResult{Result: statusCh, Error: make(chan error, 1)}
}

func (f *fakeDockerAPI) ContainerPrune(_ context.Context, _ dockerclient.ContainerPruneOptions) (dockerclient.ContainerPruneResult, error) {
	f.containersPruneCalls++
	return dockerclient.ContainerPruneResult{}, nil
}

func (f *fakeDockerAPI) ImagePrune(_ context.Context, _ dockerclient.ImagePruneOptions) (dockerclient.ImagePruneResult, error) {
	f.imagesPruneCalls++
	return dockerclient.ImagePruneResult{}, nil
}

func (f *fakeDockerAPI) BuildCachePrune(_ context.Context, opts dockerclient.BuildCachePruneOptions) (dockerclient.BuildCachePruneResult, error) {
	f.buildCachePruneCalls++
	f.buildCachePruneOpts = append(f.buildCachePruneOpts, opts)
	return dockerclient.BuildCachePruneResult{}, nil
}

func (f *fakeDockerAPI) VolumeRemove(_ context.Context, volumeID string, _ dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error) {
	f.volumesRemoved = append(f.volumesRemoved, volumeID)
	return dockerclient.VolumeRemoveResult{}, nil
}

func (f *fakeDockerAPI) ContainerList(_ context.Context, _ dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	return dockerclient.ContainerListResult{}, nil
}

func (f *fakeDockerAPI) VolumeList(_ context.Context, _ dockerclient.VolumeListOptions) (dockerclient.VolumeListResult, error) {
	return dockerclient.VolumeListResult{}, nil
}

func (f *fakeDockerAPI) Info(_ context.Context, _ dockerclient.InfoOptions) (dockerclient.SystemInfoResult, error) {
	return dockerclient.SystemInfoResult{Info: system.Info{DockerRootDir: f.rootDir}}, nil
}

func (f *fakeDockerAPI) DiskUsage(_ context.Context, _ dockerclient.DiskUsageOptions) (dockerclient.DiskUsageResult, error) {
	return f.diskUsage, nil
}
