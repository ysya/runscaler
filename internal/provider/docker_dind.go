package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
)

func (p *DockerProvider) startDinDInstance(ctx context.Context, name, jit string) (runnerID string, retErr error) {
	client := p.dockerClient
	ctx, cancel := context.WithTimeout(ctx, p.dindConfig.EffectiveDinDStartupTimeout())
	defer cancel()
	if err := p.ensureRunnerImage(ctx); err != nil {
		return "", err
	}
	if err := p.ensureDockerImage(ctx, p.dindConfig.EffectiveDinDImage()); err != nil {
		return "", fmt.Errorf("prepare DinD daemon image: %w", err)
	}
	instance := uuid.NewString()
	prefix := "runscaler-dind-" + instance
	defer func() {
		if retErr != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cleanupCancel()
			if err := RemoveDinDInstance(cleanupCtx, client, instance, p.logger); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("rollback DinD instance %s: %w", instance, err))
			}
		}
	}()
	volumes := map[string]string{}
	for _, role := range []string{"data", "socket", "work", "externals"} {
		v, err := client.VolumeCreate(ctx, dockerclient.VolumeCreateOptions{Name: prefix + "-" + role, Labels: dindLabels(instance, role)})
		if err != nil {
			return "", fmt.Errorf("create DinD %s volume: %w", role, err)
		}
		volumes[role] = v.Volume.Name
	}
	vm := func(role, target string) mount.Mount {
		return mount.Mount{Type: mount.TypeVolume, Source: volumes[role], Target: target, VolumeOptions: &mount.VolumeOptions{NoCopy: true}}
	}
	init, err := client.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name: prefix + "-init", Platform: p.platform,
		Config: &container.Config{
			Image: p.runnerImage, User: "runner", Entrypoint: []string{"sh", "-c"},
			Cmd:    []string{`sudo chown 1001:123 /dind-work /dind-externals && cp -a /home/runner/externals/. /dind-externals/`},
			Labels: dindLabels(instance, "init"),
		},
		HostConfig: &container.HostConfig{
			Mounts:    []mount.Mount{vm("work", "/dind-work"), vm("externals", "/dind-externals")},
			Resources: p.containerResources(), NetworkMode: "none",
		},
	})
	if err != nil {
		return "", fmt.Errorf("create DinD initializer: %w", err)
	}
	if _, err = client.ContainerStart(ctx, init.ID, dockerclient.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start DinD initializer: %w", err)
	}
	if err = waitDinDInitializer(ctx, client, init.ID); err != nil {
		return "", err
	}
	if _, err = client.ContainerRemove(ctx, init.ID, dockerclient.ContainerRemoveOptions{RemoveVolumes: true}); err != nil {
		return "", fmt.Errorf("remove DinD initializer: %w", err)
	}
	socket := vm("socket", "/var/run")
	work := vm("work", "/home/runner/_work")
	externals := vm("externals", "/home/runner/externals")
	externals.ReadOnly = true
	daemonMounts := []mount.Mount{vm("data", "/var/lib/docker"), socket, work, externals}
	runnerMounts := []mount.Mount{socket, work}
	var targets []string
	if p.sharedVolume != "" {
		shared := p.sharedVolumeMount()
		runnerMounts = append(runnerMounts, shared)
		targets = append(targets, shared.Target)
		// The daemon starts first. Only the runner image may seed the shared volume.
		shared.VolumeOptions = &mount.VolumeOptions{NoCopy: true}
		daemonMounts = append(daemonMounts, shared)
	}
	limits := container.Resources{Memory: int64(p.dindConfig.DinDMemory) * 1024 * 1024, NanoCPUs: int64(p.dindConfig.DinDCPU) * 1_000_000_000}
	if p.dindConfig.DinDPidsLimit > 0 {
		limit := p.dindConfig.DinDPidsLimit
		limits.PidsLimit = &limit
	}
	daemonHost := &container.HostConfig{Privileged: true, Mounts: daemonMounts, Resources: limits}
	if p.network != "" {
		daemonHost.NetworkMode = container.NetworkMode(p.network)
	}
	daemon, err := client.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name: prefix + "-daemon", Platform: p.platform,
		Config: &container.Config{
			Image:  p.dindConfig.EffectiveDinDImage(),
			Cmd:    []string{"dockerd", "--host=unix:///var/run/docker.sock", "--group=123"},
			Labels: dindLabels(instance, "daemon"),
			Healthcheck: &container.HealthConfig{
				Test:     []string{"CMD", "docker", "--host=unix:///var/run/docker.sock", "info"},
				Interval: time.Second, Timeout: 5 * time.Second, Retries: 120,
			},
		},
		HostConfig: daemonHost,
	})
	if err != nil {
		return "", fmt.Errorf("create DinD daemon: %w", err)
	}
	if _, err = client.ContainerStart(ctx, daemon.ID, dockerclient.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start DinD daemon: %w", err)
	}
	if err = waitDinDReady(ctx, client, daemon.ID); err != nil {
		return "", err
	}
	env := append(p.buildContainerEnv(jit), "DOCKER_HOST=unix:///var/run/docker.sock")
	labels := dindLabels(instance, "runner")
	labels[ManagedByLabel] = ManagedByValue
	runner, err := client.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name: name, Platform: p.platform,
		Config: &container.Config{
			Image: p.runnerImage, User: "runner", Cmd: runnerCmd(targets, p.actionsCacheURL != ""), Env: env, Labels: labels,
		},
		HostConfig: &container.HostConfig{
			Mounts: runnerMounts, GroupAdd: []string{"123"},
			NetworkMode:  container.NetworkMode("container:" + daemon.ID),
			CgroupnsMode: "private",
			Resources:    p.containerResources(), SecurityOpt: []string{"label:disable"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create DinD runner: %w", err)
	}
	// Labels cannot be changed on existing containers or volumes. A tiny
	// ownership volume persists the ID->group mapping before the runner starts.
	owner := dindLabels(instance, "owner")
	owner[dindRunnerLabel] = runner.ID
	if _, err = client.VolumeCreate(ctx, dockerclient.VolumeCreateOptions{Name: prefix + "-owner", Labels: owner}); err != nil {
		return "", fmt.Errorf("record DinD ownership: %w", err)
	}
	if _, err = client.ContainerStart(ctx, runner.ID, dockerclient.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start DinD runner: %w", err)
	}
	p.logger.Debug("Runner started with private Docker daemon", slog.String("name", name), slog.String("containerID", runner.ID), slog.String("instance", instance))
	return runner.ID, nil
}

