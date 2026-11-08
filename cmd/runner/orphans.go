package main

import (
	"context"
	"log/slog"
	"strings"

	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/provider"
)

// containerAPI is the slice of the Docker client that orphan reconciliation
// needs: listing and force-removing containers.
type containerAPI interface {
	ContainerList(ctx context.Context, options dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error)
	ContainerRemove(ctx context.Context, containerID string, options dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error)
}

// OrphanContainer is a container runner created that no live process is
// tracking — left behind when a previous run was killed without a chance to
// clean up (SIGKILL, OOM, power loss, panic).
type OrphanContainer struct {
	ID     string
	Name   string
	Status string
}

// findOrphanContainers lists containers this tool created, as recognised by
// provider.OwnsContainer: the managed-by label, or the runner name scheme
// for containers from releases that labelled them differently.
//
// SAFETY: every container this returns is assumed to belong to a previous
// run of this process. That holds only because internal/lock guarantees a
// single runner per host — without it, this would claim containers another
// live runner is actively using. If that guarantee is ever relaxed, this
// function and its callers must be revisited.
func findOrphanContainers(ctx context.Context, client containerAPI) ([]OrphanContainer, error) {
	result, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}

	var orphans []OrphanContainer
	for _, c := range result.Items {
		if !provider.OwnsContainer(c.Labels, c.Names) {
			continue
		}
		orphans = append(orphans, OrphanContainer{
			ID:     c.ID,
			Name:   containerDisplayName(c),
			Status: string(c.State),
		})
	}
	return orphans, nil
}

// containerDisplayName returns a human-friendly name for a container,
// falling back to a short container ID when Docker reports no names
// (e.g. partially-created containers tagged only by label).
func containerDisplayName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	if len(c.ID) >= 12 {
		return c.ID[:12]
	}
	return c.ID
}

// removeOrphanContainers force-removes each orphan and returns how many were
// removed. A failure on one container is logged and does not stop the rest —
// a leftover container is a smaller problem than aborting the sweep, and
// later this also runs during startup reconciliation, where refusing to
// start would be worse still.
func removeOrphanContainers(ctx context.Context, client containerAPI, orphans []OrphanContainer, logger *slog.Logger) (removed int) {
	for _, o := range orphans {
		if _, err := client.ContainerRemove(ctx, o.ID, dockerclient.ContainerRemoveOptions{Force: true}); err != nil {
			logger.Warn("Failed to remove orphaned container",
				slog.String("container", o.Name),
				slog.String("id", o.ID),
				slog.Any("error", err))
			continue
		}
		removed++
	}
	return removed
}
