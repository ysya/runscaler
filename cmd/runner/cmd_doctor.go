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
	"strings"
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
	for _, d := range cfg.Deprecations {
		fmt.Printf("  ⚠ config: %s\n", d)
	}
	if fix && len(cfg.Warnings) > 0 {
		return fmt.Errorf("refusing destructive cleanup with config warnings; run 'runner validate' and fix them first")
	}

	mode, releaseLock, err := doctorResourceCheck(fix)
	if err != nil {
		return err
	}
	if releaseLock != nil {
		defer releaseLock()
	}

	// Scaleset API connectivity check (if config is available)
	apiErr := doctorGitHubCheck(cmd.Context(), &cfg)

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	var totalOrphans int

	// Docker checks
	sharedVolumes := doctorSharedVolumes(dockerSockets, scaleSets)
	for socket := range dockerSockets {
		orphans, err := checkDocker(ctx, socket, sharedVolumes[socket], mode)
		if err != nil {
			return errors.Join(apiErr, err)
		}
		totalOrphans += orphans
	}

	// Tart checks
	orphans, err := checkTart(ctx, mode, tartHomes)
	if err != nil {
		return errors.Join(apiErr, err)
	}
	totalOrphans += orphans

	return reportDoctorSummary(cmd.OutOrStdout(), totalOrphans, fix, apiErr)
}

// doctorGitHubCheck probes GitHub for every configured scale set or, when no
// config file was found and nothing configured a scale set, says so and
// skips the probe rather than report an empty scale set as a failure.
func doctorGitHubCheck(ctx context.Context, cfg *config.Config) error {
	if !configMissing(*cfg) {
		return checkScalesetAPI(ctx, cfg)
	}
	fmt.Println("  - GitHub API: no config file found (skipping)")
	if files := searchedConfigFiles(); len(files) > 0 {
		fmt.Printf("    Searched %s\n", strings.Join(files, ", "))
	}
	fmt.Println("    Create one with 'runner init', or pass --config PATH")
	return nil
}

// resourceCheck is how doctor treats the containers and VMs runner owns.
type resourceCheck int

const (
	// reportOrphans lists them as left behind: no runner is running.
	reportOrphans resourceCheck = iota
	// removeOrphans removes them under the run lock (--fix).
	removeOrphans
	// reportInUse lists them as the running runner's own.
	reportInUse
)

