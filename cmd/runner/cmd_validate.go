package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/actions/scaleset"
	dockerclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/provider"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate configuration and connectivity",
	Long:  "Check that the config file is valid, Docker/Tart is reachable, and GitHub credentials work.",
	Example: `  runner validate --config config.toml
  runner validate --config config.toml --preflight`,
	RunE: runValidate,
}

func init() {
	validateCmd.Flags().Bool("preflight", false, "Pull Docker runner images and ensure Tart images are available")
}

func runValidate(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	if configMissing(cfg) {
		return missingConfigError()
	}
	scaleSets, err := validateLocalConfig(cfg, cmd.OutOrStdout())
	if err != nil {
		return err
	}

	// Check which instance providers are needed.
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
	dockerClients := make(map[string]*dockerclient.Client)
	defer func() {
		for _, client := range dockerClients {
			_ = client.Close()
		}
	}()

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
			ping, err := dockerClient.Ping(ctx, dockerclient.PingOptions{NegotiateAPIVersion: true})
			if err != nil {
				_ = dockerClient.Close()
				fmt.Printf("  ✗ Docker connectivity at %s: %s\n", socket, err)
				fmt.Println("\n  Possible fixes:")
				fmt.Println("  1. Ensure Docker is running")
				fmt.Println("  2. Add your user to the docker group: sudo usermod -aG docker $USER")
				fmt.Println("  3. Re-login or run: newgrp docker")
				return fmt.Errorf("validation failed")
			}
			if err := requireDockerAPIVersion(ping.APIVersion, dockerClient.ClientVersion()); err != nil {
				_ = dockerClient.Close()
				fmt.Printf("  ✗ Docker at %s: %s\n", socket, err)
				return fmt.Errorf("validation failed")
			}
			dockerClients[socket] = dockerClient
			fmt.Printf("  ✓ Docker is reachable at %s\n", socket)
		}
	}

	// Test Tart binary (only if needed)
	if needsTart {
		if _, err := exec.LookPath("tart"); err != nil {
			fmt.Println("  ✗ Tart binary not found in PATH")
			fmt.Println("\n  Install Tart: brew install openai/tools/tart")
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

	if err := validateGitHubAccess(commandContext(cmd), os.Stdout, scaleSets, newScaleSetClient); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(cmd.OutOrStdout(), nil))
	if err := runOptionalImagePreflight(cmd, scaleSets,
		func(ctx context.Context, ss config.ScaleSetConfig) error {
			return pullDockerRunnerImage(ctx, ss, dockerClients, cmd.OutOrStdout(), logger)
		},
		func(ctx context.Context, ss config.ScaleSetConfig) error {
			return ensureTartRunnerImage(ctx, ss, logger)
		}); err != nil {
		return err
	}

	runtimes := newScaleSetRuntimes(scaleSets, runtimeBackends{dockerClients: dockerClients})
	present := func(ctx context.Context, home, image string) (bool, error) {
		return provider.TartImagePresent(ctx, provider.TartCommandRunner(home), image)
	}
	detect := func(ctx context.Context, home, image string) (provider.TartGuest, error) {
		return provider.DetectTartGuest(ctx, provider.TartCommandRunner(home), home, image)
	}
	if err := reportTartGuests(commandContext(cmd), cmd.OutOrStdout(), runtimes, present, detect, logger); err != nil {
		return err
	}

	fmt.Println("\nAll checks passed.")
	return nil
}

func validateLocalConfig(cfg config.Config, out io.Writer) ([]config.ScaleSetConfig, error) {
	problemCount := 0

	// Strict mode: what `runner run` merely warns about (unknown keys,
	// mixed single/multi mode) fails validation.
	for _, w := range cfg.Warnings {
		fmt.Fprintf(out, "  ✗ config: %s\n", w)
		problemCount++
	}
	for _, d := range cfg.Deprecations {
		fmt.Fprintf(out, "  ⚠ config: %s\n", d)
	}
	for _, err := range cfg.GlobalValidationErrors() {
		fmt.Fprintf(out, "  ✗ global config: %s\n", err)
		problemCount++
	}

	scaleSets := cfg.ResolveScaleSets()
	if len(scaleSets) == 0 {
		fmt.Fprintln(out, "  ✗ no scale sets configured")
		problemCount++
	}
	// The same offline checks `runner run` applies before it starts, so a
	// config that validates also starts.
	for _, err := range scaleSetProblems(cfg, scaleSets) {
		fmt.Fprintf(out, "  ✗ %s\n", err)
		problemCount++
	}
	if problemCount > 0 {
		return nil, fmt.Errorf("validation failed")
	}
	for i, ss := range scaleSets {
		fmt.Fprintf(out, "  ✓ scaleset[%d] %q — provider=%s url=%s max=%d min=%d\n",
			i, ss.ScaleSetName, ss.Provider, ss.RegistrationURL, ss.MaxRunners, ss.MinRunners)
	}
	return scaleSets, nil
}

// validateGitHubAccess probes each scale set through the same read-only
// probe as `runner doctor`: its configured runner group, a bounded timeout,
// and only safe error categories in the output. Unlike doctor it stops at
// the first failure.
func validateGitHubAccess(ctx context.Context, out io.Writer, scaleSets []config.ScaleSetConfig, newClient func(config.ScaleSetConfig) (scaleSetAPI, error)) error {
	for i, ss := range scaleSets {
		probeCtx, cancel := context.WithTimeout(ctx, apiProbeTimeout)
		probe, err := probeScaleSet(probeCtx, ss, newClient)
		cancel()
		if err != nil {
			fmt.Fprintf(out, "  ✗ scaleset[%d] %q GitHub API: %s\n", i, ss.ScaleSetName, safeAPIError(err))
			var rateLimited *scaleset.RateLimitError
			switch {
			case errors.As(err, &rateLimited):
				fmt.Fprintln(out, "    GitHub is rate-limiting these credentials; wait for the limit to reset, then validate again")
			case ss.GitHubApp != nil:
				fmt.Fprintln(out, "    Check the GitHub App installation, private key, and runner permissions")
			default:
				fmt.Fprintln(out, "    Check that your token has the correct scopes (admin:org or repo)")
			}
			return fmt.Errorf("validation failed")
		}
		state := "not registered yet"
		if probe.registered {
			state = "registered"
		}
		fmt.Fprintf(out, "  ✓ scaleset[%d] %q GitHub API is reachable; scale set %s\n", i, ss.ScaleSetName, state)
	}
	return nil
}
