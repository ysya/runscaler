package main

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	dockerclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/config"
)

var validateCmd = &cobra.Command{
	Use:     "validate",
	Short:   "Validate configuration and connectivity",
	Long:    "Check that the config file is valid, Docker/Tart is reachable, and GitHub tokens work.",
	Example: `  runner validate --config config.toml`,
	RunE:    runValidate,
}

func runValidate(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}

	// Strict mode: what `runner run` merely warns about (unknown keys,
	// mixed single/multi mode) fails validation.
	if len(cfg.Warnings) > 0 {
		for _, w := range cfg.Warnings {
			fmt.Printf("  ✗ config: %s\n", w)
		}
		return fmt.Errorf("validation failed")
	}
	if err := cfg.ValidateGlobal(); err != nil {
		fmt.Printf("  ✗ global config: %s\n", err)
		return fmt.Errorf("validation failed")
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
	if err := validateScaleSetCollection(scaleSets); err != nil {
		fmt.Printf("  ✗ scale set collection: %s\n", err)
		return fmt.Errorf("validation failed")
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

	// Test every distinct Docker socket used by resolved scale sets.
	if needsDocker {
		for socket := range groupDockerScaleSets(scaleSets) {
			dockerClient, err := dockerclient.New(
				dockerclient.FromEnv,
				dockerclient.WithHost("unix://"+socket),
			)
			if err != nil {
				fmt.Printf("  ✗ Docker client for %s: %s\n", socket, err)
				return fmt.Errorf("validation failed")
			}
			if _, err := dockerClient.Ping(ctx, dockerclient.PingOptions{NegotiateAPIVersion: true}); err != nil {
				_ = dockerClient.Close()
				fmt.Printf("  ✗ Docker connectivity at %s: %s\n", socket, err)
				fmt.Println("\n  Possible fixes:")
				fmt.Println("  1. Ensure Docker is running")
				fmt.Println("  2. Add your user to the docker group: sudo usermod -aG docker $USER")
				fmt.Println("  3. Re-login or run: newgrp docker")
				return fmt.Errorf("validation failed")
			}
			_ = dockerClient.Close()
			fmt.Printf("  ✓ Docker is reachable at %s\n", socket)
		}
	}

	// Test Tart binary (only if needed)
	if needsTart {
		if _, err := exec.LookPath("tart"); err != nil {
			fmt.Println("  ✗ Tart binary not found in PATH")
			fmt.Println("\n  Install Tart: brew install cirruslabs/cli/tart")
			return fmt.Errorf("validation failed")
		}
		fmt.Println("  ✓ Tart binary found")

	}

	// Show shared volume status
	if needsDocker {
		for _, ss := range scaleSets {
			if !ss.IsTart() && ss.Docker.SharedVolume != "" {
				fmt.Printf("  ✓ scaleset %q shared volume enabled at %s\n", ss.ScaleSetName, ss.Docker.SharedVolume)
			}
		}
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