// doctorResourceCheck decides how doctor treats runner's containers and VMs
// from the machine-wide lock `runner run` holds. Unlike a health probe, the
// lock stays reliable when health is disabled, moved to another port, or the
// process is still starting. --fix holds the lock for the whole cleanup and
// returns its release. A diagnosis only probes it: while a runner holds it,
// runner's containers and VMs are that runner's, not leftovers.
func doctorResourceCheck(fix bool) (resourceCheck, func(), error) {
	if fix {
		release, err := runnerlock.Acquire(runnerLockPath, runnerlock.Info{PID: os.Getpid(), StartedAt: time.Now()})
		if err != nil {
			if errors.Is(err, runnerlock.ErrAlreadyRunning) {
				fmt.Println("  ✗ runner is currently running (machine lock is held)")
				fmt.Println("    Stop runner first before using --fix")
				return reportOrphans, nil, fmt.Errorf("cannot fix while runner is running")
			}
			return reportOrphans, nil, fmt.Errorf("cannot verify runner lock before cleanup: %w", err)
		}
		return removeOrphans, release, nil
	}
	running, err := probeRunningRunner()
	switch {
	case err != nil:
		fmt.Printf("  ⚠ Cannot tell whether runner is running: %s\n", err)
		fmt.Println("    Containers and VMs runner owns are listed as orphans even if a runner is using them")
	case running != nil:
		fmt.Printf("  ℹ runner is running (%s)\n", running.Detail())
		fmt.Println("    The containers and VMs it owns are in use, so they are not reported as orphans")
		return reportInUse, nil, nil
	}
	return reportOrphans, nil, nil
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
	var groups []probedRunnerGroup
	for _, ss := range scaleSets {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		probeCtx, cancel := context.WithTimeout(ctx, apiProbeTimeout)
		probe, err := probeScaleSet(probeCtx, ss, newClient)
		cancel()
		if err != nil {
			safeErr := safeAPIError(err)
			fmt.Fprintf(out, "  ✗ GitHub API [%s]: %s\n", ss.ScaleSetName, safeErr)
			failures = append(failures, fmt.Errorf("GitHub API [%s]: %w", ss.ScaleSetName, safeErr))
			continue
		}
		if probe.registered {
			fmt.Fprintf(out, "  ✓ GitHub API [%s]: reachable; scale set registered\n", ss.ScaleSetName)
		} else {
			fmt.Fprintf(out, "  ✓ GitHub API [%s]: reachable; scale set not registered yet\n", ss.ScaleSetName)
		}
		groups = addProbedRunnerGroup(groups, ss, probe)
	}
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := reportUnconfiguredScaleSets(ctx, out, group, scaleSets); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// probedRunnerGroup is a runner group doctor reached, through the first
// configured scale set it probed there.
type probedRunnerGroup struct {
	scaleSet config.ScaleSetConfig
	probe    scaleSetProbe
}

func addProbedRunnerGroup(groups []probedRunnerGroup, ss config.ScaleSetConfig, probe scaleSetProbe) []probedRunnerGroup {
	for _, g := range groups {
		if g.scaleSet.RegistrationURL == ss.RegistrationURL && g.probe.runnerGroupID == probe.runnerGroupID {
			return groups
		}
	}
	return append(groups, probedRunnerGroup{scaleSet: ss, probe: probe})
}

// maxUnconfiguredScaleSetsShown bounds the names doctor prints per runner
// group.
const maxUnconfiguredScaleSetsShown = 20

// reportUnconfiguredScaleSets prints the scale sets registered in group's
// runner group that no scale set configured for the same registration URL
// names. runner never deletes a scale set on its own, so one left behind by a
// rename or a removed [[scaleset]] keeps the jobs that target it queued.
// Another host may run the others, so finding some is not a failure. Names
// come from GitHub and are printed quoted.
func reportUnconfiguredScaleSets(ctx context.Context, out io.Writer, group probedRunnerGroup, scaleSets []config.ScaleSetConfig) error {
	name := group.scaleSet.ScaleSetName
	listCtx, cancel := context.WithTimeout(ctx, apiProbeTimeout)
	registered, err := group.probe.client.ListRunnerScaleSets(listCtx, group.probe.runnerGroupID)
	cancel()
	if err != nil {
		safeErr := safeAPIError(err)
		fmt.Fprintf(out, "  ✗ GitHub API [%s]: listing its runner group failed: %s\n", name, safeErr)
		return fmt.Errorf("GitHub API [%s]: listing its runner group: %w", name, safeErr)
	}
	var others []string
	for _, rs := range registered {
		if !configuredScaleSet(scaleSets, group.scaleSet.RegistrationURL, rs.Name) {
			others = append(others, rs.Name)
		}
	}
	if len(others) == 0 {
		return nil
	}
	slices.Sort(others)
	fmt.Fprintf(out, "  ⚠ GitHub API [%s]: its runner group has %d scale set(s) not in this config\n", name, len(others))
	for _, other := range others[:min(len(others), maxUnconfiguredScaleSetsShown)] {
		fmt.Fprintf(out, "      %q\n", other)
	}
	if extra := len(others) - maxUnconfiguredScaleSetsShown; extra > 0 {
		fmt.Fprintf(out, "      … and %d more\n", extra)
	}
	fmt.Fprintln(out, "    If no other host runs one, jobs that target it stay queued. Delete it with")
	fmt.Fprintln(out, "    'runner scaleset delete NAME' after adding it to the config.")
	return nil
}

// configuredScaleSet reports whether scaleSets configure name for
// registrationURL, in any runner group. Names compare without regard to case,
// so a configured scale set is never reported over a difference in case.
func configuredScaleSet(scaleSets []config.ScaleSetConfig, registrationURL, name string) bool {
	for _, ss := range scaleSets {
		if ss.RegistrationURL == registrationURL && strings.EqualFold(ss.ScaleSetName, name) {
			return true
		}
	}
	return false
}

// checkDocker checks for Docker daemon reachability, orphaned containers, and
// the shared volumes configured on socket.
func checkDocker(ctx context.Context, socket string, sharedVolumes []string, mode resourceCheck) (int, error) {
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
	orphans, err := checkDockerContainers(ctx, dockerClient, mode)
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

// checkDockerContainers reports runner's containers, as orphans or as the
// running runner's own, and with removeOrphans removes them, via
// findOrphanContainers and removeOrphanContainers.
func checkDockerContainers(ctx context.Context, client provider.RunnerCleanupAPI, mode resourceCheck) (int, error) {
	orphans, err := findOrphanContainers(ctx, client)
	if err != nil {
		fmt.Printf("  ✗ Failed to list Docker containers: %s\n", err)
		return 0, err
	}

	if len(orphans) == 0 {
		fmt.Println("  ✓ No orphaned Docker containers")
		return 0, nil
	}

	if mode != removeOrphans {
		issues := len(orphans)
		heading := fmt.Sprintf("  ⚠ Found %d orphaned Docker container(s)", issues)
		if mode == reportInUse {
			issues = 0
			heading = fmt.Sprintf("  ℹ %d Docker container(s) belong to the running runner", len(orphans))
		}
		fmt.Println(heading)
		for _, o := range orphans {
			fmt.Printf("      %s (%s)\n", o.Name, o.Status)
		}
		return issues, nil
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
func checkTart(ctx context.Context, mode resourceCheck, homes map[string]bool) (int, error) {
	if _, err := exec.LookPath("tart"); err != nil {
		fmt.Println("  - Tart: not installed (skipping)")
		return 0, nil
	}
	fmt.Println("  ✓ Tart binary found")

	var total int
	for home := range homes {
		issues, err := checkTartVMs(ctx, provider.TartCommandRunner(home), home, mode)
		if err != nil {
			return total, err
		}
		total += issues
	}
	return total, nil
}

// checkTartVMs reports the Tart VMs runner owns in one TART_HOME, as orphans
// or as the running runner's own, and with removeOrphans removes them,
// through the same ownership rules and removal path as startup
// reconciliation (findOrphanTartVMs, removeOrphanTartVMs).
func checkTartVMs(ctx context.Context, runner provider.CommandRunner, home string, mode resourceCheck) (int, error) {
	orphans, err := findOrphanTartVMs(ctx, runner)
	if err != nil {
		fmt.Printf("  ✗ Failed to list Tart VMs: %s\n", err)
		return 0, err
	}

	if len(orphans) == 0 {
		fmt.Printf("  ✓ No orphaned Tart VMs (TART_HOME=%q)\n", home)
		return 0, nil
	}

	if mode != removeOrphans {
		issues := len(orphans)
		heading := fmt.Sprintf("  ⚠ Found %d orphaned Tart VM(s)", issues)
		if mode == reportInUse {
			issues = 0
			heading = fmt.Sprintf("  ℹ %d Tart VM(s) belong to the running runner", len(orphans))
		}
		fmt.Println(heading)
		for _, vm := range orphans {
			fmt.Printf("      %s (%s)\n", vm.Name, vm.State)
		}
		return issues, nil
	}

	// Per-VM failures are logged (see removeOrphanTartVMs) rather than
	// printed, as for containers: the same removal path runs unattended
	// during startup reconciliation.
	removed := removeOrphanTartVMs(ctx, runner, orphans, slog.Default())
	fmt.Printf("  ✓ Removed %d orphaned Tart VM(s)\n", removed)
	return len(orphans) - removed, nil
}
