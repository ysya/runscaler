package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	version = "dev"
	commit  = ""
	date    = ""
)

var cfg Config

func init() {
	flags := cmd.Flags()

	// Required
	flags.StringVar(&cfg.RegistrationURL, "url", "", "REQUIRED: Registration URL (e.g. https://github.com/org or https://github.com/org/repo)")
	flags.StringVar(&cfg.ScaleSetName, "name", "", "REQUIRED: Name of the scale set (also used as runs-on label)")
	flags.StringVar(&cfg.Token, "token", "", "REQUIRED: Personal access token")

	// Scaling
	flags.IntVar(&cfg.MaxRunners, "max-runners", 10, "Maximum number of runners")
	flags.IntVar(&cfg.MinRunners, "min-runners", 0, "Minimum number of runners")

	// Runner
	flags.StringSliceVar(&cfg.Labels, "labels", nil, "Runner labels (comma-separated). Defaults to --name if not provided")
	flags.StringVar(&cfg.RunnerGroup, "runner-group", scaleset.DefaultRunnerGroup, "Runner group name")
	flags.StringVar(&cfg.RunnerImage, "runner-image", "ghcr.io/actions/actions-runner:latest", "Docker image for runners")

	// Docker
	flags.StringVar(&cfg.DockerSocket, "docker-socket", "/var/run/docker.sock", "Path to Docker socket")
	flags.BoolVar(&cfg.DinD, "dind", true, "Mount Docker socket into runner containers (Docker-in-Docker)")
	flags.StringVar(&cfg.SharedVolume, "shared-volume", "", "Shared Docker volume mounted into all runners (container path, e.g. /shared)")

	// Logging
	flags.StringVar(&cfg.LogLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flags.StringVar(&cfg.LogFormat, "log-format", "text", "Log format (text, json)")

	// Config file
	flags.String("config", "", "Path to config file (TOML)")

	viper.BindPFlags(flags)
}

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var cmd = &cobra.Command{
	Use:     "runscaler",
	Version: version,
	Short:   "GitHub Actions Runner Auto-Scaler for Docker",
	Long: `Dynamically scales GitHub Actions self-hosted runners as Docker containers
using the actions/scaleset library. Runners are ephemeral — each container
handles one job and is removed upon completion.

Requires a GitHub Personal Access Token with admin:org scope (or repo scope
for repository-level runners). Configuration can be provided via CLI flags
or a TOML config file (--config).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Load config file: explicit --config flag, or search default paths
		if configFile, _ := cmd.Flags().GetString("config"); configFile != "" {
			viper.SetConfigFile(configFile)
			if err := viper.ReadInConfig(); err != nil {
				return fmt.Errorf("failed to read config file: %w", err)
			}
		} else {
			viper.SetConfigName("config")
			viper.SetConfigType("toml")
			viper.AddConfigPath(".")
			viper.AddConfigPath("/etc/runscaler")
			viper.ReadInConfig() // ignore error — default paths are optional
		}

		// Unmarshal all sources (flag > config file > default) into cfg
		if err := viper.Unmarshal(&cfg); err != nil {
			return fmt.Errorf("failed to parse configuration: %w", err)
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		// Force exit on second signal
		go func() {
			<-ctx.Done()
			// First signal received, ctx is cancelled. Wait for another.
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			<-sig
			fmt.Fprintln(os.Stderr, "\nForce exit")
			os.Exit(1)
		}()

		return run(ctx, cfg)
	},
}

func run(ctx context.Context, c Config) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("configuration validation failed: %w", err)
	}

	logger := c.Logger()

	// Create scaleset client
	scalesetClient, err := c.ScalesetClient(logger)
	if err != nil {
		return fmt.Errorf("failed to create scaleset client: %w", err)
	}

	// Resolve runner group ID
	var runnerGroupID int
	switch c.RunnerGroup {
	case scaleset.DefaultRunnerGroup:
		runnerGroupID = 1
	default:
		runnerGroup, err := scalesetClient.GetRunnerGroupByName(ctx, c.RunnerGroup)
		if err != nil {
			return fmt.Errorf("failed to get runner group: %w", err)
		}
		runnerGroupID = runnerGroup.ID
	}

	// Get or create runner scale set
	desired := &scaleset.RunnerScaleSet{
		Name:          c.ScaleSetName,
		RunnerGroupID: runnerGroupID,
		Labels:        c.BuildLabels(),
		RunnerSetting: scaleset.RunnerSetting{
			DisableUpdate: true,
		},
	}

	scaleSet, err := scalesetClient.GetRunnerScaleSet(ctx, runnerGroupID, c.ScaleSetName)
	if err != nil || scaleSet == nil {
		// Not found — create new
		scaleSet, err = scalesetClient.CreateRunnerScaleSet(ctx, desired)
		if err != nil {
			return fmt.Errorf("failed to create runner scale set: %w", err)
		}
		logger.Info("Scale set created",
			slog.Int("scaleSetID", scaleSet.ID),
			slog.String("name", scaleSet.Name),
		)
	} else {
		// Found — update labels and settings
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
	scalesetClient.SetSystemInfo(systemInfo(scaleSet.ID))

	// Delete scale set on exit
	defer func() {
		logger.Info("Deleting runner scale set", slog.Int("scaleSetID", scaleSet.ID))
		if err := scalesetClient.DeleteRunnerScaleSet(context.WithoutCancel(ctx), scaleSet.ID); err != nil {
			logger.Error("Failed to delete runner scale set", slog.String("error", err.Error()))
		}
	}()

	// Create Docker client and verify connectivity
	dockerClient, err := dockerclient.NewClientWithOpts(
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
			"  4. Or check the docker-socket path in your config",
			c.DockerSocket, err)
	}

	// Pull runner image
	logger.Info("Pulling runner image", slog.String("image", c.RunnerImage))
	pull, err := dockerClient.ImagePull(ctx, c.RunnerImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull runner image: %w", err)
	}
	if _, err := io.ReadAll(pull); err != nil {
		return fmt.Errorf("failed to read image pull response: %w", err)
	}
	pull.Close()

	// Create message session
	hostname, err := os.Hostname()
	if err != nil {
		hostname = uuid.NewString()
		logger.Info("Failed to get hostname, using UUID", "uuid", hostname)
	}

	sessionClient, err := scalesetClient.MessageSessionClient(ctx, scaleSet.ID, hostname)
	if err != nil {
		return fmt.Errorf("failed to create message session: %w", err)
	}
	defer sessionClient.Close(context.Background())

	// Create listener
	l, err := listener.New(sessionClient, listener.Config{
		ScaleSetID: scaleSet.ID,
		MaxRunners: c.MaxRunners,
		Logger:     logger.WithGroup("listener"),
	})
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}

	// Create scaler
	scaler := &Scaler{
		logger:         logger.WithGroup("scaler"),
		runnerImage:    c.RunnerImage,
		scaleSetID:     scaleSet.ID,
		dockerClient:   dockerClient,
		scalesetClient: scalesetClient,
		minRunners:     c.MinRunners,
		maxRunners:     c.MaxRunners,
		dockerSocket:   c.DockerSocket,
		dind:           c.DinD,
		sharedVolume:   c.SharedVolume,
		runners: runnerState{
			idle: make(map[string]string),
			busy: make(map[string]string),
		},
	}
	defer scaler.shutdown(context.WithoutCancel(ctx))

	// Start listening
	logger.Info("Listening for jobs",
		slog.String("scaleSet", c.ScaleSetName),
		slog.Int("maxRunners", c.MaxRunners),
		slog.Int("minRunners", c.MinRunners),
	)

	if err := l.Run(ctx, scaler); !errors.Is(err, context.Canceled) {
		return fmt.Errorf("listener run failed: %w", err)
	}

	return nil
}
