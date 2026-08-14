package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/backend"
	"github.com/ysya/runscaler/internal/config"
	runnerlock "github.com/ysya/runscaler/internal/lock"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose and clean up orphaned runners",
	Long: `Check for orphaned Docker containers, Tart VMs, and volumes
left by runner after abnormal termination.

By default, only reports what it finds. Use --fix to remove orphaned resources.`,
	Example: `  runner doctor          # Diagnose only
  runner doctor --fix    # Diagnose and clean up`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().Bool("fix", false, "Remove orphaned resources")
	doctorCmd.Flags().Int("health-port", config.DefaultHealthPort, "Health check port to detect running instance")
	doctorCmd.Flags().String("health-address", config.DefaultHealthAddress, "Health check address to detect older runner instances")
}

// runnerNamePattern matches container/VM names created by runner.
// scaler.go generates: runner-{uuid[:8]} where uuid[:8] is 8 hex chars.
var runnerNamePattern = regexp.MustCompile(`^/?runner-[0-9a-f]{8}$`)

func runDoctor(cmd *cobra.Command, args []string) error {
	fix, _ := cmd.Flags().GetBool("fix")
	healthPort, _ := cmd.Flags().GetInt("health-port")
	healthAddress, _ := cmd.Flags().GetString("health-address")

	// Include both legacy/default and configured backend locations.
	dockerSockets := map[string]bool{config.DefaultDockerSocket: true}
	tartHomes := map[string]bool{"": true}
	cfg, cfgErr := loadConfig(cmd)
	if cfgErr != nil {
		return fmt.Errorf("cannot load config for safe diagnosis: %w", cfgErr)
	}
	if !cmd.Flags().Changed("health-port") {
		healthPort = cfg.HealthPort
	}
	if !cmd.Flags().Changed("health-address") {
		healthAddress = cfg.HealthAddress
	}
	for _, ss := range cfg.ResolveScaleSets() {
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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

		// Keep the health probe as defense in depth for older runner versions
		// that predate the machine lock.
		if isRunnerRunning(healthAddress, healthPort) {
			fmt.Println("  ✗ runner is currently running (health endpoint responded)")
			fmt.Println("    Stop runner first before using --fix")
			return fmt.Errorf("cannot fix while runner is running")
		}
	}

	// Scaleset API connectivity check (if config is available)
	checkScalesetAPI(ctx, &cfg)

	var totalOrphans int

	// Docker checks
	for socket := range dockerSockets {
		orphans, err := checkDocker(ctx, socket, fix)
		if err != nil {
			return err
		}
		totalOrphans += orphans
	}

	// Tart checks
	orphans, err := checkTart(ctx, fix, tartHomes)
	if err != nil {
		return err
	}
	totalOrphans += orphans

	// Summary
	fmt.Println()
	if totalOrphans == 0 {
		fmt.Println("All clean. No orphaned resources found.")
	} else if fix {
		fmt.Printf("Cleanup incomplete: %d orphaned resource(s) remain.\n", totalOrphans)
		return fmt.Errorf("orphaned resources remain after cleanup")
	} else {
		fmt.Printf("Found %d orphaned resource(s). Run 'runner doctor --fix' to clean up.\n", totalOrphans)
		return fmt.Errorf("orphaned resources found")
	}
	return nil
}

// checkScalesetAPI tests scaleset API connectivity and prints debug info.
func checkScalesetAPI(ctx context.Context, cfg *config.Config) {
	scaleSets := cfg.ResolveScaleSets()
	if len(scaleSets) == 0 {
		return
	}

	// Use the first scale set with a token for the connectivity check
	var ss config.ScaleSetConfig
	for _, s := range scaleSets {
		if s.Token != "" && s.RegistrationURL != "" {
			ss = s
			break
		}
	}
	if ss.Token == "" {
		fmt.Println("  - Scaleset API: no token configured (skipping)")
		return
	}

	client, err := config.NewScalesetClient(ss.RegistrationURL, ss.Token, nil)
	if err != nil {
		fmt.Printf("  ✗ Scaleset API: failed to create client: %s\n", err)
		return
	}

	fmt.Println("  ✓ Scaleset API client created")
	fmt.Printf("    Debug: %s\n", client.DebugInfo())
}

