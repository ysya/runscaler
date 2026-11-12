package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"

	"github.com/ysya/runscaler/internal/config"
)

func TestWorkspaceProxySocketAccessibleToRunnerUID(t *testing.T) {
	root := shortWorkspaceTestDir(t)
	private := filepath.Join(root, "runner-12345678")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(private, "docker.sock")
	proxy, err := startWorkspaceProxy(socket, filepath.Join(root, "upstream.sock"), "/var/lib/docker/volumes/test/_data")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0666 {
		t.Fatalf("socket mode = %04o, want 0666 so the runner UID can connect", got)
	}
}

func TestWorkspaceProxyRewritesContainerBinds(t *testing.T) {
	root := shortWorkspaceTestDir(t)
	daemonSocket := filepath.Join(root, "daemon.sock")
	listener, err := net.Listen("unix", daemonSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	requests := make(chan map[string]any, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		requests <- request
		w.WriteHeader(http.StatusCreated)
	})}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	hostWork := filepath.Join(root, "runner-a", "work")
	proxySocket := filepath.Join(root, "runner-a.sock")
	proxy, err := startWorkspaceProxy(proxySocket, daemonSocket, hostWork)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", proxySocket)
	}}}
	request := `{"Image":"node","HostConfig":{"Binds":["/home/runner/_work/repo:/github/workspace:rw","/home/runner/_workspace:/unrelated:ro"],"Mounts":[{"Type":"bind","Source":"/home/runner/_work/_temp","Target":"/github/runner_temp"},{"Type":"volume","Source":"data","Target":"/data"}]}}`
	resp, err := client.Post("http://docker/v1.48/containers/create", "application/json", strings.NewReader(request))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := <-requests
	host := got["HostConfig"].(map[string]any)
	binds := host["Binds"].([]any)
	if binds[0] != filepath.Join(hostWork, "repo")+":/github/workspace:rw" {
		t.Errorf("workspace bind = %v", binds[0])
	}
	if binds[1] != "/home/runner/_workspace:/unrelated:ro" {
		t.Errorf("unrelated bind changed: %v", binds[1])
	}
	mounts := host["Mounts"].([]any)
	if mounts[0].(map[string]any)["Source"] != filepath.Join(hostWork, "_temp") {
		t.Errorf("workspace mount = %v", mounts[0])
	}
	if mounts[1].(map[string]any)["Source"] != "data" {
		t.Errorf("volume mount changed: %v", mounts[1])
	}
	ping, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatal(err)
	}
	ping.Body.Close()
	if ping.StatusCode != http.StatusOK {
		t.Errorf("ping status = %d", ping.StatusCode)
	}
	plain, err := client.Post("http://docker/v1.48/containers/create", "application/json", strings.NewReader(`{"Image":"node"}`))
	if err != nil {
		t.Fatal(err)
	}
	plain.Body.Close()
	if plain.StatusCode != http.StatusCreated {
		t.Fatalf("plain container create status = %d, want %d", plain.StatusCode, http.StatusCreated)
	}
	if got := <-requests; got["Image"] != "node" {
		t.Errorf("plain container create body = %v", got)
	}
}

