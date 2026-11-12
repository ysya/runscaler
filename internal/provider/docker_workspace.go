package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	dockerclient "github.com/moby/moby/client"
)

const workspaceVolumeLabel = "runscaler.workspace"

type runnerWorkspace struct {
	dir    string
	work   string
	volume string
	socket string
	proxy  *workspaceProxy
}

func workspaceStillMounted(ctx context.Context, client RunnerCleanupAPI, work string) (bool, error) {
	result, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		return false, fmt.Errorf("list Docker mounts before workspace cleanup: %w", err)
	}
	return workspaceMountedBy(result.Items, work), nil
}

func workspaceMountedBy(containers []container.Summary, work string) bool {
	for _, c := range containers {
		for _, m := range c.Mounts {
			if m.Type == mount.TypeBind && workspaceBindSource(m.Source, work) {
				return true
			}
		}
	}
	return false
}

func workspaceBindSource(source, work string) bool {
	return work != "" && (source == work || strings.HasPrefix(source, work+string(os.PathSeparator)))
}

func validWorkspaceVolume(v volume.Volume) bool {
	return v.Name != "" && v.Driver == "local" && filepath.IsAbs(v.Mountpoint) &&
		filepath.Clean(v.Mountpoint) == v.Mountpoint && v.Mountpoint != "/" &&
		v.Labels[workspaceVolumeLabel] == "true" && runnerNamePattern.MatchString(v.Labels[runnerOwnerLabel])
}

// CleanupWorkspaceVolumes reclaims local volumes left by a crashed manager
// after runner reconciliation. A surviving bind or named-volume reference
// keeps the volume; no unrelated volume is touched.
func CleanupWorkspaceVolumes(ctx context.Context, client RunnerCleanupAPI) error {
	listed, err := client.VolumeList(ctx, dockerclient.VolumeListOptions{
		Filters: make(dockerclient.Filters).Add("label", workspaceVolumeLabel),
	})
	if err != nil {
		return fmt.Errorf("list runner workspace volumes: %w", err)
	}
	containers, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		return fmt.Errorf("list Docker mounts before workspace volume cleanup: %w", err)
	}
	var names []string
	for _, v := range listed.Items {
		if !validWorkspaceVolume(v) {
			continue
		}
		inUse := workspaceMountedBy(containers.Items, v.Mountpoint)
		for _, c := range containers.Items {
			for _, m := range c.Mounts {
				if m.Type == mount.TypeVolume && m.Name == v.Name {
					inUse = true
				}
			}
		}
		if !inUse {
			names = append(names, v.Name)
		}
	}
	slices.Sort(names)
	var errs []error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if _, err := client.VolumeRemove(ctx, name, dockerclient.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove runner workspace volume %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (p *DockerProvider) prepareWorkspace(ctx context.Context, name string) (*runnerWorkspace, error) {
	if p.workspaceRoot == "" {
		return nil, nil
	}
	if !runnerNamePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid runner name for private workspace")
	}
	if err := checkWorkspaceRoot(p.workspaceRoot); err != nil {
		return nil, err
	}
	dir := filepath.Join(p.workspaceRoot, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, fmt.Errorf("create private runner workspace: %w", err)
	}
	created, err := p.dockerClient.VolumeCreate(ctx, dockerclient.VolumeCreateOptions{
		Driver: "local",
		Labels: map[string]string{runnerOwnerLabel: name, workspaceVolumeLabel: "true"},
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("create private runner workspace volume: %w", err)
	}
	v := created.Volume
	ws := &runnerWorkspace{dir: dir, work: v.Mountpoint, volume: v.Name, socket: filepath.Join(dir, "docker.sock")}
	if !validWorkspaceVolume(v) || v.Labels[runnerOwnerLabel] != name {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if cleanupErr := p.discardWorkspace(cleanupCtx, ws); cleanupErr != nil {
			return nil, fmt.Errorf("invalid private runner workspace volume; cleanup: %w", cleanupErr)
		}
		return nil, fmt.Errorf("invalid private runner workspace volume")
	}
	proxy, err := startWorkspaceProxy(ws.socket, p.dockerSocket, ws.work)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if cleanupErr := p.discardWorkspace(cleanupCtx, ws); cleanupErr != nil {
			return nil, fmt.Errorf("%w; workspace cleanup: %v", err, cleanupErr)
		}
		return nil, err
	}
	ws.proxy = proxy
	return ws, nil
}

func checkWorkspaceRoot(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return fmt.Errorf("docker.workspace-root must be a clean absolute host path other than /")
	}
	root, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat docker.workspace-root: %w", err)
	}
	if !root.IsDir() || root.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("docker.workspace-root must be a private directory")
	}
	stat, ok := root.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("docker.workspace-root must be owned by the manager user")
	}
	return nil
}

// CleanupWorkspaceRoot reclaims private directories left when the previous
// manager exited before it could finish runner cleanup. Call only after the
// machine-wide lock is held and orphan containers were removed successfully.
func CleanupWorkspaceRoot(root string, remaining []container.Summary) error {
	if err := checkWorkspaceRoot(root); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !runnerNamePattern.MatchString(entry.Name()) {
			continue
		}
		if workspaceMountedBy(remaining, filepath.Join(root, entry.Name(), "work")) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("remove orphan runner workspace: %w", err)
		}
	}
	return nil
}

func (p *DockerProvider) discardWorkspace(ctx context.Context, ws *runnerWorkspace) error {
	if ws == nil {
		return nil
	}
	if ws.proxy != nil {
		if err := ws.proxy.Close(); err != nil {
			return fmt.Errorf("close runner Docker socket proxy: %w", err)
		}
	}
	if ws.volume != "" {
		if _, err := p.dockerClient.VolumeRemove(ctx, ws.volume, dockerclient.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("remove private runner workspace volume: %w", err)
		}
	}
	if err := os.RemoveAll(ws.dir); err != nil {
		return fmt.Errorf("remove private runner workspace: %w", err)
	}
	return nil
}
