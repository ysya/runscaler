package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/config"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose and clean up orphaned runners",
	Long: `Check for orphaned Docker containers, Tart VMs, and volumes
left by runscaler after abnormal termination.

By default, only reports what it finds. Use --fix to remove orphaned resources.`,
	Example: `  runscaler doctor          # Diagnose only
  runscaler doctor --fix    # Diagnose and clean up`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().Bool("fix", false, "Remove orphaned resources")
	doctorCmd.Flags().Int("health-port", config.DefaultHealthPort, "Health check port to detect running instance")
}

// runnerNamePattern matches container/VM names created by runscaler.
// scaler.go generates: runner-{uuid[:8]} where uuid[:8] is 8 hex chars.
var runnerNamePattern = regexp.MustCompile(`^/?runner-[0-9a-f]{8}$`)

func runDoctor(cmd *cobra.Command, args []string) error {
	fix, _ := cmd.Flags().GetBool("fix")
	healthPort, _ := cmd.Flags().GetInt("health-port")

	// Try to load config for docker socket path and scaleset info
	dockerSocket := config.DefaultDockerSocket
	cfg, cfgErr := loadConfig(cmd)
	if cfgErr == nil {
		dockerSocket = cfg.Defaults.Docker.Socket
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Safety check: refuse --fix if runscaler is currently running
	if fix {
		if isRunscalerRunning(healthPort) {
			fmt.Println("  ✗ runscaler is currently running (health endpoint responded)")
			fmt.Println("    Stop runscaler first before using --fix")
			return fmt.Errorf("cannot fix while runscaler is running")
		}
	}

	// Scaleset API connectivity check (if config is available)
	if cfgErr == nil {
		checkScalesetAPI(ctx, &cfg)
	}

	var totalOrphans int

	// Docker checks
	orphans, err := checkDocker(ctx, dockerSocket, fix)
	if err != nil {
		return err
	}
	totalOrphans += orphans

	// Tart checks
	orphans, err = checkTart(ctx, fix)
	if err != nil {
		return err
	}
	totalOrphans += orphans

	// Summary
	fmt.Println()
	if totalOrphans == 0 {
		fmt.Println("All clean. No orphaned resources found.")
	} else if fix {
		fmt.Println("All clean.")
	} else {
		fmt.Printf("Found %d orphaned resource(s). Run 'runscaler doctor --fix' to clean up.\n", totalOrphans)
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

// isRunscalerRunning checks if a runscaler instance is responding on the health port.
func isRunscalerRunning(port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/readyz", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// checkDocker checks for Docker daemon reachability, orphaned containers, and volumes.
func checkDocker(ctx context.Context, socket string, fix bool) (int, error) {
	dockerClient, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithHost("unix://"+socket),
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		fmt.Println("  - Docker: not available (skipping)")
		return 0, nil
	}

	if _, err := dockerClient.Ping(ctx); err != nil {
		fmt.Println("  - Docker: not reachable (skipping)")
		return 0, nil
	}
	fmt.Println("  ✓ Docker daemon is reachable")

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

// checkDockerContainers finds and optionally removes orphaned runner containers.
func checkDockerContainers(ctx context.Context, client *dockerclient.Client, fix bool) (int, error) {
	containers, err := client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		fmt.Printf("  ✗ Failed to list Docker containers: %s\n", err)
		return 0, err
	}

	var orphans []container.Summary
	for _, c := range containers {
		if c.Labels["managed-by"] == "runscaler" {
			orphans = append(orphans, c)
			continue
		}
		for _, name := range c.Names {
			if runnerNamePattern.MatchString(name) {
				orphans = append(orphans, c)
				break
			}
		}
	}

	if len(orphans) == 0 {
		fmt.Println("  ✓ No orphaned Docker containers")
		return 0, nil
	}

	if !fix {
		fmt.Printf("  ⚠ Found %d orphaned Docker container(s)\n", len(orphans))
		for _, c := range orphans {
			fmt.Printf("      %s (%s)\n", containerDisplayName(c), c.State)
		}
		return len(orphans), nil
	}

	// Fix: remove orphaned containers
	removed := 0
	for _, c := range orphans {
		name := containerDisplayName(c)
		if err := client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			fmt.Printf("  ✗ Failed to remove container %s: %s\n", name, err)
		} else {
			removed++
		}
	}
	fmt.Printf("  ✓ Removed %d orphaned Docker container(s)\n", removed)
	return 0, nil
}

// checkDockerVolume checks for the runscaler-shared volume.
func checkDockerVolume(ctx context.Context, client *dockerclient.Client, fix bool) (int, error) {
	_, err := client.VolumeInspect(ctx, "runscaler-shared")
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			fmt.Println("  ✓ No orphaned Docker volumes")
			return 0, nil
		}
		// Treat unknown errors as "no volume" rather than failing
		fmt.Println("  ✓ No orphaned Docker volumes")
		return 0, nil
	}

	if !fix {
		fmt.Println("  ⚠ Found orphaned volume: runscaler-shared")
		return 1, nil
	}

	if err := client.VolumeRemove(ctx, "runscaler-shared", true); err != nil {
		fmt.Printf("  ✗ Failed to remove volume runscaler-shared: %s\n", err)
		return 1, nil
	}
	fmt.Println("  ✓ Removed orphaned volume: runscaler-shared")
	return 0, nil
}

// tartListEntry represents a VM from `tart list --format json`.
type tartListEntry struct {
	Name   string `json:"Name"`
	State  string `json:"State"`
	Source string `json:"Source"`
}

// checkTart checks for Tart binary and orphaned VMs.
func checkTart(ctx context.Context, fix bool) (int, error) {
	if _, err := exec.LookPath("tart"); err != nil {
		fmt.Println("  - Tart: not installed (skipping)")
		return 0, nil
	}
	fmt.Println("  ✓ Tart binary found")

	return checkTartVMs(ctx, fix)
}

// checkTartVMs finds and optionally removes orphaned Tart VMs.
func checkTartVMs(ctx context.Context, fix bool) (int, error) {
	out, err := exec.CommandContext(ctx, "tart", "list", "--format", "json").Output()
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
		fmt.Println("  ✓ No orphaned Tart VMs")
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
		_ = exec.CommandContext(ctx, "tart", "stop", vm.Name).Run()
		if err := exec.CommandContext(ctx, "tart", "delete", vm.Name).Run(); err != nil {
			fmt.Printf("  ✗ Failed to delete VM %s: %s\n", vm.Name, err)
		} else {
			removed++
		}
	}
	fmt.Printf("  ✓ Removed %d orphaned Tart VM(s)\n", removed)
	return 0, nil
}
