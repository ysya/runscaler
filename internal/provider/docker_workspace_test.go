package provider

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/ysya/runscaler/internal/config"
)

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
	for i, name := range []string{"runner-12345678", "runner-abcdef12"} {
		call := mock.createCalls[i]
		work := findMountByTarget(call.hostConfig.Mounts, "/home/runner/_work")
		if work == nil || work.Type != mount.TypeBind || work.Source != filepath.Join(root, name, "work") {
			t.Errorf("runner %s workspace mount = %+v", name, work)
		}
		socket := findMountByTarget(call.hostConfig.Mounts, "/var/run/docker.sock")
		if socket == nil || socket.Source != filepath.Join(root, name, "docker.sock") {
			t.Errorf("runner %s socket mount = %+v", name, socket)
		}
	}
	if err := provider.RemoveInstance(context.Background(), "sha256-runner-12345678"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "runner-12345678")); !os.IsNotExist(err) {
		t.Errorf("removed runner workspace still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "runner-abcdef12", "work")); err != nil {
		t.Errorf("other runner workspace removed: %v", err)
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

func TestDockerProviderPreservesWorkspaceWhileChildUsesBind(t *testing.T) {
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
	work := filepath.Join(root, name, "work")
	mock.containers = []container.Summary{{ID: "unowned-child", Mounts: []container.MountPoint{{Type: mount.TypeBind, Source: filepath.Join(work, "repo")}}}}
	if err := provider.RemoveInstance(context.Background(), id); err == nil {
		t.Fatal("removed workspace while an unowned child still mounts it")
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("active workspace removed: %v", err)
	}
	mock.containers = nil
	if err := provider.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Errorf("workspace remains after child exits: %v", err)
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
