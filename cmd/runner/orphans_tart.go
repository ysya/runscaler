package main

import (
	"context"
	"log/slog"
	"sort"

	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/provider"
)

// findOrphanTartVMs lists the local VMs runner created, by the ownership
// rules in provider.OwnsTartVM: OCI entries and local base images are never
// claimed, only the exact ephemeral-runner, warm-pool and prune-marker
// naming schemes.
//
// SAFETY: every VM this returns is assumed to belong to a previous run of
// this process. That holds only because internal/lock guarantees a single
// runner per host. If that guarantee is ever relaxed, this function and its
// callers must be revisited.
func findOrphanTartVMs(ctx context.Context, runner provider.CommandRunner) ([]provider.TartVM, error) {
	vms, err := provider.ListTartVMs(ctx, runner)
	if err != nil {
		return nil, err
	}
	var orphans []provider.TartVM
	for _, vm := range vms {
		if provider.OwnsTartVM(vm) {
			orphans = append(orphans, vm)
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Name < orphans[j].Name })
	return orphans, nil
}

// removeOrphanTartVMs stops and deletes each orphan. Stop errors are expected
// for VMs that are already stopped; delete failures are logged and do not
// prevent the remaining VMs from being reconciled.
func removeOrphanTartVMs(ctx context.Context, runner provider.CommandRunner, orphans []provider.TartVM, logger *slog.Logger) (removed int) {
	for _, vm := range orphans {
		if _, err := runner.Run(ctx, "tart", "stop", vm.Name); err != nil {
			logger.Debug("Could not stop orphaned Tart VM; it may already be stopped",
				slog.String("vm", vm.Name),
				slog.Any("error", err))
		}
		if _, err := runner.Run(ctx, "tart", "delete", vm.Name); err != nil {
			logger.Warn("Failed to delete orphaned Tart VM",
				slog.String("vm", vm.Name),
				slog.Any("error", err))
			continue
		}
		removed++
	}
	return removed
}

// reconcileOrphans removes resources left by a previous process before any
// scale set starts. Its safety depends on startManager acquiring the
// machine-wide internal/lock before entering run; without that lock these
// resources could belong to another live runner process.
//
// Reconciliation reclaims anonymous, explicitly runner-owned temporary, and
// private workspace volumes. Shared/cache volumes outlive the process and may
// hold data needed by a later job in an in-flight workflow run.
func reconcileOrphans(ctx context.Context, dockerClients map[string]*dockerclient.Client, tartHomes []string, logger *slog.Logger, workspaceRoots ...string) {
	workspacesSafeToRemove := len(dockerClients) > 0
	var remainingContainers []container.Summary
	sockets := make([]string, 0, len(dockerClients))
	for socket := range dockerClients {
		sockets = append(sockets, socket)
	}
	sort.Strings(sockets)
	for _, socket := range sockets {
		client := dockerClients[socket]
		orphans, err := findOrphanContainers(ctx, client)
		if err != nil {
			workspacesSafeToRemove = false
			logger.Warn("Failed to list orphaned Docker containers; continuing startup",
				slog.String("socket", socket),
				slog.Any("error", err))
			continue
		}
		if len(orphans) == 0 {
			logger.Debug("No orphaned Docker containers found", slog.String("socket", socket))
		} else {
			names := make([]string, 0, len(orphans))
			for _, orphan := range orphans {
				names = append(names, orphan.Name)
			}
			removed := removeOrphanContainers(ctx, client, orphans, logger)
			if removed != len(orphans) {
				workspacesSafeToRemove = false
			}
			logger.Info("Reconciled orphaned Docker containers",
				slog.String("socket", socket),
				slog.Int("found", len(orphans)),
				slog.Int("removed", removed),
				slog.Any("containers", names))
		}
		remaining, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
		if err != nil {
			workspacesSafeToRemove = false
			logger.Warn("Failed to inspect Docker mounts before workspace cleanup", slog.String("socket", socket), slog.Any("error", err))
			continue
		}
		remainingContainers = append(remainingContainers, remaining.Items...)
	}
	if workspacesSafeToRemove {
		for _, socket := range sockets {
			if err := provider.CleanupWorkspaceVolumes(ctx, dockerClients[socket]); err != nil {
				logger.Warn("Failed to remove orphan runner workspace volumes", slog.String("socket", socket), slog.Any("error", err))
			}
		}
		for _, root := range uniqueSortedStrings(workspaceRoots) {
			if err := provider.CleanupWorkspaceRoot(root, remainingContainers); err != nil {
				logger.Warn("Failed to remove orphan runner workspaces", slog.String("root", root), slog.Any("error", err))
			}
		}
	}

	for _, home := range uniqueSortedStrings(tartHomes) {
		runner := provider.TartCommandRunner(home)
		orphans, err := findOrphanTartVMs(ctx, runner)
		if err != nil {
			logger.Warn("Failed to list orphaned Tart VMs; continuing startup",
				slog.String("tartHome", home),
				slog.Any("error", err))
			continue
		}
		if len(orphans) == 0 {
			logger.Debug("No orphaned Tart VMs found", slog.String("tartHome", home))
			continue
		}
		names := make([]string, 0, len(orphans))
		for _, vm := range orphans {
			names = append(names, vm.Name)
		}
		removed := removeOrphanTartVMs(ctx, runner, orphans, logger)
		logger.Info("Reconciled orphaned Tart VMs",
			slog.String("tartHome", home),
			slog.Int("found", len(orphans)),
			slog.Int("removed", removed),
			slog.Any("vms", names))
	}
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
