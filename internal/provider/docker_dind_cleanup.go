package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
)

const (
	dindManagedLabel  = "runscaler.dind-managed"
	dindInstanceLabel = "runscaler.dind-instance"
	dindRoleLabel     = "runscaler.dind-role"
	dindRunnerLabel   = "runscaler.dind-runner-id"
)

func dindLabels(instance, role string) map[string]string {
	return map[string]string{dindManagedLabel: "true", dindInstanceLabel: instance, dindRoleLabel: role}
}

func ownedDinDResource(labels map[string]string, roles ...string) bool {
	return labels[dindManagedLabel] == "true" && labels[dindInstanceLabel] != "" && slices.Contains(roles, labels[dindRoleLabel])
}

// FindDinDOrphans inventories resource groups, including failed starts that
// left only storage. Call only while holding the host-wide runner lock, before
// starting controllers; live instances have the same durable ownership labels.
func FindDinDOrphans(ctx context.Context, client RunnerCleanupAPI) ([]string, error) {
	containers, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true, Filters: make(dockerclient.Filters).Add("label", dindManagedLabel+"=true")})
	if err != nil {
		return nil, fmt.Errorf("list DinD containers: %w", err)
	}
	volumes, err := client.VolumeList(ctx, dockerclient.VolumeListOptions{Filters: make(dockerclient.Filters).Add("label", dindManagedLabel+"=true")})
	if err != nil {
		return nil, fmt.Errorf("list DinD volumes: %w", err)
	}
	groups := map[string]bool{}
	for _, c := range containers.Items {
		if ownedDinDResource(c.Labels, "runner", "daemon", "init") {
			groups[c.Labels[dindInstanceLabel]] = true
		}
	}
	for _, v := range volumes.Items {
		if ownedDinDResource(v.Labels, "data", "socket", "work", "externals", "owner") {
			groups[v.Labels[dindInstanceLabel]] = true
		}
	}
	result := make([]string, 0, len(groups))
	for g := range groups {
		result = append(result, g)
	}
	sort.Strings(result)
	return result, nil
}

// dinDGroupForRunner uses the ownership volume as a durable index when the
// runner was removed externally. Keep this index until all other storage has
// been removed so retry does not depend on container metadata surviving.
func dinDGroupForRunner(ctx context.Context, client RunnerCleanupAPI, id string, containers []container.Summary) (string, error) {
	for _, c := range containers {
		if c.ID == id && ownedDinDResource(c.Labels, "runner") {
			return c.Labels[dindInstanceLabel], nil
		}
	}
	volumes, err := client.VolumeList(ctx, dockerclient.VolumeListOptions{Filters: make(dockerclient.Filters).Add("label", dindRunnerLabel+"="+id)})
	if err != nil {
		return "", fmt.Errorf("find DinD ownership: %w", err)
	}
	for _, v := range volumes.Items {
		if ownedDinDResource(v.Labels, "owner") && v.Labels[dindRunnerLabel] == id {
			return v.Labels[dindInstanceLabel], nil
		}
	}
	return "", nil
}

// RemoveDinDInstance removes exactly one explicitly owned group. It also works
// for init-only, sidecar-only, and volume-only groups after a process crash.
func RemoveDinDInstance(ctx context.Context, client RunnerCleanupAPI, instance string, logger *slog.Logger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instance == "" {
		return errors.New("DinD instance identity is empty")
	}
	filter := make(dockerclient.Filters).Add("label", dindManagedLabel+"=true").Add("label", dindInstanceLabel+"="+instance)
	result, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return fmt.Errorf("list DinD group containers: %w", err)
	}
	volumes, err := client.VolumeList(ctx, dockerclient.VolumeListOptions{Filters: filter})
	if err != nil {
		return fmt.Errorf("list DinD group volumes: %w", err)
	}
	var objects []container.Summary
	for _, c := range result.Items {
		if c.Labels[dindInstanceLabel] == instance && ownedDinDResource(c.Labels, "runner", "daemon", "init") {
			objects = append(objects, c)
		}
	}
	// Stop and remove the runner before its network-namespace owner. Retain
	// all storage and the durable index on any container-removal failure.
	rank := map[string]int{"runner": 0, "init": 1, "daemon": 2}
	sort.Slice(objects, func(i, j int) bool {
		return rank[objects[i].Labels[dindRoleLabel]] < rank[objects[j].Labels[dindRoleLabel]]
	})
	var errs []error
	timeout := 0
	for _, c := range objects {
		if _, err := client.ContainerStop(ctx, c.ID, dockerclient.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) && !cerrdefs.IsNotModified(err) {
			errs = append(errs, fmt.Errorf("stop DinD %s: %w", c.Labels[dindRoleLabel], err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for _, c := range objects {
		if _, err := client.ContainerRemove(ctx, c.ID, dockerclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove DinD %s: %w", c.Labels[dindRoleLabel], err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	// The owner marker is last: successful container deletion must not erase
	// the lookup required for retrying a failed data-volume deletion.
	sort.Slice(volumes.Items, func(i, j int) bool {
		return volumes.Items[i].Labels[dindRoleLabel] != "owner" && volumes.Items[j].Labels[dindRoleLabel] == "owner"
	})
	for _, v := range volumes.Items {
		if v.Labels[dindInstanceLabel] != instance || !ownedDinDResource(v.Labels, "data", "socket", "work", "externals", "owner") {
			continue
		}
		if v.Labels[dindRoleLabel] == "owner" && len(errs) > 0 {
			continue
		}
		if _, err := client.VolumeRemove(ctx, v.Name, dockerclient.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove DinD %s volume: %w", v.Labels[dindRoleLabel], err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	logger.Debug("Removed DinD instance", slog.String("instance", instance))
	return ctx.Err()
}
