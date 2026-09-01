package main

import (
	"context"
	"log/slog"

	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/provider"
)

// OrphanContainer is a container runner created that no live process is
// tracking — left behind when a previous run was killed without a chance to
// clean up (SIGKILL, OOM, power loss, panic).
type OrphanContainer struct {
	ID     string
	Name   string
	Status string
}

// findOrphanContainers lists containers this tool created, identified by the
// managed-by label it sets on every runner container, plus a name-pattern
// fallback for containers created before the label existed.
//
// SAFETY: every container this returns is assumed to belong to a previous
// run of this process. That holds only because internal/lock guarantees a
// single runner per host — without it, this would claim containers another
// live runner is actively using. If that guarantee is ever relaxed, this
// function and its callers must be revisited.
func findOrphanContainers(ctx context.Context, client provider.DockerAPI) ([]OrphanContainer, error) {
	result, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}

	var orphans []OrphanContainer
	for _, c := range result.Items {
		owned := c.Labels["managed-by"] == "runner"
		if !owned {
			for _, name := range c.Names {
				if runnerNamePattern.MatchString(name) {
					owned = true
					break
				}
			}
		}
		if !owned {
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

// removeOrphanContainers force-removes each orphan and returns how many were
// removed. A failure on one container is logged and does not stop the rest —
// a leftover container is a smaller problem than aborting the sweep, and
// later this also runs during startup reconciliation, where refusing to
// start would be worse still.
func removeOrphanContainers(ctx context.Context, client provider.DockerAPI, orphans []OrphanContainer, logger *slog.Logger) (removed int) {
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