func TestDockerProviderWorkspaceIsPrivateAndRemoved(t *testing.T) {
	root := filepath.Join(shortWorkspaceTestDir(t), "workspaces")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	mock := &mockDocker{}
	provider := NewDockerProvider(config.ScaleSetConfig{RunnerImage: "test-image", Docker: config.DockerConfig{
		WorkspaceRoot: root,
	}}, mock, slog.New(slog.DiscardHandler))
	provider.dind = true
	provider.dockerSocket = filepath.Join(root, "upstream.sock")
	for _, name := range []string{"runner-12345678", "runner-abcdef12"} {
		if _, err := provider.StartInstance(context.Background(), name, "jit"); err != nil {
			t.Fatal(err)
		}
	}
	if len(mock.createCalls) != 2 {
		t.Fatalf("created = %d", len(mock.createCalls))
	}
	if len(mock.volumesCreated) != 2 || mock.volumesCreated[0].Driver != "local" || mock.volumesCreated[1].Driver != "local" {
		t.Fatalf("workspace volumes = %+v, want two local volumes", mock.volumesCreated)
	}
	for i, name := range []string{"runner-12345678", "runner-abcdef12"} {
		call := mock.createCalls[i]
		work := findMountByTarget(call.hostConfig.Mounts, "/home/runner/_work")
		if work == nil || work.Type != mount.TypeVolume || work.Source != mock.volumes[i].Name {
			t.Errorf("runner %s workspace mount = %+v", name, work)
		}
		if !strings.Contains(strings.Join(call.config.Cmd, " "), "fix_own '/home/runner/_work'") {
			t.Errorf("runner %s does not prepare workspace volume ownership: %v", name, call.config.Cmd)
		}
		if _, err := os.Stat(filepath.Join(root, name, "work")); !os.IsNotExist(err) {
			t.Errorf("runner %s still uses a host checkout directory: %v", name, err)
		}
		socket := findMountByTarget(call.hostConfig.Mounts, "/var/run/docker.sock")
		if socket == nil || socket.Source != filepath.Join(root, name, "docker.sock") {
			t.Errorf("runner %s socket mount = %+v", name, socket)
		}
		if info, err := os.Stat(socket.Source); err != nil || info.Mode().Perm() != 0666 {
			t.Errorf("runner %s cannot access private socket as a different UID: info=%v err=%v", name, info, err)
		}
	}
	firstVolume := mock.volumes[0].Name
	if err := provider.RemoveInstance(context.Background(), "sha256-runner-12345678"); err != nil {
		t.Fatal(err)
	}
	if len(mock.volumesRemoved) != 1 || mock.volumesRemoved[0] != firstVolume {
		t.Errorf("removed volumes = %v, want only first workspace volume", mock.volumesRemoved)
	}
	if _, err := os.Stat(filepath.Join(root, "runner-12345678")); !os.IsNotExist(err) {
		t.Errorf("removed runner workspace still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "runner-abcdef12", "docker.sock")); err != nil {
		t.Errorf("other runner socket removed: %v", err)
	}
	if err := provider.RemoveInstance(context.Background(), "sha256-runner-abcdef12"); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupWorkspaceRootRemovesOnlyOwnedRunnerEntries(t *testing.T) {
	root := filepath.Join(shortWorkspaceTestDir(t), "workspaces")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(root, "runner-12345678")
	if err := os.Mkdir(owned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "payload"), []byte("job data"), 0600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "user-data")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(shortWorkspaceTestDir(t), "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "runner-abcdef12")); err != nil {
		t.Fatal(err)
	}
	active := []container.Summary{{ID: "unowned-child", Mounts: []container.MountPoint{{Type: mount.TypeBind, Source: filepath.Join(owned, "work", "repo")}}}}
	if err := CleanupWorkspaceRoot(root, active); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); err != nil {
		t.Fatalf("workspace removed while mounted by a child: %v", err)
	}
	if err := CleanupWorkspaceRoot(root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(owned); !os.IsNotExist(err) {
		t.Errorf("owned workspace remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "runner-abcdef12")); !os.IsNotExist(err) {
		t.Errorf("owned symlink remains: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("symlink destination touched: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("unowned directory touched: %v", err)
	}
}

func TestDockerProviderRemovesWorkspaceBoundChild(t *testing.T) {
	root := filepath.Join(shortWorkspaceTestDir(t), "workspaces")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	mock := &mockDocker{}
	provider := NewDockerProvider(config.ScaleSetConfig{RunnerImage: "test-image", Docker: config.DockerConfig{WorkspaceRoot: root}}, mock, slog.New(slog.DiscardHandler))
	provider.dind = true
	provider.dockerSocket = filepath.Join(root, "upstream.sock")
	name := "runner-12345678"
	id, err := provider.StartInstance(context.Background(), name, "jit")
	if err != nil {
		t.Fatal(err)
	}
	work := mock.volumes[0].Mountpoint
	volumeName := mock.volumes[0].Name
	mock.containers = []container.Summary{
		{ID: id, Names: []string{"/" + name}, Mounts: []container.MountPoint{{Type: mount.TypeVolume, Name: volumeName, Destination: runnerWorkPath}}},
		{ID: "workspace-child", Mounts: []container.MountPoint{{Type: mount.TypeBind, Source: filepath.Join(work, "repo", "repo")}}},
		{ID: "other-child", Mounts: []container.MountPoint{{Type: mount.TypeBind, Source: "/var/lib/docker/volumes/other/_data/repo"}}},
	}
	if err := provider.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(mock.removed, []string{"workspace-child", id}) {
		t.Errorf("removed containers = %v, want workspace child then runner", mock.removed)
	}
	if !slices.Equal(mock.volumesRemoved, []string{volumeName}) {
		t.Errorf("removed volumes = %v, want only runner workspace volume", mock.volumesRemoved)
	}
	if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
		t.Errorf("workspace socket directory remains: %v", err)
	}
}

func TestDockerProviderIgnoresMalformedWorkspaceVolumeMountpoint(t *testing.T) {
	root := filepath.Join(shortWorkspaceTestDir(t), "workspaces")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	mock := &mockDocker{}
	p := NewDockerProvider(config.ScaleSetConfig{RunnerImage: "test-image", Docker: config.DockerConfig{WorkspaceRoot: root}}, mock, slog.New(slog.DiscardHandler))
	p.dockerSocket = filepath.Join(root, "upstream.sock")
	id, err := p.StartInstance(context.Background(), "runner-12345678", "jit")
	if err != nil {
		t.Fatal(err)
	}
	mock.volumes[0].Mountpoint = ""
	mock.containers = []container.Summary{
		{ID: id, Names: []string{"/runner-12345678"}, Mounts: []container.MountPoint{{Type: mount.TypeVolume, Name: mock.volumes[0].Name, Destination: runnerWorkPath}}},
		{ID: "unrelated", Mounts: []container.MountPoint{{Type: mount.TypeBind, Source: "/tmp/unrelated"}}},
	}
	if err := p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(mock.removed, []string{id}) {
		t.Fatalf("removed containers = %v, want only runner", mock.removed)
	}
}

func TestCleanupWorkspaceVolumesRemovesOnlyUnusedRunnerVolumes(t *testing.T) {
	workspace := func(name, runner string) volume.Volume {
		return volume.Volume{Name: name, Driver: "local", Mountpoint: "/var/lib/docker/volumes/" + name + "/_data", Labels: map[string]string{workspaceVolumeLabel: "true", runnerOwnerLabel: runner}}
	}
	mock := &mockDocker{volumes: []volume.Volume{
		workspace("unused", "runner-12345678"),
		workspace("bind-used", "runner-abcdef12"),
		workspace("volume-used", "runner-11223344"),
		workspace("not-owned", "other"),
		{Name: "cache", Driver: "local", Mountpoint: "/var/lib/docker/volumes/cache/_data", Labels: map[string]string{runnerOwnerLabel: "runner-12345678"}},
	}}
	mock.containers = []container.Summary{
		{ID: "bind-child", Mounts: []container.MountPoint{{Type: mount.TypeBind, Source: filepath.Join(mock.volumes[1].Mountpoint, "repo")}}},
		{ID: "volume-child", Mounts: []container.MountPoint{{Type: mount.TypeVolume, Name: mock.volumes[2].Name}}},
	}
	if err := CleanupWorkspaceVolumes(context.Background(), mock); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(mock.volumesRemoved, []string{"unused"}) {
		t.Errorf("removed volumes = %v, want only unused", mock.volumesRemoved)
	}
}

func TestDockerProviderWorkspaceVolumeCleanupRetries(t *testing.T) {
	root := filepath.Join(shortWorkspaceTestDir(t), "workspaces")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	mock := &mockDocker{}
	p := NewDockerProvider(config.ScaleSetConfig{RunnerImage: "test-image", Docker: config.DockerConfig{WorkspaceRoot: root}}, mock, slog.New(slog.DiscardHandler))
	p.dockerSocket = filepath.Join(root, "upstream.sock")
	id, err := p.StartInstance(context.Background(), "runner-12345678", "jit")
	if err != nil {
		t.Fatal(err)
	}
	mock.volumeRemoveErrOn = mock.volumes[0].Name
	if err := p.RemoveInstance(context.Background(), id); err == nil {
		t.Fatal("volume removal failure did not request cleanup retry")
	}
	if _, err := os.Stat(filepath.Join(root, "runner-12345678")); err != nil {
		t.Fatalf("retry metadata removed: %v", err)
	}
	mock.volumeRemoveErrOn = ""
	if err := p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "runner-12345678")); !os.IsNotExist(err) {
		t.Fatalf("socket directory remains after retry: %v", err)
	}
}

