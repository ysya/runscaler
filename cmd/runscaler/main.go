package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/backend"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/health"
	"github.com/ysya/runscaler/internal/scaler"
)

var (
	version = "dev"
	commit  = ""
	date    = ""
)

func init() {
	// Persistent flags — available to all subcommands
	cmd.PersistentFlags().String("config", "", "Path to config file (TOML)")

	flags := cmd.Flags()

	// Per-scaleset (also used as legacy single mode)
	flags.String("url", "", "Registration URL (e.g. https://github.com/org)")
	flags.String("name", "", "Name of the scale set (also used as runs-on label)")
	flags.String("token", "", "Personal access token")
	flags.Int("max-runners", config.DefaultMaxRunners, "Maximum number of runners")
	flags.Int("min-runners", 0, "Minimum number of runners")
	flags.StringSlice("labels", nil, "Runner labels (comma-separated)")
	flags.String("runner-group", config.DefaultRunnerGroup, "Runner group name")
	flags.String("runner-image", config.DefaultRunnerImage, "Docker image for runners")
	flags.String("backend", config.DefaultBackend, "Runner backend (docker or tart)")

	// Docker backend
	flags.String("docker-socket", config.DefaultDockerSocket, "Path to Docker socket")
	flags.Bool("dind", config.DefaultDinD, "Mount Docker socket into runner containers (Docker-in-Docker)")
	flags.String("shared-volume", "", "Shared Docker volume mounted into all runners (container path, e.g. /shared)")

	// Tart backend
	flags.String("tart-image", "", "Base Tart VM image for macOS runners")
	flags.String("tart-runner-dir", "", "Runner binary path inside VM")
	flags.Int("tart-cpu", 0, "Number of CPU cores for each VM (0 = use image default)")
	flags.Int("tart-memory", 0, "Memory in MB for each VM (0 = use image default)")
	flags.Int("tart-pool-size", 0, "Number of pre-warmed VMs to keep ready (0 = disabled)")

	// Global
	flags.String("log-level", config.DefaultLogLevel, "Log level (debug, info, warn, error)")
	flags.String("log-format", config.DefaultLogFormat, "Log format (text, json)")

	// Operational
	flags.Bool("dry-run", false, "Validate everything without starting listeners")
	flags.Int("health-port", config.DefaultHealthPort, "Health check HTTP port (0 to disable)")

	// Bind flags to viper keys explicitly.
	// Flat keys (flag name == viper key):
	viper.BindPFlag("url", flags.Lookup("url"))
	viper.BindPFlag("name", flags.Lookup("name"))
	viper.BindPFlag("token", flags.Lookup("token"))
	viper.BindPFlag("max-runners", flags.Lookup("max-runners"))
	viper.BindPFlag("min-runners", flags.Lookup("min-runners"))
	viper.BindPFlag("labels", flags.Lookup("labels"))
	viper.BindPFlag("runner-group", flags.Lookup("runner-group"))
	viper.BindPFlag("runner-image", flags.Lookup("runner-image"))
	viper.BindPFlag("backend", flags.Lookup("backend"))
	viper.BindPFlag("log-level", flags.Lookup("log-level"))
	viper.BindPFlag("log-format", flags.Lookup("log-format"))
	viper.BindPFlag("dry-run", flags.Lookup("dry-run"))
	viper.BindPFlag("health-port", flags.Lookup("health-port"))

	// Nested keys (flag name → nested viper key for backend sub-structs):
	viper.BindPFlag("docker.socket", flags.Lookup("docker-socket"))
	viper.BindPFlag("docker.dind", flags.Lookup("dind"))
	viper.BindPFlag("docker.shared-volume", flags.Lookup("shared-volume"))
	viper.BindPFlag("tart.image", flags.Lookup("tart-image"))
	viper.BindPFlag("tart.runner-dir", flags.Lookup("tart-runner-dir"))
	viper.BindPFlag("tart.cpu", flags.Lookup("tart-cpu"))
	viper.BindPFlag("tart.memory", flags.Lookup("tart-memory"))
	viper.BindPFlag("tart.pool-size", flags.Lookup("tart-pool-size"))

	// Register subcommands
	cmd.AddCommand(initCmd, validateCmd, statusCmd)
}

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var cmd = &cobra.Command{
	Use:     "runscaler [flags]",
	Version: version,
	Short:   "GitHub Actions Runner Auto-Scaler for Docker",
	Long: `Dynamically scales GitHub Actions self-hosted runners as Docker containers
using the actions/scaleset library. Runners are ephemeral — each container
handles one job and is removed upon completion.

Supports multiple scale sets via [[scaleset]] entries in TOML config,
or a single scale set via CLI flags.`,
	Example: `  # Quick start
  runscaler init                            # Generate config.toml interactively
  runscaler validate --config config.toml   # Verify configuration
  runscaler --config config.toml            # Start scaling

  # Using CLI flags
  runscaler --url https://github.com/org --name my-runners --token ghp_xxx

  # Dry run (validate everything without starting listeners)
  runscaler --dry-run --config config.toml`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(cmd)
		if err != nil {
			return err
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		// Force exit on second signal
		go func() {
			<-ctx.Done()
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			<-sig
			fmt.Fprintln(os.Stderr, "\nForce exit")
			os.Exit(1)
		}()

		return run(ctx, cfg)
	},
}

