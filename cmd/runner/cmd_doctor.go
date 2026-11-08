package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	dockerclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/bytesize"
	"github.com/ysya/runscaler/internal/config"
	runnerlock "github.com/ysya/runscaler/internal/lock"
	"github.com/ysya/runscaler/internal/provider"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose GitHub connectivity and orphaned runners",
	Long: `Probe the GitHub API for every configured scale set with a five-second
timeout per probe, then check for Docker containers and Tart VMs left after
abnormal termination. Shared volumes are reported for visibility but never
removed automatically because they may hold data for an in-flight workflow.

By default, only reports what it finds. Use --fix to remove orphaned resources.`,
	Example: `  runner doctor          # Diagnose only
  runner doctor --fix    # Diagnose and clean up`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().Bool("fix", false, "Remove orphaned containers and VMs")
}

func runDoctor(cmd *cobra.Command, args []string) error {
	fix, _ := cmd.Flags().GetBool("fix")

	// Include the default and configured provider locations.
	dockerSockets := map[string]bool{config.DefaultDockerSocket: true}
	tartHomes := map[string]bool{"": true}
	if os.Getenv("RUNNER_MAINTENANCE_CONTEXT") == "1" {
		dockerSockets = map[string]bool{}
		tartHomes = map[string]bool{}
	}
	cfg, cfgErr := loadConfig(cmd)
	if cfgErr != nil {
		return fmt.Errorf("cannot load config for safe diagnosis: %w", cfgErr)
	}
	if targetInContext(cmd) != nil {
		dockerSockets = map[string]bool{}
		tartHomes = map[string]bool{}
	}
	scaleSets := cfg.ResolveScaleSets()
	for _, ss := range scaleSets {
		if ss.IsTart() {
			tartHomes[ss.Tart.Home] = true
		} else {
			dockerSockets[ss.Docker.Socket] = true
		}
	}
	// Surface non-fatal config diagnostics in the doctor report.
	for _, w := range cfg.Warnings {
		fmt.Printf("  ⚠ config: %s\n", w)
	}
	if fix && len(cfg.Warnings) > 0 {
		return fmt.Errorf("refusing destructive cleanup with config warnings; run 'runner validate' and fix them first")
	}

	// Safety check: take the same machine-wide lock as `runner run`. Unlike a
	// health probe this remains reliable when health is disabled, moved to a
	// different port, or the process is still starting.
	if fix {
		releaseLock, err := runnerlock.Acquire(runnerlock.DefaultPath, runnerlock.Info{
			PID:       os.Getpid(),
			StartedAt: time.Now(),
		})
		if err != nil {
			if errors.Is(err, runnerlock.ErrAlreadyRunning) {
				fmt.Println("  ✗ runner is currently running (machine lock is held)")
				fmt.Println("    Stop runner first before using --fix")
				return fmt.Errorf("cannot fix while runner is running")
			}
			return fmt.Errorf("cannot verify runner lock before cleanup: %w", err)
		}
		defer releaseLock()
	}

	// Scaleset API connectivity check (if config is available)
	apiErr := checkScalesetAPI(cmd.Context(), &cfg)

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	var totalOrphans int

	// Docker checks
	sharedVolumes := doctorSharedVolumes(dockerSockets, scaleSets)
	for socket := range dockerSockets {
		orphans, err := checkDocker(ctx, socket, sharedVolumes[socket], fix)
		if err != nil {
			return errors.Join(apiErr, err)
		}
		totalOrphans += orphans
	}

	// Tart checks
	orphans, err := checkTart(ctx, fix, tartHomes)
	if err != nil {
		return errors.Join(apiErr, err)
	}
	totalOrphans += orphans

	return reportDoctorSummary(cmd.OutOrStdout(), totalOrphans, fix, apiErr)
}

func reportDoctorSummary(out io.Writer, totalOrphans int, fix bool, apiErr error) error {
	fmt.Fprintln(out)
	if totalOrphans == 0 {
		if apiErr != nil {
			fmt.Fprintln(out, "No orphaned resources found. GitHub API diagnosis failed.")
		} else {
			fmt.Fprintln(out, "All clean. No orphaned resources found.")
		}
	} else if fix {
		fmt.Fprintf(out, "Cleanup incomplete: %d orphaned resource(s) remain.\n", totalOrphans)
		return errors.Join(apiErr, fmt.Errorf("orphaned resources remain after cleanup"))
	} else {
		fmt.Fprintf(out, "Found %d orphaned resource(s). Run 'runner doctor --fix' to clean up.\n", totalOrphans)
		return errors.Join(apiErr, fmt.Errorf("orphaned resources found"))
	}
	return apiErr
}

// checkScalesetAPI probes the read-only API for every configured scale set.
func checkScalesetAPI(ctx context.Context, cfg *config.Config) error {
	return checkScalesetAPIWith(ctx, cfg, os.Stdout, newScaleSetClient)
}

