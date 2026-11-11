package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
)

type runnerWorkspace struct {
	dir    string
	work   string
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
			if m.Type == mount.TypeBind && (m.Source == work || strings.HasPrefix(m.Source, work+string(os.PathSeparator))) {
				return true
			}
		}
	}
	return false
}

func (p *DockerProvider) prepareWorkspace(name string) (*runnerWorkspace, error) {
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
	ws := &runnerWorkspace{dir: dir, work: filepath.Join(dir, "work"), socket: filepath.Join(dir, "docker.sock")}
	if err := os.Mkdir(ws.work, 0700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("create runner work directory: %w", err)
	}
	proxy, err := startWorkspaceProxy(ws.socket, p.dockerSocket, ws.work)
	if err != nil {
		_ = os.RemoveAll(dir)
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

func (p *DockerProvider) discardWorkspace(ws *runnerWorkspace) error {
	if ws == nil {
		return nil
	}
	if ws.proxy != nil {
		if err := ws.proxy.Close(); err != nil {
			return fmt.Errorf("close runner Docker socket proxy: %w", err)
		}
	}
	if err := os.RemoveAll(ws.dir); err != nil {
		return fmt.Errorf("remove private runner workspace: %w", err)
	}
	return nil
}