func waitDinDInitializer(ctx context.Context, client DockerAPI, id string) error {
	result, err := waitContainerResult(ctx, client, id)
	if err != nil {
		return fmt.Errorf("wait for DinD initializer: %w", err)
	}
	if result.StatusCode != 0 {
		return fmt.Errorf("DinD initializer exited with code %d", result.StatusCode)
	}
	return nil
}

func waitDinDReady(ctx context.Context, client DockerAPI, id string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := client.ContainerInspect(ctx, id, dockerclient.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect DinD daemon readiness: %w", err)
		}
		state := result.Container.State
		if state == nil || !state.Running {
			return errors.New("DinD daemon exited before becoming ready")
		}
		if state.Health != nil {
			switch state.Health.Status {
			case container.Healthy:
				return nil
			case container.Unhealthy:
				return errors.New("DinD daemon became unhealthy before startup completed")
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for DinD daemon readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (p *DockerProvider) waitDinDInstance(ctx context.Context, id string) error {
	result, err := p.dockerClient.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		return fmt.Errorf("find DinD instance: %w", err)
	}
	group, err := dinDGroupForRunner(ctx, p.dockerClient, id, result.Items)
	if err != nil {
		return err
	}
	if group == "" {
		return p.waitContainer(ctx, id)
	}
	var daemon string
	for _, c := range result.Items {
		if c.Labels[dindInstanceLabel] == group && ownedDinDResource(c.Labels, "daemon") {
			daemon = c.ID
			break
		}
	}
	if daemon == "" {
		return p.stopRunnerForDaemonFailure(ctx, id, errors.New("DinD daemon is missing"))
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type exit struct {
		daemon bool
		err    error
	}
	done := make(chan exit, 2)
	go func() { done <- exit{err: p.waitContainer(waitCtx, id)} }()
	go func() { done <- exit{daemon: true, err: p.waitContainer(waitCtx, daemon)} }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case result := <-done:
		if !result.daemon {
			return result.err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if result.err != nil {
			return fmt.Errorf("observe DinD daemon: %w", result.err)
		}
		return p.stopRunnerForDaemonFailure(ctx, id, errors.New("DinD daemon exited"))
	}
}

func (p *DockerProvider) stopRunnerForDaemonFailure(ctx context.Context, id string, cause error) error {
	timeout := 0
	_, err := p.dockerClient.ContainerStop(ctx, id, dockerclient.ContainerStopOptions{Timeout: &timeout})
	if err != nil && !cerrdefs.IsNotFound(err) && !cerrdefs.IsNotModified(err) {
		return errors.Join(cause, err)
	}
	p.logger.Warn("Runner stopped after private Docker daemon failure", slog.String("containerID", id), slog.Any("error", cause))
	// InstanceWatcher returns nil for confirmed termination. Returning the
	// daemon failure here would ask the controller to retry observation forever
	// when the daemon was removed, retaining the dead runner's capacity slot.
	return nil
}
