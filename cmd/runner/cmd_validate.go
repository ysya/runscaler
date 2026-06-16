package main

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	dockerclient "github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/config"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate configuration and connectivity",
	Long:  "Check that the config file is valid, Docker/Tart is reachable, and GitHub tokens work.",
	Example: `  runscaler validate --config config.toml`,
	RunE: runValidate,
}

func runValidate(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}

	// Validate scale sets
	scaleSets := cfg.ResolveScaleSets()
	if len(scaleSets) == 0 {
		return fmt.Errorf("no scale sets configured")
	}

	for i := range scaleSets {
		if err := scaleSets[i].Validate(); err != nil {
			fmt.Printf("  ✗ scaleset[%d] %q: %s\n", i, scaleSets[i].ScaleSetName, err)
			return fmt.Errorf("validation failed")
		}
		fmt.Printf("  ✓ scaleset[%d] %q — backend=%s url=%s max=%d min=%d\n",
			i, scaleSets[i].ScaleSetName, scaleSets[i].Backend, scaleSets[i].RegistrationURL,
			scaleSets[i].MaxRunners, scaleSets[i].MinRunners,
		)
	}

	// Check which backends are needed
	needsDocker := false
	needsTart := false
	for _, ss := range scaleSets {
		if ss.IsTart() {
			needsTart = true
		} else {
			needsDocker = true
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test Docker connectivity (only if needed)
	if needsDocker {
		dockerClient, err := dockerclient.NewClientWithOpts(
			dockerclient.FromEnv,
			dockerclient.WithHost("unix://"+cfg.Defaults.Docker.Socket),
			dockerclient.WithAPIVersionNegotiation(),
		)
		if err != nil {
			fmt.Printf("  ✗ Docker client: %s\n", err)
			return fmt.Errorf("validation failed")
		}

		if _, err := dockerClient.Ping(ctx); err != nil {
			fmt.Printf("  ✗ Docker connectivity: %s\n", err)
			fmt.Println("\n  Possible fixes:")
			fmt.Println("  1. Ensure Docker is running")
			fmt.Println("  2. Add your user to the docker group: sudo usermod -aG docker $USER")
			fmt.Println("  3. Re-login or run: newgrp docker")
			return fmt.Errorf("validation failed")
		}
		fmt.Printf("  ✓ Docker is reachable at %s\n", cfg.Defaults.Docker.Socket)
	}

	// Test Tart binary (only if needed)
	if needsTart {
		if _, err := exec.LookPath("tart"); err != nil {
			fmt.Println("  ✗ Tart binary not found in PATH")
			fmt.Println("\n  Install Tart: brew install cirruslabs/cli/tart")
			return fmt.Errorf("validation failed")
		}
		fmt.Println("  ✓ Tart binary found")

		for _, ss := range scaleSets {
			if ss.IsTart() && ss.MaxRunners > 2 {
				fmt.Printf("  ⚠ scaleset %q: max-runners=%d exceeds macOS 2-VM-per-host limit\n",
					ss.ScaleSetName, ss.MaxRunners)
			}
		}
	}

	// Show shared volume status
	if cfg.Defaults.Docker.SharedVolume != "" {
		fmt.Printf("  ✓ Shared volume enabled at %s\n", cfg.Defaults.Docker.SharedVolume)
	} else if needsDocker {
		fmt.Println("  - Shared volume: not configured (cross-job sharing will not work)")
	}

	// Test GitHub API connectivity for each scale set
	logger := config.NewLogger(cfg.LogLevel, cfg.LogFormat)
	for i, ss := range scaleSets {
		client, err := config.NewScalesetClient(ss.RegistrationURL, ss.Token, logger)
		if err != nil {
			fmt.Printf("  ✗ scaleset[%d] %q GitHub API: %s\n", i, ss.ScaleSetName, err)
			return fmt.Errorf("validation failed")
		}
		_, err = client.GetRunnerGroupByName(ctx, "default")
		if err != nil {
			fmt.Printf("  ✗ scaleset[%d] %q GitHub API: %s\n", i, ss.ScaleSetName, err)
			fmt.Println("    Check that your token has the correct scopes (admin:org or repo)")
			return fmt.Errorf("validation failed")
		}
		fmt.Printf("  ✓ scaleset[%d] %q GitHub API is reachable\n", i, ss.ScaleSetName)
	}

	fmt.Println("\nAll checks passed.")
	return nil
}