func TestDockerProviderWorkspaceVolumeRemovedWhenStartFails(t *testing.T) {
	root := filepath.Join(shortWorkspaceTestDir(t), "workspaces")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	mock := &mockDocker{startErr: errors.New("simulated start failure")}
	p := NewDockerProvider(config.ScaleSetConfig{RunnerImage: "test-image", Docker: config.DockerConfig{WorkspaceRoot: root}}, mock, slog.New(slog.DiscardHandler))
	p.dockerSocket = filepath.Join(root, "upstream.sock")
	if _, err := p.StartInstance(context.Background(), "runner-12345678", "jit"); err == nil {
		t.Fatal("start failure was ignored")
	}
	if !slices.Equal(mock.volumesRemoved, []string{"test-workspace-1"}) {
		t.Fatalf("workspace volume not removed after start failure: %v", mock.volumesRemoved)
	}
	if _, err := os.Stat(filepath.Join(root, "runner-12345678")); !os.IsNotExist(err) {
		t.Fatalf("socket directory remains after start failure: %v", err)
	}
}

func TestDockerProviderWorkspaceVolumeRemovedWhenStartIsCanceled(t *testing.T) {
	root := filepath.Join(shortWorkspaceTestDir(t), "workspaces")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mock := &mockDocker{startErr: context.Canceled, startHook: cancel, rejectCanceled: true}
	p := NewDockerProvider(config.ScaleSetConfig{RunnerImage: "test-image", Docker: config.DockerConfig{WorkspaceRoot: root}}, mock, slog.New(slog.DiscardHandler))
	p.dockerSocket = filepath.Join(root, "upstream.sock")
	if _, err := p.StartInstance(ctx, "runner-12345678", "jit"); err == nil {
		t.Fatal("start cancellation was ignored")
	}
	if !slices.Equal(mock.volumesRemoved, []string{"test-workspace-1"}) {
		t.Fatalf("workspace volume not removed after cancellation: %v", mock.volumesRemoved)
	}
	if _, err := os.Stat(filepath.Join(root, "runner-12345678")); !os.IsNotExist(err) {
		t.Fatalf("socket directory remains after cancellation: %v", err)
	}
}

func shortWorkspaceTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "runscaler-ws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
