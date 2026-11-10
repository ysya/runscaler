package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
)

// RunnerCleanupAPI is the lifecycle-only surface needed to remove a runner
// and its explicitly owned resources. Daemon pruning remains in cachestore.
type RunnerCleanupAPI interface {
	ContainerStop(context.Context, string, dockerclient.ContainerStopOptions) (dockerclient.ContainerStopResult, error)
	ContainerList(context.Context, dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error)
	ContainerRemove(context.Context, string, dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error)
	VolumeList(context.Context, dockerclient.VolumeListOptions) (dockerclient.VolumeListResult, error)
	VolumeRemove(context.Context, string, dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error)
}

const runnerOwnerLabel = "runscaler.runner-id"

// RemoveRunnerContainer stops a runner, removes containers explicitly tied
// to it, then removes the runner itself. Keep the stopped runner's metadata
// until child cleanup succeeds so name-based ownership survives a retry.
// Anonymous volumes and explicitly owned temporary named volumes are reclaimed;
// shared/cache volumes, images, and daemon build caches are preserved.
func RemoveRunnerContainer(ctx context.Context, client RunnerCleanupAPI, instanceID string, logger *slog.Logger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instanceID == "" {
		return errors.New("runner container ID is empty")
	}

	// Stop before listing so the runner cannot create more socket-backed
	// containers during the sweep. Match the existing force-removal behavior
	// instead of adding a graceful-stop delay to the controller's deadline.
	timeout := 0
	if _, err := client.ContainerStop(ctx, instanceID, dockerclient.ContainerStopOptions{Timeout: &timeout}); err != nil &&
		!cerrdefs.IsNotFound(err) && !cerrdefs.IsNotModified(err) {
		return fmt.Errorf("failed to stop runner container: %w", err)
	}
	result, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		return fmt.Errorf("failed to list runner child containers: %w", err)
	}
	runner := container.Summary{ID: instanceID}
	for _, c := range result.Items {
		if c.ID == instanceID {
			runner = c
			break
		}
	}
	// Inventory labeled volumes before deleting children, while all of the
	// ownership evidence is still available. Buildx state volumes need no
	// extra label when an owned builder mounts its exact <builder>_state name.
	volumeResult, err := client.VolumeList(ctx, dockerclient.VolumeListOptions{
		Filters: make(dockerclient.Filters).Add("label", runnerOwnerLabel),
	})
	if err != nil {
		return fmt.Errorf("failed to list runner volumes: %w", err)
	}
	candidates := make(map[string]bool)
	for _, v := range volumeResult.Items {
		if referencesRunner(v.Labels[runnerOwnerLabel], runner, result.Items) {
			candidates[v.Name] = true
		}
	}

	var errs []error
	removed := make(map[string]bool)
	for _, c := range result.Items {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		// Another runner can share a network namespace intentionally; it has
		// its own lifecycle and must never be treated as a child container.
		if c.ID == instanceID || c.Labels["managed-by"] == "runner" {
			continue
		}
		networkRef, sharesContainerNetwork := strings.CutPrefix(c.HostConfig.NetworkMode, "container:")
		owned := sharesContainerNetwork && referencesRunner(networkRef, runner, result.Items)
		if !owned && !referencesRunner(c.Labels[runnerOwnerLabel], runner, result.Items) {
			continue
		}
		if builder := ownedBuildxName(c); builder != "" {
			for _, m := range c.Mounts {
				if m.Type == mount.TypeVolume && m.Name == builder+"_state" {
					candidates[m.Name] = true
				}
			}
		}
		if _, err := client.ContainerRemove(ctx, c.ID, dockerclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove runner child container %s: %w", c.ID, err))
			logger.Warn("Failed to remove runner child container",
				slog.String("runnerID", instanceID), slog.String("containerID", c.ID), slog.Any("error", err))
			continue
		}
		removed[c.ID] = true
		logger.Debug("Removed runner child container",
			slog.String("runnerID", instanceID), slog.String("containerID", c.ID))
	}
	// Retain every volume referenced by the runner, another container, or a
	// child whose removal failed. The daemon also checks current references
	// during VolumeRemove, covering mounts added after this inventory.
	for _, c := range result.Items {
		if removed[c.ID] {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == mount.TypeVolume {
				delete(candidates, m.Name)
			}
		}
	}
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		// Named-volume reclamation is best effort, like the existing buildx
		// sweeper. A driver failure must not block runner removal; report the
		// leftover explicitly. Never force past the daemon's volume checks.
		if _, err := client.VolumeRemove(ctx, name, dockerclient.VolumeRemoveOptions{}); err != nil {
			if !cerrdefs.IsNotFound(err) {
				logger.Warn("Failed to remove runner volume",
					slog.String("runnerID", instanceID), slog.String("volume", name), slog.Any("error", err))
			}
			continue
		}
		logger.Debug("Removed runner volume", slog.String("runnerID", instanceID), slog.String("volume", name))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := client.ContainerRemove(ctx, instanceID, dockerclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("failed to remove runner container: %w", err)
	}
	return nil
}

// referencesRunner accepts exact names/IDs and unambiguous ID prefixes of
// at least Docker's normal 12-character short-ID length. Shared bridge
// networks, name substrings, and ambiguous references prove no ownership.
func referencesRunner(ref string, runner container.Summary, containers []container.Summary) bool {
	if !matchesContainerReference(ref, runner) {
		return false
	}
	for _, c := range containers {
		if c.ID != runner.ID && matchesContainerReference(ref, c) {
			return false
		}
	}
	return true
}

func matchesContainerReference(ref string, c container.Summary) bool {
	if ref == "" {
		return false
	}
	if ref == c.ID || (len(ref) >= 12 && strings.HasPrefix(c.ID, ref)) {
		return true
	}
	for _, name := range c.Names {
		if strings.TrimPrefix(name, "/") == strings.TrimPrefix(ref, "/") {
			return true
		}
	}
	return false
}

// ownedBuildxName is used only after runner ownership has already been proven.
func ownedBuildxName(c container.Summary) string {
	for _, name := range c.Names {
		name = strings.TrimPrefix(name, "/")
		if strings.HasPrefix(name, "buildx_buildkit_") {
			return name
		}
	}
	return ""
}
