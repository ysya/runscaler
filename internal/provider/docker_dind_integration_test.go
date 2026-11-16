package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/config"
)

// This opt-in test creates only UUID-named resources and removes only the
// groups it created. It never invokes a daemon-wide prune or orphan sweep.
// No GitHub token or live runner registration is used.
func TestDinDDockerIntegration(t *testing.T) {
	if os.Getenv("RUNSCALER_DIND_INTEGRATION") != "1" {
		t.Skip("set RUNSCALER_DIND_INTEGRATION=1 to run disposable privileged Docker integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		out, err := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			t.Fatal(err)
		}
		host = strings.TrimSpace(string(out))
	}
	client, err := dockerclient.New(dockerclient.WithHost(host))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err = client.Ping(ctx, dockerclient.PingOptions{NegotiateAPIVersion: true}); err != nil {
		t.Fatal(err)
	}
	buildxDir := t.TempDir()
	run := func(input string, args ...string) (string, error) {
		command := exec.CommandContext(ctx, "docker", append([]string{"--host", host}, args...)...)
		command.Env = append(os.Environ(), "BUILDX_CONFIG="+buildxDir)
		if input != "" {
			command.Stdin = strings.NewReader(input)
		}
		out, err := command.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(input string, args ...string) string {
		t.Helper()
		out, err := run(input, args...)
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return out
	}
	image := "runscaler-dind-integration:" + uuid.NewString()
	base := os.Getenv("RUNSCALER_DIND_RUNNER_IMAGE")
	if base == "" {
		base = "ghcr.io/actions/actions-runner:latest"
	}
	dockerfile := fmt.Sprintf("FROM %s\nUSER root\nRUN printf '#!/bin/sh\\nexec sleep infinity\\n' > /home/runner/run.sh && chmod 755 /home/runner/run.sh\nUSER runner\n", base)
	must(dockerfile, "build", "--pull=false", "--tag", image, "-")
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		out, err := exec.CommandContext(cleanup, "docker", "--host", host, "image", "rm", image).CombinedOutput()
		if err != nil {
			t.Errorf("remove test-only image: %v %s", err, out)
		}
	})
	sharedName := "runscaler-dind-test-shared-" + uuid.NewString()
	cacheName := "runscaler-dind-test-cache-" + uuid.NewString()
	for _, name := range []string{sharedName, cacheName} {
		if _, err := client.VolumeCreate(ctx, dockerclient.VolumeCreateOptions{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		for _, name := range []string{sharedName, cacheName} {
			if _, err := client.VolumeRemove(cleanup, name, dockerclient.VolumeRemoveOptions{}); err != nil {
				t.Errorf("remove test-only persistent store: %v", err)
			}
		}
	})
	p := NewDockerProvider(config.ScaleSetConfig{RunnerImage: image, Docker: config.DockerConfig{
		DaemonMode: "dind", SharedVolume: "/shared", SharedVolumeName: sharedName,
		CacheVolumes: []string{cacheName + ":/home/runner/.cache"},
	}}, client, slog.New(slog.DiscardHandler))
	groups := map[string]bool{}
	t.Cleanup(func() {
		for group := range groups {
			cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
			err := RemoveDinDInstance(cleanup, client, group, slog.Default())
			done()
			if err != nil {
				t.Errorf("clean test-owned group %s: %v", group, err)
			}
		}
	})
	assertGone := func(group string) {
		t.Helper()
		filter := make(dockerclient.Filters).Add("label", dindInstanceLabel+"="+group)
		cs, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true, Filters: filter})
		if err != nil {
			t.Fatal(err)
		}
		vs, err := client.VolumeList(ctx, dockerclient.VolumeListOptions{Filters: filter})
		if err != nil {
			t.Fatal(err)
		}
		if len(cs.Items) != 0 || len(vs.Items) != 0 {
			t.Fatalf("test group %s leaked %d containers and %d volumes", group, len(cs.Items), len(vs.Items))
		}
	}
	var runners, daemons, daemonIDs []string
	for i := range 2 {
		id, err := p.StartInstance(ctx, "runscaler-dind-test-"+uuid.NewString(), "")
		if err != nil {
			t.Fatal(err)
		}
		runners = append(runners, id)
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
			defer done()
			if err := p.RemoveInstance(cleanup, id); err != nil {
				t.Errorf("clean test-only runner: %v", err)
			}
		})
		ins, err := client.ContainerInspect(ctx, id, dockerclient.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		group := ins.Container.Config.Labels[dindInstanceLabel]
		groups[group] = true
		if ins.Container.HostConfig.Privileged {
			t.Fatal("runner is privileged")
		}
		if ins.Container.HostConfig.CgroupnsMode != "private" {
			t.Fatal("runner shares host cgroup namespace")
		}
		if cgroups := must("", "exec", id, "cat", "/proc/1/cgroup"); strings.Contains(cgroups, ":/docker/") {
			t.Fatalf("GitHub runner container compatibility check would fail: %s", cgroups)
		}
		cs, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true, Filters: make(dockerclient.Filters).Add("label", dindInstanceLabel+"="+group)})
		if err != nil {
			t.Fatal(err)
		}
		var daemon string
		for _, c := range cs.Items {
			if c.Labels[dindRoleLabel] == "daemon" {
				daemon = c.ID
			}
		}
		if daemon == "" {
			t.Fatal("missing daemon")
		}
		daemons = append(daemons, daemon)
		daemonIDs = append(daemonIDs, must("", "exec", id, "docker", "info", "--format", "{{.ID}}"))
		listeners := must("", "exec", daemon, "netstat", "-lnt")
		if strings.Contains(listeners, ":2375") || strings.Contains(listeners, ":2376") {
			t.Fatalf("Docker API exposed through TCP: %s", listeners)
		}
		if i == 1 {
			if out, err := run("", "exec", id, "docker", "image", "inspect", "runscaler-dind-check:local"); err == nil {
				t.Fatalf("job B sees job A image: %s", out)
			}
		}
		must("", "exec", id, "docker", "pull", "busybox:1.37")
		if i == 0 {
			must("", "exec", id, "sh", "-c", "printf shared-ok > /shared/probe; printf cache-ok > /home/runner/.cache/probe")
		}
		if got := must("", "exec", id, "docker", "run", "--rm", "-v", "/shared:/shared", "-v", "/home/runner/.cache:/cache", "busybox:1.37", "sh", "-c", "cat /shared/probe /cache/probe"); got != "shared-okcache-ok" {
			t.Fatalf("persistent store mirroring=%q", got)
		}
		must("", "exec", id, "docker", "network", "create", "same-name")
		must("", "exec", id, "docker", "volume", "create", "same-name")
		must("", "exec", id, "docker", "run", "-d", "--name", "same-name", "--network", "same-name", "--mount", "type=volume,src=same-name,dst=/data", "busybox:1.37", "sleep", "3600")
		build := "FROM busybox:1.37\nRUN printf built-ok > /probe\nCMD [\"cat\",\"/probe\"]\n"
		must(build, "exec", "-i", id, "docker", "buildx", "build", "--load", "--tag", "runscaler-dind-check:local", "-")
		if got := must("", "exec", id, "docker", "run", "--rm", "runscaler-dind-check:local"); got != "built-ok" {
			t.Fatalf("build/run result=%q", got)
		}
		must("", "exec", id, "sh", "-c", "printf workspace-ok > /home/runner/_work/probe")
		if got := must("", "exec", id, "docker", "run", "--rm", "-v", "/home/runner/_work:/work", "busybox:1.37", "cat", "/work/probe"); got != "workspace-ok" {
			t.Fatalf("workspace bind=%q", got)
		}
		must("", "exec", id, "docker", "run", "--rm", "-v", "/home/runner/externals:/tools:ro", "busybox:1.37", "sh", "-c", `test -n "$(ls /tools)"`)
		must("", "exec", id, "docker", "run", "-d", "--name", "http", "-p", "127.0.0.1:18080:8080", "busybox:1.37", "sh", "-c", "mkdir -p /www; printf service-ok > /www/index.html; exec httpd -f -p 8080 -h /www")
		if got := must("", "exec", id, "curl", "--fail", "--silent", "--retry", "10", "--retry-connrefused", "--retry-delay", "1", "http://127.0.0.1:18080"); got != "service-ok" {
			t.Fatalf("localhost service=%q", got)
		}
	}
	if daemonIDs[0] == daemonIDs[1] {
		t.Fatal("runners share a daemon identity")
	}
	t.Log("two independent daemon IDs; duplicate container/network/volume names; isolated images; Buildx build/run; workspace and tools bind mounts; service localhost verified")
	watchCtx, watchCancel := context.WithTimeout(ctx, 15*time.Second)
	defer watchCancel()
	exited := make(chan error, 1)
	go func() { exited <- p.WaitInstance(watchCtx, runners[0]) }()
	must("", "kill", daemons[0])
	if err := <-exited; err != nil {
		t.Fatalf("terminated runner not reported as stopped: %v", err)
	}
	state, err := client.ContainerInspect(ctx, runners[0], dockerclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if state.Container.State.Running {
		t.Fatal("runner survived daemon failure")
	}
	if err := p.RemoveInstance(ctx, runners[0]); err != nil {
		t.Fatal(err)
	}
	// Removing the second runner externally must not strand its daemon/data.
	must("", "rm", "--force", runners[1])
	if err := p.RemoveInstance(ctx, runners[1]); err != nil {
		t.Fatal(err)
	}
	for group := range groups {
		assertGone(group)
	}
	for _, name := range []string{sharedName, cacheName} {
		if _, err := client.VolumeInspect(ctx, name, dockerclient.VolumeInspectOptions{}); err != nil {
			t.Fatalf("persistent store removed with job: %v", err)
		}
	}
	t.Log("daemon-exit supervision, cleanup, and missing-runner recovery verified")
	// A forced not-ready health response exercises timeout and independent
	// rollback using real outer-daemon resources, without a live GitHub job.
	never := &dinDNeverReady{Client: client}
	short := 3 * time.Second
	failed := NewDockerProvider(config.ScaleSetConfig{RunnerImage: image, Docker: config.DockerConfig{DaemonMode: "dind", DinDStartupTimeout: &short}}, never, slog.New(slog.DiscardHandler))
	if _, err := failed.StartInstance(ctx, "runscaler-dind-test-"+uuid.NewString(), ""); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected startup timeout, got %v", err)
	}
	if never.group == "" {
		t.Fatal("timeout did not provision any test resources")
	}
	groups[never.group] = true
	assertGone(never.group)
	cancelCtx, cancelStartup := context.WithCancel(ctx)
	defer cancelStartup()
	canceledClient := &dinDNeverReady{Client: client, cancel: cancelStartup}
	canceledProvider := NewDockerProvider(config.ScaleSetConfig{RunnerImage: image, Docker: config.DockerConfig{DaemonMode: "dind"}}, canceledClient, slog.New(slog.DiscardHandler))
	if _, err := canceledProvider.StartInstance(cancelCtx, "runscaler-dind-test-"+uuid.NewString(), ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected startup cancellation, got %v", err)
	}
	groups[canceledClient.group] = true
	assertGone(canceledClient.group)
	// Inventory and remove a group with no surviving container metadata.
	orphan := uuid.NewString()
	groups[orphan] = true
	if _, err := client.VolumeCreate(ctx, dockerclient.VolumeCreateOptions{Name: "runscaler-dind-test-" + orphan, Labels: dindLabels(orphan, "data")}); err != nil {
		t.Fatal(err)
	}
	orphans, err := FindDinDOrphans(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(orphans, orphan) {
		t.Fatal("volume-only orphan was not found")
	}
	if err := RemoveDinDInstance(ctx, client, orphan, slog.Default()); err != nil {
		t.Fatal(err)
	}
	assertGone(orphan)
	t.Log("persistent store preservation, startup cancellation/timeout rollback, and volume-only recovery verified; all test groups empty")
}

type dinDNeverReady struct {
	*dockerclient.Client
	group  string
	cancel context.CancelFunc
}

func (c *dinDNeverReady) VolumeCreate(ctx context.Context, o dockerclient.VolumeCreateOptions) (dockerclient.VolumeCreateResult, error) {
	c.group = o.Labels[dindInstanceLabel]
	return c.Client.VolumeCreate(ctx, o)
}
func (c *dinDNeverReady) ContainerInspect(ctx context.Context, id string, o dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error) {
	r, err := c.Client.ContainerInspect(ctx, id, o)
	if err == nil && c.cancel != nil {
		c.cancel()
		return r, ctx.Err()
	}
	if err == nil && r.Container.State != nil {
		r.Container.State.Health = &container.Health{Status: container.Starting}
	}
	return r, err
}