// isRunnerRunning checks if a runner instance is responding on the health port.
func isRunnerRunning(address string, port int) bool {
	if port <= 0 {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/readyz", net.JoinHostPort(address, fmt.Sprintf("%d", port))))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// checkDocker checks for Docker daemon reachability, orphaned containers, and volumes.
func checkDocker(ctx context.Context, socket string, fix bool) (int, error) {
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

	// Check orphaned volume
	orphans, err = checkDockerVolume(ctx, dockerClient, fix)
	if err != nil {
		return totalOrphans, err
	}
	totalOrphans += orphans

	return totalOrphans, nil
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

// checkDockerContainers finds and optionally removes orphaned runner
// containers, via findOrphanContainers and removeOrphanContainers.
func checkDockerContainers(ctx context.Context, client backend.DockerAPI, fix bool) (int, error) {
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

// sharedVolumeNames lists the shared volume names doctor looks for: the
// current name plus legacy names from before the runscaler→runner rename.
var sharedVolumeNames = []string{"runner-shared", "runscaler-shared"}

// checkDockerVolume reports whether runner's shared handoff volume(s)
// exist and, for each one found, prints its name and size. It never
// removes anything, regardless of fix: fix is accepted only so this
// function's signature matches checkDockerContainers' and the shared call
// site in checkDocker can pass the same variable to both — the value is
// not read below, and volumeAPI does not even expose a VolumeRemove method
// to call. Returns the number of unresolved issues, which this check never
// contributes to (a real Docker failure is instead returned as an error).
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
func checkDockerVolume(ctx context.Context, client volumeAPI, fix bool) (int, error) {
	found := 0
	for _, name := range sharedVolumeNames {
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
	return backend.FormatBytes(uint64(usage.Size))
}

// tartListEntry represents a VM from `tart list --format json`.
type tartListEntry struct {
	Name   string `json:"Name"`
	State  string `json:"State"`
	Source string `json:"Source"`
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
		issues, err := checkTartVMs(ctx, fix, home)
		if err != nil {
			return total, err
		}
		total += issues
	}
	return total, nil
}

// checkTartVMs finds and optionally removes orphaned Tart VMs.
func checkTartVMs(ctx context.Context, fix bool, home string) (int, error) {
	out, err := tartDoctorCommand(ctx, home, "list", "--format", "json").Output()
	if err != nil {
		fmt.Printf("  ✗ Failed to list Tart VMs: %s\n", err)
		return 0, err
	}

	var vms []tartListEntry
	if err := json.Unmarshal(out, &vms); err != nil {
		fmt.Printf("  ✗ Failed to parse Tart VM list: %s\n", err)
		return 0, err
	}

	var orphans []tartListEntry
	for _, vm := range vms {
		if strings.HasPrefix(vm.Name, "runner-") || strings.HasPrefix(vm.Name, "pool-") {
			orphans = append(orphans, vm)
		}
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

	// Fix: stop and delete orphaned VMs
	removed := 0
	for _, vm := range orphans {
		// Stop first (ignore error — may already be stopped)
		_ = tartDoctorCommand(ctx, home, "stop", vm.Name).Run()
		if err := tartDoctorCommand(ctx, home, "delete", vm.Name).Run(); err != nil {
			fmt.Printf("  ✗ Failed to delete VM %s: %s\n", vm.Name, err)
		} else {
			removed++
		}
	}
	fmt.Printf("  ✓ Removed %d orphaned Tart VM(s)\n", removed)
	return len(orphans) - removed, nil
}

func tartDoctorCommand(ctx context.Context, home string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "tart", args...)
	if home != "" {
		c.Env = append(os.Environ(), "TART_HOME="+home)
	}
	return c
}