func checkScalesetAPIWith(ctx context.Context, cfg *config.Config, out io.Writer, newClient func(config.ScaleSetConfig) (scaleSetAPI, error)) error {
	scaleSets := cfg.ResolveScaleSets()
	var failures []error
	for _, ss := range scaleSets {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		probeCtx, cancel := context.WithTimeout(ctx, apiProbeTimeout)
		registered, err := probeScaleSet(probeCtx, ss, newClient)
		cancel()
		if err != nil {
			safeErr := safeAPIError(err)
			fmt.Fprintf(out, "  ✗ GitHub API [%s]: %s\n", ss.ScaleSetName, safeErr)
			failures = append(failures, fmt.Errorf("GitHub API [%s]: %w", ss.ScaleSetName, safeErr))
			continue
		}
		if registered {
			fmt.Fprintf(out, "  ✓ GitHub API [%s]: reachable; scale set registered\n", ss.ScaleSetName)
		} else {
			fmt.Fprintf(out, "  ✓ GitHub API [%s]: reachable; scale set not registered yet\n", ss.ScaleSetName)
		}
	}
	return errors.Join(failures...)
}

// checkDocker checks for Docker daemon reachability, orphaned containers, and
// the shared volumes configured on socket.
func checkDocker(ctx context.Context, socket string, sharedVolumes []string, fix bool) (int, error) {
	dockerClient, err := dockerclient.New(
		dockerclient.FromEnv,
		dockerclient.WithHost("unix://"+socket),
	)
	if err != nil {
		fmt.Println("  - Docker: not available (skipping)")
		return 0, nil
	}
	defer func() { _ = dockerClient.Close() }()

	if _, err := dockerClient.Ping(ctx, dockerclient.PingOptions{NegotiateAPIVersion: true}); err != nil {
		fmt.Println("  - Docker: not reachable (skipping)")
		return 0, nil
	}
	fmt.Println("  ✓ Docker daemon is reachable")
	fmt.Printf("    Socket: %s\n", socket)

	var totalOrphans int

	// Check orphaned containers
	orphans, err := checkDockerContainers(ctx, dockerClient, fix)
	if err != nil {
		return totalOrphans, err
	}
	totalOrphans += orphans

	// Report shared volumes
	orphans, err = checkDockerVolume(ctx, dockerClient, sharedVolumes)
	if err != nil {
		return totalOrphans, err
	}
	totalOrphans += orphans

	return totalOrphans, nil
}

// checkDockerContainers finds and optionally removes orphaned runner
// containers, via findOrphanContainers and removeOrphanContainers.
func checkDockerContainers(ctx context.Context, client containerAPI, fix bool) (int, error) {
	orphans, err := findOrphanContainers(ctx, client)
	if err != nil {
		fmt.Printf("  ✗ Failed to list Docker containers: %s\n", err)
		return 0, err
	}

	if len(orphans) == 0 {
		fmt.Println("  ✓ No orphaned Docker containers")
		return 0, nil
	}

	if !fix {
		fmt.Printf("  ⚠ Found %d orphaned Docker container(s)\n", len(orphans))
		for _, o := range orphans {
			fmt.Printf("      %s (%s)\n", o.Name, o.Status)
		}
		return len(orphans), nil
	}

	// Fix: remove orphaned containers. Per-container failures are logged
	// (see removeOrphanContainers) rather than printed, since the same
	// removal path also runs unattended during startup reconciliation.
	removed := removeOrphanContainers(ctx, client, orphans, slog.Default())
	fmt.Printf("  ✓ Removed %d orphaned Docker container(s)\n", removed)
	return len(orphans) - removed, nil
}

// volumeAPI is the subset of the Docker client doctor needs for the shared
// volume check (so it can be faked in tests). It intentionally omits
// VolumeRemove: checkDockerVolume must never delete the shared volume (see
// its doc comment below), and leaving removal out of the interface makes
// that a compile-time fact rather than a convention a future change could
// quietly break.
type volumeAPI interface {
	VolumeInspect(ctx context.Context, id string, options dockerclient.VolumeInspectOptions) (dockerclient.VolumeInspectResult, error)
}

// doctorSharedVolumes lists, per Docker socket, the shared volumes doctor
// reports: the default name, which a scale set uses when it names none, and
// every name the scale sets on that socket configure.
func doctorSharedVolumes(sockets map[string]bool, scaleSets []config.ScaleSetConfig) map[string][]string {
	names := make(map[string][]string, len(sockets))
	for socket := range sockets {
		names[socket] = []string{config.DefaultSharedVolumeName}
	}
	for _, ss := range scaleSets {
		if ss.IsTart() || ss.Docker.SharedVolume == "" {
			continue
		}
		if name := ss.SharedVolumeName(); !slices.Contains(names[ss.Docker.Socket], name) {
			names[ss.Docker.Socket] = append(names[ss.Docker.Socket], name)
		}
	}
	return names
}

