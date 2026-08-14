package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/backend"
)

var poolNamePattern = regexp.MustCompile(`^pool-[0-9]+-[0-9]+$`)

// findOrphanTartVMs lists local VMs created by runner. OCI entries and local
// base images are deliberately excluded: only the exact ephemeral runner and
// warm-pool naming schemes are safe to claim.
//
// SAFETY: every VM this returns is assumed to belong to a previous run of
// this process. That holds only because internal/lock guarantees a single
// runner per host. If that guarantee is ever relaxed, this function and its
// callers must be revisited.
func findOrphanTartVMs(ctx context.Context, runner backend.CommandRunner) ([]string, error) {
	out, err := runner.Run(ctx, "tart", "list", "--format", "json")
	if err != nil {
		return nil, err
	}

	var vms []tartListEntry
	if err := json.Unmarshal(out, &vms); err != nil {
		return nil, fmt.Errorf("parse tart VM list: %w", err)
	}

	var orphans []string
	for _, vm := range vms {
		if vm.Source != "local" {
			continue
		}
		if runnerNamePattern.MatchString(vm.Name) || poolNamePattern.MatchString(vm.Name) {
			orphans = append(orphans, vm.Name)
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}

// removeOrphanTartVMs stops and deletes each orphan. Stop errors are expected
// for VMs that are already stopped; delete failures are logged and do not
// prevent the remaining VMs from being reconciled.
func removeOrphanTartVMs(ctx context.Context, runner backend.CommandRunner, orphans []string, logger *slog.Logger) (removed int) {
	for _, name := range orphans {
		if _, err := runner.Run(ctx, "tart", "stop", name); err != nil {
			logger.Debug("Could not stop orphaned Tart VM; it may already be stopped",
				slog.String("vm", name),
				slog.Any("error", err))
		}
		if _, err := runner.Run(ctx, "tart", "delete", name); err != nil {
			logger.Warn("Failed to delete orphaned Tart VM",
				slog.String("vm", name),
				slog.Any("error", err))
			continue
		}
		removed++
	}
	return removed
}

// reconcileOrphans removes resources left by a previous process before any
// scale set starts. Its safety depends on startScaling acquiring the
// machine-wide internal/lock before entering run; without that lock these
// resources could belong to another live runner process.
//
// Reconciliation never examines or removes Docker volumes. Shared and cache
// volumes intentionally outlive the process and may hold data needed by a
// later job in an in-flight workflow run.
func reconcileOrphans(ctx context.Context, dockerClients map[string]*dockerclient.Client, tartHomes []string, logger *slog.Logger) {
	sockets := make([]string, 0, len(dockerClients))
	for socket := range dockerClients {
		sockets = append(sockets, socket)
	}
	sort.Strings(sockets)
	for _, socket := range sockets {
		client := dockerClients[socket]
		orphans, err := findOrphanContainers(ctx, client)
		if err != nil {
			logger.Warn("Failed to list orphaned Docker containers; continuing startup",
				slog.String("socket", socket),
				slog.Any("error", err))
			continue
		}
		if len(orphans) == 0 {
			logger.Debug("No orphaned Docker containers found", slog.String("socket", socket))
			continue
		}
		names := make([]string, 0, len(orphans))
		for _, orphan := range orphans {
			names = append(names, orphan.Name)
		}
		removed := removeOrphanContainers(ctx, client, orphans, logger)
		logger.Info("Reconciled orphaned Docker containers",
			slog.String("socket", socket),
			slog.Int("found", len(orphans)),
			slog.Int("removed", removed),
			slog.Any("containers", names))
	}

	homes := uniqueSortedStrings(tartHomes)
	for _, home := range homes {
		runner := tartOrphanCommandRunner{home: home}
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
		removed := removeOrphanTartVMs(ctx, runner, orphans, logger)
		logger.Info("Reconciled orphaned Tart VMs",
			slog.String("tartHome", home),
			slog.Int("found", len(orphans)),
			slog.Int("removed", removed),
			slog.Any("vms", orphans))
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

// tartOrphanCommandRunner runs Tart commands against one configured
// TART_HOME. It lives in cmd/runner because startup reconciliation is a
// process-level concern rather than part of one Tart backend instance.
type tartOrphanCommandRunner struct {
	home string
}

func (r tartOrphanCommandRunner) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	if r.home != "" {
		cmd.Env = append(os.Environ(), "TART_HOME="+r.home)
	}
	return cmd
}

func (r tartOrphanCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := r.command(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (r tartOrphanCommandRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	cmd := r.command(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