func run(ctx context.Context, cfg config.Config) error {
	logger := config.NewLogger(cfg.LogLevel, cfg.LogFormat)

	scaleSets := cfg.ResolveScaleSets()
	for i := range scaleSets {
		if err := scaleSets[i].Validate(); err != nil {
			return fmt.Errorf("scaleset[%d] %q: %w", i, scaleSets[i].ScaleSetName, err)
		}
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

	// Create shared Docker client if any scaleset uses Docker backend
	var dockerClient *dockerclient.Client
	if needsDocker {
		var err error
		dockerClient, err = dockerclient.NewClientWithOpts(
			dockerclient.FromEnv,
			dockerclient.WithAPIVersionNegotiation(),
		)
		if err != nil {
			return fmt.Errorf("failed to create docker client: %w", err)
		}

		if _, err := dockerClient.Ping(ctx); err != nil {
			return fmt.Errorf("cannot connect to Docker at %s: %w\n\n"+
				"  Possible fixes:\n"+
				"  1. Ensure Docker is running\n"+
				"  2. Add your user to the docker group: sudo usermod -aG docker $USER\n"+
				"  3. Re-login or run: newgrp docker\n"+
				"  4. Or check the docker socket path in your config",
				cfg.Defaults.Docker.Socket, err)
		}

		// Pull unique runner images for Docker scalesets
		pulled := make(map[string]bool)
		for _, ss := range scaleSets {
			if ss.IsTart() || pulled[ss.RunnerImage] {
				continue
			}
			logger.Info("Pulling runner image", slog.String("image", ss.RunnerImage))
			pull, err := dockerClient.ImagePull(ctx, ss.RunnerImage, image.PullOptions{})
			if err != nil {
				return fmt.Errorf("failed to pull runner image %s: %w", ss.RunnerImage, err)
			}
			if _, err := io.ReadAll(pull); err != nil {
				return fmt.Errorf("failed to read image pull response: %w", err)
			}
			pull.Close()
			pulled[ss.RunnerImage] = true
		}
	}

	// Verify Tart binary exists if any scaleset uses Tart backend
	if needsTart {
		if _, err := exec.LookPath("tart"); err != nil {
			return fmt.Errorf("tart binary not found in PATH: %w\n\n"+
				"  Install Tart: brew install cirruslabs/cli/tart", err)
		}
		logger.Info("Tart backend enabled")

		// Ensure Tart images are available locally (auto-pull if missing)
		pulled := make(map[string]bool)
		for _, ss := range scaleSets {
			if !ss.IsTart() || pulled[ss.Tart.Image] {
				continue
			}
			tb := backend.NewTartBackend(ss, logger)
			if err := tb.EnsureImage(ctx); err != nil {
				return err
			}
			pulled[ss.Tart.Image] = true
		}

		// Warn about the 2-VM macOS limit
		for _, ss := range scaleSets {
			if ss.IsTart() && ss.MaxRunners > 2 {
				logger.Warn("macOS VMs are limited to 2 concurrent per host by Apple",
					slog.String("scaleset", ss.ScaleSetName),
					slog.Int("maxRunners", ss.MaxRunners),
				)
			}
		}
	}

	if cfg.DryRun {
		logger.Info("Dry run complete — configuration and connectivity are valid",
			slog.Int("scaleSetCount", len(scaleSets)),
		)
		return nil
	}

	// Start health check server
	var healthServer *health.HealthServer
	if cfg.HealthPort > 0 {
		healthServer = health.NewHealthServer(cfg.HealthPort, version)
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.HealthPort))
		if err != nil {
			return fmt.Errorf("failed to start health server on port %d: %w", cfg.HealthPort, err)
		}
		go func() {
			if err := healthServer.Serve(ln); err != nil && err != http.ErrServerClosed {
				logger.Error("Health server error", slog.String("error", err.Error()))
			}
		}()
		defer healthServer.Shutdown(context.WithoutCancel(ctx))
		logger.Info("Health check server started", slog.Int("port", cfg.HealthPort))
	}

	logger.Info("Starting scale sets", slog.Int("count", len(scaleSets)))

	// Run each scale set in its own goroutine
	var wg sync.WaitGroup
	errs := make(chan error, len(scaleSets))

	for i := range scaleSets {
		wg.Add(1)
		go func(ss config.ScaleSetConfig) {
			defer wg.Done()
			ssLogger := logger.WithGroup("[" + ss.ScaleSetName + "]")
			if err := runScaleSet(ctx, ss, dockerClient, ssLogger, healthServer); err != nil {
				errs <- fmt.Errorf("scaleset %q: %w", ss.ScaleSetName, err)
			}
		}(scaleSets[i])
	}

	wg.Wait()
	close(errs)

	// Collect errors
	var errsSlice []error
	for err := range errs {
		errsSlice = append(errsSlice, err)
	}
	if len(errsSlice) > 0 {
		return errors.Join(errsSlice...)
	}
	return nil
}