// checkDockerVolume reports whether each named shared handoff volume exists
// and, for each one found, prints its name and size. It never removes
// anything, even under --fix: volumeAPI does not even expose a VolumeRemove
// method to call. Returns the number of unresolved issues, which this check
// never contributes to (a real Docker failure is instead returned as an
// error).
//
// Existence used to be a reliable orphan signal: runner deleted this
// volume itself at process exit, so anything still there after a run meant
// a crash or a kill. That stopped being true once runner stopped deleting
// it at exit. The volume is the mechanism by which one job in a workflow
// run hands data to a later job of the *same* run — a build job writes a
// tarball, a later job reads it (see cachestore.NewSharedVolumeStore) — so
// it is now designed to outlive the process for as long as that run is in
// flight. Its mere presence therefore carries no information about
// whether it is still needed: it looks identical whether the last run
// using it finished an hour ago or a later job of an active run simply
// has not started yet, and nothing observable from here tells those two
// cases apart.
//
// Do NOT restore automatic removal on the assumption this omission is a
// bug. `doctor --fix` running during a routine upgrade window would
// delete an in-flight run's handoff data, and the failure would surface
// as a missing file in the user's CI job, not as an error from this
// command — the exact failure mode already fixed on the exit path, just
// re-opened through a different trigger. Reclaiming genuinely stale
// content is already handled correctly elsewhere, by mechanisms that can
// tell "stale" from "in use" the way bare existence cannot: with
// shared-volume-max-age configured, the Tier3 sweep
// (cachestore.NewSharedVolumeStore) deletes files inside the volume past
// their TTL without ever removing the volume itself; left unconfigured,
// the volume grows unbounded by design and runner warns about that at
// startup — a capacity problem for the operator to configure away, not a
// signal for doctor to act on. An operator who has independently
// confirmed the volume is no longer needed (e.g. no workflow is running)
// can still remove it by hand with `docker volume rm`.
func checkDockerVolume(ctx context.Context, client volumeAPI, names []string) (int, error) {
	found := 0
	for _, name := range names {
		result, err := client.VolumeInspect(ctx, name, dockerclient.VolumeInspectOptions{})
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				continue
			}
			return 0, fmt.Errorf("inspect Docker volume %s: %w", name, err)
		}
		found++
		fmt.Printf("  ℹ Shared volume %s exists (%s)\n", name, volumeSizeText(result))
		fmt.Println("    It is designed to outlive the process and may still be needed by an in-flight workflow run.")
		fmt.Printf("    If you are certain it is no longer needed, remove it manually: docker volume rm %s\n", name)
	}
	if found == 0 {
		fmt.Println("  ✓ No shared Docker volume present")
	}
	return 0, nil
}

// volumeSizeText formats a volume's on-disk usage for doctor's report, or
// says plainly when Docker did not report one. UsageData is documented as
// populated only by the disk-usage endpoint (`docker system df`) — a plain
// volume inspect, which is all this check performs, leaves it nil — so
// "not reported" is the expected outcome here, not a sign anything is
// wrong. A negative Size (the driver's "not available" sentinel for
// non-local volume drivers) is treated the same way.
func volumeSizeText(result dockerclient.VolumeInspectResult) string {
	usage := result.Volume.UsageData
	if usage == nil || usage.Size < 0 {
		return "size not reported"
	}
	return bytesize.Format(uint64(usage.Size))
}

// checkTart checks for Tart binary and orphaned VMs.
func checkTart(ctx context.Context, fix bool, homes map[string]bool) (int, error) {
	if _, err := exec.LookPath("tart"); err != nil {
		fmt.Println("  - Tart: not installed (skipping)")
		return 0, nil
	}
	fmt.Println("  ✓ Tart binary found")

	var total int
	for home := range homes {
		issues, err := checkTartVMs(ctx, provider.TartCommandRunner(home), home, fix)
		if err != nil {
			return total, err
		}
		total += issues
	}
	return total, nil
}

// checkTartVMs reports, and with fix removes, the Tart VMs runner left
// behind in one TART_HOME, through the same ownership rules and removal
// path as startup reconciliation (findOrphanTartVMs, removeOrphanTartVMs).
func checkTartVMs(ctx context.Context, runner provider.CommandRunner, home string, fix bool) (int, error) {
	orphans, err := findOrphanTartVMs(ctx, runner)
	if err != nil {
		fmt.Printf("  ✗ Failed to list Tart VMs: %s\n", err)
		return 0, err
	}

	if len(orphans) == 0 {
		fmt.Printf("  ✓ No orphaned Tart VMs (TART_HOME=%q)\n", home)
		return 0, nil
	}

	if !fix {
		fmt.Printf("  ⚠ Found %d orphaned Tart VM(s)\n", len(orphans))
		for _, vm := range orphans {
			fmt.Printf("      %s (%s)\n", vm.Name, vm.State)
		}
		return len(orphans), nil
	}

	// Per-VM failures are logged (see removeOrphanTartVMs) rather than
	// printed, as for containers: the same removal path runs unattended
	// during startup reconciliation.
	removed := removeOrphanTartVMs(ctx, runner, orphans, slog.Default())
	fmt.Printf("  ✓ Removed %d orphaned Tart VM(s)\n", removed)
	return len(orphans) - removed, nil
}
