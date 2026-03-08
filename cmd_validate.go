package main

import (
	"context"
	"fmt"
	"time"

	dockerclient "github.com/docker/docker/client"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate configuration and connectivity",
	Long:  "Check that the config file is valid, Docker is reachable, and GitHub tokens work.",
	Example: `  runscaler validate --config config.toml
  runscaler validate --url https://github.com/org --name test --token ghp_xxx`,
	RunE: runValidate,
}

func init() {
	flags := validateCmd.Flags()
	flags.String("config", "", "Path to config file (TOML)")
}

func runValidate(cmd *cobra.Command, args []string) error {
	// Load config using same logic as root command
	if configFile, _ := cmd.Flags().GetString("config"); configFile != "" {
		viper.SetConfigFile(configFile)
		if err := viper.ReadInConfig(); err != nil {
			return fmt.Errorf("failed to read config file: %w", err)
		}
	}

	var c Config
	if err := viper.Unmarshal(&c); err != nil {
		return fmt.Errorf("failed to parse configuration: %w", err)
	}

	// Validate scale sets
	scaleSets := c.ResolveScaleSets()
	if len(scaleSets) == 0 {
		return fmt.Errorf("no scale sets configured")
	}

	for i := range scaleSets {
		if err := scaleSets[i].Validate(); err != nil {
			fmt.Printf("  ✗ scaleset[%d] %q: %s\n", i, scaleSets[i].ScaleSetName, err)
			return fmt.Errorf("validation failed")
		}
		fmt.Printf("  ✓ scaleset[%d] %q — url=%s max=%d min=%d\n",
			i, scaleSets[i].ScaleSetName, scaleSets[i].RegistrationURL,
			scaleSets[i].MaxRunners, scaleSets[i].MinRunners,
		)
	}

	// Test Docker connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dockerClient, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
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
	fmt.Printf("  ✓ Docker is reachable at %s\n", c.DockerSocket)

	// Show shared volume status
	if c.SharedVolume != "" {
		fmt.Printf("  ✓ Shared volume enabled at %s\n", c.SharedVolume)
	} else {
		fmt.Println("  - Shared volume: not configured (cross-job sharing will not work)")
	}

	// Test GitHub API connectivity for each scale set
	for i, ss := range scaleSets {
		logger := c.Logger()
		client, err := ss.ScalesetClient(logger)
		if err != nil {
			fmt.Printf("  ✗ scaleset[%d] %q GitHub API: %s\n", i, ss.ScaleSetName, err)
			return fmt.Errorf("validation failed")
		}
		// Try to resolve runner group as a connectivity test
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