// runScaleSet manages the lifecycle of a single scale set.
func runScaleSet(ctx context.Context, ss config.ScaleSetConfig, dockerClient *dockerclient.Client, logger *slog.Logger, h *health.HealthServer) error {
	// Create scaleset client
	scalesetClient, err := config.NewScalesetClient(ss.RegistrationURL, ss.Token, logger)
	if err != nil {
		return fmt.Errorf("failed to create scaleset client: %w", err)
	}

	// Resolve runner group ID
	var runnerGroupID int
	switch ss.RunnerGroup {
	case scaleset.DefaultRunnerGroup, "":
		runnerGroupID = 1
	default:
		runnerGroup, err := scalesetClient.GetRunnerGroupByName(ctx, ss.RunnerGroup)
		if err != nil {
			return fmt.Errorf("failed to get runner group: %w", err)
		}
		runnerGroupID = runnerGroup.ID
	}

	// Get or create runner scale set
	desired := &scaleset.RunnerScaleSet{
		Name:          ss.ScaleSetName,
		RunnerGroupID: runnerGroupID,
		Labels:        config.BuildLabels(ss.ScaleSetName, ss.Labels),
		RunnerSetting: scaleset.RunnerSetting{
			DisableUpdate: true,
		},
	}

	scaleSet, err := scalesetClient.GetRunnerScaleSet(ctx, runnerGroupID, ss.ScaleSetName)
	if err != nil || scaleSet == nil {
		scaleSet, err = scalesetClient.CreateRunnerScaleSet(ctx, desired)
		if err != nil {
			return fmt.Errorf("failed to create runner scale set: %w", err)
		}
		logger.Info("Scale set created",
			slog.Int("scaleSetID", scaleSet.ID),
			slog.String("name", scaleSet.Name),
		)
	} else {
		scaleSet, err = scalesetClient.UpdateRunnerScaleSet(ctx, scaleSet.ID, desired)
		if err != nil {
			return fmt.Errorf("failed to update runner scale set: %w", err)
		}
		logger.Info("Scale set reused",
			slog.Int("scaleSetID", scaleSet.ID),
			slog.String("name", scaleSet.Name),
		)
	}

	// Set user agent info
	scalesetClient.SetSystemInfo(config.NewSystemInfo(scaleSet.ID, version))

	// Delete scale set on exit
	defer func() {
		logger.Info("Deleting runner scale set", slog.Int("scaleSetID", scaleSet.ID))
		if err := scalesetClient.DeleteRunnerScaleSet(context.WithoutCancel(ctx), scaleSet.ID); err != nil {
			logger.Error("Failed to delete runner scale set", slog.String("error", err.Error()))
		}
	}()

	// Create message session
	hostname, err := os.Hostname()
	if err != nil {
		hostname = uuid.NewString()
	}
	sessionID := fmt.Sprintf("%s-%s", hostname, ss.ScaleSetName)

	sessionClient, err := scalesetClient.MessageSessionClient(ctx, scaleSet.ID, sessionID)
	if err != nil {
		return fmt.Errorf("failed to create message session: %w", err)
	}
	defer sessionClient.Close(context.Background())

	// Create listener
	l, err := listener.New(sessionClient, listener.Config{
		ScaleSetID: scaleSet.ID,
		MaxRunners: ss.MaxRunners,
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}

	// Create backend based on config
	var b backend.RunnerBackend
	if ss.IsTart() {
		tb := backend.NewTartBackend(ss, logger)
		tb.StartPool(ctx) // starts warm pool if tart-pool-size > 0
		b = tb
	} else {
		b = backend.NewDockerBackend(ss, dockerClient, logger)
	}

	s := scaler.NewScaler(scaleSet.ID, ss.MinRunners, ss.MaxRunners, b, scalesetClient, logger)
	defer s.Shutdown(context.WithoutCancel(ctx))

	if ss.Docker.SharedVolume != "" {
		logger.Info("Shared volume enabled",
			slog.String("path", ss.Docker.SharedVolume),
			slog.Bool("dind", ss.IsDinD()),
		)
	}

	// Register with health server
	if h != nil {
		h.RegisterScaler(ss.ScaleSetName, s)
		defer h.UnregisterScaler(ss.ScaleSetName)
	}

	// Start listening
	logger.Info("Listening for jobs",
		slog.Int("maxRunners", ss.MaxRunners),
		slog.Int("minRunners", ss.MinRunners),
	)

	if err := l.Run(ctx, s); !errors.Is(err, context.Canceled) {
		return fmt.Errorf("listener failed: %w", err)
	}

	return nil
}
