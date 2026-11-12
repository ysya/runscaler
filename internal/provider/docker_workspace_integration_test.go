package provider

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
)

// Run explicitly with RUNSCALER_TEST_DOCKER_SOCKET and a locally available
// shell image. It exercises daemon-side bind resolution through two proxies.
func TestDockerWorkspaceBindIntegration(t *testing.T) {
	socket := os.Getenv("RUNSCALER_TEST_DOCKER_SOCKET")
	if socket == "" {
		t.Skip("set RUNSCALER_TEST_DOCKER_SOCKET to run the Docker integration test")
	}
	image := os.Getenv("RUNSCALER_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set RUNSCALER_TEST_DOCKER_IMAGE to a locally available shell image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := dockerclient.New(dockerclient.FromEnv, dockerclient.WithHost("unix://"+socket))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Ping(ctx, dockerclient.PingOptions{NegotiateAPIVersion: true}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(shortWorkspaceTestDir(t), "workspaces")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	p := &DockerProvider{dockerClient: client, dockerSocket: socket, workspaceRoot: root, logger: slog.New(slog.DiscardHandler)}
	var workspaces []*runnerWorkspace
	for _, name := range []string{"runner-12345678", "runner-abcdef12"} {
		ws, err := p.prepareWorkspace(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		workspaces = append(workspaces, ws)
		t.Cleanup(func() {
			cleanCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			defer stop()
			if err := p.discardWorkspace(cleanCtx, ws); err != nil {
				t.Errorf("clean up workspace volume: %v", err)
			}
		})
	}
	if workspaces[0].volume == workspaces[1].volume || workspaces[0].work == workspaces[1].work {
		t.Fatal("concurrent workspaces share a Docker volume or mountpoint")
	}

	run := func(c *dockerclient.Client, cfg *container.Config, host *container.HostConfig) {
		t.Helper()
		created, err := c.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{Config: cfg, HostConfig: host})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			defer stop()
			if _, err := client.ContainerRemove(cleanCtx, created.ID, dockerclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
				t.Errorf("clean up test container: %v", err)
			}
		})
		if _, err := c.ContainerStart(ctx, created.ID, dockerclient.ContainerStartOptions{}); err != nil {
			t.Fatal(err)
		}
		wait := c.ContainerWait(ctx, created.ID, dockerclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
		select {
		case err := <-wait.Error:
			if err != nil {
				t.Fatal(err)
			}
		case result := <-wait.Result:
			if result.StatusCode != 0 {
				t.Fatalf("container exited %d: %s", result.StatusCode, result.Error)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	for i, ws := range workspaces {
		marker := []string{"alpha", "beta"}[i]
		run(client, &container.Config{Image: image, User: "0", Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", fmt.Sprintf("mkdir -p /work/repo/.devcontainer && printf '%s' > /work/repo/.devcontainer/devcontainer.json", marker)}},
			&container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: ws.volume, Target: "/work"}}})
	}
	for i, ws := range workspaces {
		proxyClient, err := dockerclient.New(dockerclient.FromEnv, dockerclient.WithHost("unix://"+ws.socket))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = proxyClient.Close() })
		marker := []string{"alpha", "beta"}[i]
		other := []string{"beta", "alpha"}[i]
		cmd := fmt.Sprintf("test -f /workspace/.devcontainer/devcontainer.json && grep -q %s /workspace/.devcontainer/devcontainer.json && ! grep -q %s /workspace/.devcontainer/devcontainer.json", marker, other)
		run(proxyClient, &container.Config{Image: image, Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", cmd}},
			&container.HostConfig{Binds: []string{runnerWorkPath + "/repo:/workspace:ro"}})
	}
}
