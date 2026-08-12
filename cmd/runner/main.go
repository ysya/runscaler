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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
	dockerclient "github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/jsonmessage"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/term"

	"github.com/ysya/runscaler/internal/backend"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/health"
	runnerlock "github.com/ysya/runscaler/internal/lock"
	"github.com/ysya/runscaler/internal/metrics"
	"github.com/ysya/runscaler/internal/scaler"
	"github.com/ysya/runscaler/internal/versioncheck"
)

var (
	version = "dev"
	commit  = ""
	date    = ""
)

func init() {
	// Persistent flags — available to all subcommands
	cmd.PersistentFlags().String("config", "", "Path to config file (TOML)")

	flags := runCommand.Flags()

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
	flags.Int("docker-memory", 0, "Memory limit in MB for each Docker runner container (0 = unlimited)")
	flags.Int("docker-cpu", 0, "CPU cores for each Docker runner container (0 = unlimited)")
	flags.String("docker-platform", "", "Force container platform (e.g. linux/amd64)")

	// Tart backend
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
	flags.String("health-address", config.DefaultHealthAddress, "Health check listen address")

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
	viper.BindPFlag("health-address", flags.Lookup("health-address"))

	// Nested keys (flag name → nested viper key for backend sub-structs):
	viper.BindPFlag("docker.socket", flags.Lookup("docker-socket"))
	viper.BindPFlag("docker.dind", flags.Lookup("dind"))
	viper.BindPFlag("docker.shared-volume", flags.Lookup("shared-volume"))
	viper.BindPFlag("docker.memory", flags.Lookup("docker-memory"))
	viper.BindPFlag("docker.cpu", flags.Lookup("docker-cpu"))
	viper.BindPFlag("docker.platform", flags.Lookup("docker-platform"))
	viper.BindPFlag("tart.runner-dir", flags.Lookup("tart-runner-dir"))
	viper.BindPFlag("tart.cpu", flags.Lookup("tart-cpu"))
	viper.BindPFlag("tart.memory", flags.Lookup("tart-memory"))
	viper.BindPFlag("tart.pool-size", flags.Lookup("tart-pool-size"))

	// Register subcommands
	cmd.AddCommand(initCmd, validateCmd, statusCmd, doctorCmd, versionCmd, serviceCmd, runCommand)
}

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// startScaling loads config, sets up signal handling, and runs the scaler.
// Shared by `runner run` and the root drop-in compat path. var (not func) so
// tests can stub it.
var startScaling = func(cmd *cobra.Command) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}

	releaseLock, err := runnerlock.Acquire(runnerlock.DefaultPath, runnerlock.Info{
		PID:        os.Getpid(),
		StartedAt:  time.Now(),
		ConfigPath: viper.ConfigFileUsed(),
	})
	if err != nil {
		if errors.Is(err, runnerlock.ErrAlreadyRunning) {
			return fmt.Errorf("%w\n\n  Only one runner may run per machine.\n  To manage multiple organizations, use multiple [[scaleset]] entries in one config", err)
		}
		return err
	}
	defer releaseLock()

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
}

var cmd = &cobra.Command{
	Use:     "runner",
	Version: version,
	Short:   "GitHub Actions Runner Auto-Scaler",
	Long: `Dynamically scales GitHub Actions self-hosted runners as Docker containers
or Tart VMs using the actions/scaleset library. Runners are ephemeral — each
handles one job and is removed upon completion.

Supports multiple scale sets via [[scaleset]] entries in TOML config,
or a single scale set via 'runner run' CLI flags.`,
	Example: `  # Quick start
  runner init                            # Generate config.toml interactively
  runner validate --config config.toml   # Verify configuration
  runner run --config config.toml        # Start scaling

  # Using CLI flags
  runner run --url https://github.com/org --name my-runners --token ghp_xxx`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Drop-in compat: an old `runscaler --config X` invocation (e.g. a
		// pre-rename systemd unit after self-update) reaches the root with
		// --config set and no subcommand. Warn and start anyway so the
		// service keeps working; bare `runner` still just prints help.
		if cmd.PersistentFlags().Changed("config") {
			warnLegacy("starting via `runner --config` is deprecated — use `runner run` (or `runner migrate` to update your service)")
			return startScaling(cmd)
		}
		return cmd.Help()
	},
}

var runCommand = &cobra.Command{
	Use:   "run [flags]",
	Short: "Start scaling (run the listener in the foreground)",
	Long: `Connect to GitHub, create or reuse the runner scale set(s), and listen for
jobs — scaling runners up and down until interrupted.`,
	Example: `  runner run --config config.toml
  runner run --url https://github.com/org --name my-runners --token ghp_xxx
  runner run --dry-run --config config.toml`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return startScaling(cmd)
	},
}

func run(ctx context.Context, cfg config.Config) error {
	if err := cfg.ValidateGlobal(); err != nil {
		return fmt.Errorf("invalid global configuration: %w", err)
	}
	var logFile *config.LogFileWriter
	if path, enabled := resolveLogFilePath(cfg); enabled {
		var err error
		logFile, err = config.OpenLogFile(path)
		if err != nil {
			fmt.Fprintf(os.Stdout, "Warning: cannot open log file %s; continuing with stdout only: %v\n", path, err)
		} else {
			defer func() { _ = logFile.Close() }()
		}
	}
	logger := config.NewLoggerWithWriter(cfg.LogLevel, cfg.LogFormat, logFile)

	// Non-fatal config diagnostics (unknown keys, mixed single/multi mode).
	// Warn only — a self-updated deployment with an older config must keep
	// starting; `runner validate` fails on these instead.
	for _, w := range cfg.Warnings {
		logger.Warn(w)
	}

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
	if err := validateScaleSetCollection(scaleSets); err != nil {
		return err
	}

	// Create one Docker client per configured socket. Per-scale-set socket
	// overrides are first-class; silently routing them all through the top-level
	// default can launch privileged jobs on the wrong daemon.
	dockerClients := make(map[string]*dockerclient.Client)
	defer func() {
		for _, client := range dockerClients {
			_ = client.Close()
		}
	}()
	if needsDocker {
		for _, ss := range scaleSets {
			if ss.IsTart() || dockerClients[ss.Docker.Socket] != nil {
				continue
			}
			client, err := dockerclient.New(
				dockerclient.FromEnv,
				dockerclient.WithHost("unix://"+ss.Docker.Socket),
			)
			if err != nil {
				return fmt.Errorf("failed to create Docker client for %s: %w", ss.Docker.Socket, err)
			}
			if _, err := client.Ping(ctx, dockerclient.PingOptions{NegotiateAPIVersion: true}); err != nil {
				_ = client.Close()
				return fmt.Errorf("cannot connect to Docker at %s: %w\n\n"+
					"  Possible fixes:\n"+
					"  1. Ensure Docker is running\n"+
					"  2. Add your user to the docker group: sudo usermod -aG docker $USER\n"+
					"  3. Re-login or run: newgrp docker\n"+
					"  4. Or check the docker socket path in your config",
					ss.Docker.Socket, err)
			}
			dockerClients[ss.Docker.Socket] = client
		}
		// Pull unique runner images on each target Docker daemon.
		pulled := make(map[string]bool)
		for _, ss := range scaleSets {
			pullKey := ss.Docker.Socket + "|" + ss.RunnerImage + "|" + ss.Docker.Platform
			if ss.IsTart() || pulled[pullKey] {
				continue
			}
			logger.Info("Pulling runner image", slog.String("image", ss.RunnerImage), slog.String("platform", ss.Docker.Platform))
			pullCtx, pullCancel := context.WithTimeout(ctx, 30*time.Minute)
			pullOptions := dockerclient.ImagePullOptions{}
			if ss.Docker.Platform != "" {
				parts := strings.SplitN(ss.Docker.Platform, "/", 3)
				platform := ocispec.Platform{OS: parts[0], Architecture: parts[1]}
				if len(parts) == 3 {
					platform.Variant = parts[2]
				}
				pullOptions.Platforms = []ocispec.Platform{platform}
			}
			pull, err := dockerClients[ss.Docker.Socket].ImagePull(pullCtx, ss.RunnerImage, pullOptions)
			if err != nil {
				pullCancel()
				return fmt.Errorf("failed to pull runner image %s: %w", ss.RunnerImage, err)
			}
			fd := os.Stdout.Fd()
			pullOutput := io.Writer(os.Stdout)
			if logFile != nil {
				pullOutput = io.MultiWriter(os.Stdout, logFile)
			}
			pullErr := jsonmessage.DisplayJSONMessagesStream(pull, pullOutput, fd, term.IsTerminal(int(fd)), nil)
			pull.Close()
			pullCancel()
			if pullErr != nil {
				return fmt.Errorf("failed to pull runner image %s: %w", ss.RunnerImage, pullErr)
			}
			pulled[pullKey] = true
		}
	}

	// One coordinator is shared by every Tart scale set so MAC slots and the
	// Apple two-VM limit are host-wide rather than reset per scale set.
	var tartCoordinator *backend.TartHostCoordinator
	// Verify Tart binary exists if any scaleset uses Tart backend
	if needsTart {
		tartCoordinator = backend.NewTartHostCoordinator(2)
		if _, err := exec.LookPath("tart"); err != nil {
			return fmt.Errorf("tart binary not found in PATH: %w\n\n"+
				"  Install Tart: brew install cirruslabs/cli/tart", err)
		}
		logger.Info("Tart backend enabled")

		// Ensure Tart images are available locally (auto-pull if missing)
		pulled := make(map[string]bool)
		for _, ss := range scaleSets {
			if !ss.IsTart() || pulled[ss.RunnerImage] {
				continue
			}
			tb := backend.NewTartBackendWithCoordinator(ss, logger, tartCoordinator)
			if err := tb.EnsureImage(ctx); err != nil {
				return err
			}
			pulled[ss.RunnerImage] = true
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
		healthServer = health.NewHealthServer(cfg.HealthPort, version, logger)
		ln, err := net.Listen("tcp", net.JoinHostPort(cfg.HealthAddress, fmt.Sprintf("%d", cfg.HealthPort)))
		if err != nil {
			return fmt.Errorf("failed to start health server on port %d: %w", cfg.HealthPort, err)
		}
		go func() {
			if err := healthServer.Serve(ln); err != nil && err != http.ErrServerClosed {
				logger.Error("Health server error", slog.Any("error", err))
			}
		}()
		defer func() { _ = healthServer.Shutdown(context.WithoutCancel(ctx)) }()
		logger.Info("Health check server started", slog.String("address", cfg.HealthAddress), slog.Int("port", cfg.HealthPort))
	}

	// Cleanup ownership is per daemon. Group scale sets so distinct sockets
	// never share a client or a global-daemon sweeper.
	dockerSets := groupDockerScaleSets(scaleSets)
	for socket, sets := range dockerSets {
		client := dockerClients[socket]
		startSharedVolumeCleanup(ctx, client, sets, logger)
		startBuildxCleanup(ctx, client, sets, logger)
		startDockerPrune(ctx, client, sets, logger)
	}

	// Start periodic Tart cache cleanup (one sweeper per unique TART_HOME, so
	// scalesets sharing a TART_HOME share a sweeper and won't race).
	startTartCacheCleanup(ctx, scaleSets, logger)

	logger.Info("Starting scale sets", slog.Int("count", len(scaleSets)))

	// Non-blocking version check at startup
	go func() {
		release, err := versioncheck.Latest(ctx)
		if err != nil {
			return
		}
		if versioncheck.IsNewer(version, release.TagName) {
			logger.Warn("A newer version of runner is available — run 'runner update' to upgrade",
				slog.String("current", version),
				slog.String("latest", release.TagName),
			)
		}
	}()

	// Run each scale set in its own goroutine
	var wg sync.WaitGroup
	errs := make(chan error, len(scaleSets))
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	for i, ss := range scaleSets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ssLogger := config.NewScaleSetLoggerWithWriter(cfg.LogLevel, cfg.LogFormat, ss.ScaleSetName, i, logFile)
			if err := runScaleSet(runCtx, ss, dockerClients[ss.Docker.Socket], ssLogger, healthServer, tartCoordinator); err != nil {
				errs <- fmt.Errorf("scaleset %q: %w", ss.ScaleSetName, err)
				cancelRun()
			}
		}()
	}

	wg.Wait()
	close(errs)

	// Clean up shared Docker resources once, after all Docker-backed scale
	// sets have finished shutting down. Doing this per-backend races with
	// container removal and concurrent prune operations.
	if needsDocker {
		for socket, sets := range dockerSets {
			dockerClient := dockerClients[socket]
			// Unique named volumes backing the shared-volume mounts of Docker
			// scalesets; each is removed at exit. Cache volumes are persistent
			// and deliberately survive.
			var volumeNames []string
			seenVolumes := make(map[string]bool)
			for _, ss := range sets {
				if ss.IsTart() || ss.Docker.SharedVolume == "" {
					continue
				}
				if name := ss.SharedVolumeName(); !seenVolumes[name] {
					seenVolumes[name] = true
					volumeNames = append(volumeNames, name)
				}
			}
			// Build cache belongs to the daemon, not this process. Retention is
			// only performed by the explicit runtime-prune setting.
			backend.CleanupSharedDocker(context.WithoutCancel(ctx), dockerClient, volumeNames, false, logger)
		}
	}

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
func runScaleSet(ctx context.Context, ss config.ScaleSetConfig, dockerClient *dockerclient.Client, logger *slog.Logger, h *health.HealthServer, tartCoordinator *backend.TartHostCoordinator) error {
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
			DisableUpdate: ss.IsUpdateDisabled(),
		},
	}

	scaleSet, err := scalesetClient.GetRunnerScaleSet(ctx, runnerGroupID, ss.ScaleSetName)
	if err != nil {
		return fmt.Errorf("failed to get runner scale set: %w", err)
	}
	if scaleSet == nil {
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

	// Delete scale set on exit (with timeout to avoid hanging if API is unresponsive)
	defer func() {
		logger.Info("Deleting runner scale set", slog.Int("scaleSetID", scaleSet.ID))
		cleanCtx, cleanCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cleanCancel()
		if err := scalesetClient.DeleteRunnerScaleSet(cleanCtx, scaleSet.ID); err != nil {
			logger.Error("Failed to delete runner scale set", slog.Any("error", err))
		}
	}()

	// Create backend based on config (persists across reconnections)
	var b backend.RunnerBackend
	if ss.IsTart() {
		tb := backend.NewTartBackendWithCoordinator(ss, logger, tartCoordinator)
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

	// Metrics recorder for this scale set
	recorder := &metrics.Recorder{}

	// Register with health server
	if h != nil {
		h.RegisterScaler(ss.ScaleSetName, s)
		h.RegisterMetrics(ss.ScaleSetName, recorder)
		defer h.UnregisterScaler(ss.ScaleSetName)
	}

	// Session ID for message session
	hostname, err := os.Hostname()
	if err != nil {
		hostname = uuid.NewString()
	}
	sessionID := fmt.Sprintf("%s-%s", hostname, ss.ScaleSetName)

	// Reconnection loop: recreate session + listener on transient failures
	backoff := 5 * time.Second
	const maxBackoff = 60 * time.Second

	for {
		logger.Info("Listening for jobs",
			slog.Int("maxRunners", ss.MaxRunners),
			slog.Int("minRunners", ss.MinRunners),
		)

		listenStart := time.Now()
		if h != nil {
			h.MarkDisconnected(ss.ScaleSetName, "connecting")
		}
		listenErr := listenOnce(ctx, scalesetClient, scaleSet.ID, sessionID, ss.MaxRunners, s, recorder, logger, func() {
			if h != nil {
				h.MarkConnected(ss.ScaleSetName)
			}
		})
		if ctx.Err() != nil || errors.Is(listenErr, context.Canceled) {
			// Clean exit: the parent context was canceled (SIGTERM/SIGINT or
			// another scale set encountered a permanent failure).
			return nil
		}
		if listenErr == nil {
			return fmt.Errorf("listener stopped unexpectedly")
		}

		// If the listener ran for a meaningful period, the disconnect is
		// a fresh transient failure — reset backoff so we reconnect quickly.
		if time.Since(listenStart) > maxBackoff {
			backoff = 5 * time.Second
		}

		logger.Warn("Listener disconnected, will reconnect",
			slog.Any("error", listenErr),
			slog.Duration("backoff", backoff),
		)
		if h != nil {
			h.MarkDisconnected(ss.ScaleSetName, listenErr.Error())
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func groupDockerScaleSets(scaleSets []config.ScaleSetConfig) map[string][]config.ScaleSetConfig {
	grouped := make(map[string][]config.ScaleSetConfig)
	for _, ss := range scaleSets {
		if !ss.IsTart() {
			grouped[ss.Docker.Socket] = append(grouped[ss.Docker.Socket], ss)
		}
	}
	return grouped
}

func validateScaleSetCollection(scaleSets []config.ScaleSetConfig) error {
	names := make(map[string]int)
	totalTartMin := 0
	totalTartPool := 0
	for i, ss := range scaleSets {
		if prior, ok := names[ss.ScaleSetName]; ok {
			return fmt.Errorf("scaleset[%d] %q duplicates scaleset[%d] name; names must be unique for lifecycle and health tracking", i, ss.ScaleSetName, prior)
		}
		names[ss.ScaleSetName] = i
		if ss.IsTart() {
			totalTartMin += ss.MinRunners
			totalTartPool += ss.Tart.PoolSize
		}
	}
	if totalTartMin > 2 {
		return fmt.Errorf("combined Tart min-runners is %d, exceeding the host-wide macOS VM limit of 2", totalTartMin)
	}
	if totalTartPool > 2 {
		return fmt.Errorf("combined Tart pool-size is %d, exceeding the host-wide macOS VM limit of 2", totalTartPool)
	}
	return nil
}

// startSharedVolumeCleanup launches one background goroutine per unique
// shared-volume name among the Docker scalesets with a shared volume and
// TTL > 0, running the TTL sweeper periodically. When two scalesets share a
// volume name, the first one wins (with a warn if its config differs) — two
// sweepers on the same volume would just race. Cache volumes are never swept.
// No-op when no scaleset enables TTL or when the Docker client is unavailable.
func startSharedVolumeCleanup(ctx context.Context, client *dockerclient.Client, scaleSets []config.ScaleSetConfig, logger *slog.Logger) {
	if client == nil {
		return
	}

	type sweeper struct {
		volumeName  string
		ttl         time.Duration
		interval    time.Duration
		mountPath   string
		helperImage string
	}

	// Group by the named volume backing the mount — scalesets may isolate
	// themselves on distinct volumes, each needing its own sweeper.
	picked := make(map[string]sweeper)
	for _, ss := range scaleSets {
		if ss.IsTart() || ss.Docker.SharedVolume == "" || ss.Docker.SharedVolumeTTL <= 0 {
			continue
		}
		interval := ss.Docker.SharedVolumeCleanupInterval
		if interval <= 0 {
			interval = config.DefaultSharedVolumeCleanupInterval
		}
		s := sweeper{
			volumeName:  ss.SharedVolumeName(),
			ttl:         ss.Docker.SharedVolumeTTL,
			interval:    interval,
			mountPath:   ss.Docker.SharedVolume,
			helperImage: ss.RunnerImage,
		}
		if existing, ok := picked[s.volumeName]; ok {
			if existing != s {
				logger.Warn("Conflicting shared volume cleanup settings for volume, keeping first",
					slog.String("volume", s.volumeName),
					slog.Duration("kept_ttl", existing.ttl),
					slog.Duration("kept_interval", existing.interval),
					slog.String("kept_path", existing.mountPath),
					slog.Duration("ignored_ttl", s.ttl),
					slog.Duration("ignored_interval", s.interval),
					slog.String("ignored_path", s.mountPath),
				)
			}
			continue
		}
		picked[s.volumeName] = s
	}

	for _, s := range picked {
		s := s
		logger.Info("Shared volume TTL cleanup enabled",
			slog.String("volume", s.volumeName),
			slog.Duration("ttl", s.ttl),
			slog.Duration("interval", s.interval),
			slog.String("path", s.mountPath),
		)

		// Run an initial sweep so users don't wait `interval` for the first cleanup
		// after startup (especially relevant after a crash leaves the volume bloated).
		go func() {
			if err := backend.CleanupSharedVolumeStale(ctx, client, s.helperImage, s.volumeName, s.mountPath, s.ttl, logger); err != nil {
				logger.Warn("Initial shared volume cleanup failed", slog.Any("error", err))
			}

			ticker := time.NewTicker(s.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := backend.CleanupSharedVolumeStale(ctx, client, s.helperImage, s.volumeName, s.mountPath, s.ttl, logger); err != nil {
						logger.Warn("Periodic shared volume cleanup failed", slog.Any("error", err))
					}
				}
			}
		}()
	}
}

// startBuildxCleanup launches a background goroutine that periodically removes
// orphaned buildx BuildKit builder containers (and their state volumes) from
// the shared Docker daemon. Builders are global to the daemon — like the
// shared volume — so a single sweeper covers all Docker scalesets; the first
// Docker scaleset with cleanup enabled provides the settings. This is opt-in
// because the daemon may contain persistent or unrelated builders.
func startBuildxCleanup(ctx context.Context, client *dockerclient.Client, scaleSets []config.ScaleSetConfig, logger *slog.Logger) {
	if client == nil {
		return
	}

	var (
		ttl      time.Duration
		interval time.Duration
		enabled  bool
	)
	for _, ss := range scaleSets {
		if ss.IsTart() || !ss.IsBuildxCleanupEnabled() {
			continue
		}
		enabled = true
		ttl = ss.Docker.BuildxCleanupTTL
		if ttl <= 0 {
			ttl = config.DefaultBuildxCleanupTTL
		}
		interval = ss.Docker.BuildxCleanupInterval
		if interval <= 0 {
			interval = config.DefaultBuildxCleanupInterval
		}
		break
	}
	if !enabled {
		return
	}

	logger.Info("Buildx builder cleanup enabled",
		slog.Duration("max_age", ttl),
		slog.Duration("interval", interval),
	)

	// Run an initial sweep so a bloated daemon is reclaimed promptly at startup
	// rather than after a full interval.
	go func() {
		if err := backend.CleanupOrphanedBuildxBuilders(ctx, client, ttl, logger); err != nil {
			logger.Warn("Initial buildx cleanup failed", slog.Any("error", err))
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := backend.CleanupOrphanedBuildxBuilders(ctx, client, ttl, logger); err != nil {
					logger.Warn("Periodic buildx cleanup failed", slog.Any("error", err))
				}
			}
		}
	}()
}

// startDockerPrune launches a background goroutine that periodically reclaims
// disk on the shared Docker daemon: stopped containers and dangling images
// older than the TTL, plus age/budget-based build cache retention. With DooD,
// job-created garbage lands directly on the host daemon — which is global,
// like buildx builders — so a single sweeper covers all Docker scalesets and
// the first Docker scaleset with prune enabled provides the settings. This is
// opt-in because the target daemon may also contain non-runner workloads.
func startDockerPrune(ctx context.Context, client *dockerclient.Client, scaleSets []config.ScaleSetConfig, logger *slog.Logger) {
	if client == nil {
		return
	}

	var (
		ttl         time.Duration
		interval    time.Duration
		cacheMaxAge time.Duration
		budgetGB    int
		enabled     bool
	)
	for _, ss := range scaleSets {
		if ss.IsTart() || !ss.IsDockerPruneEnabled() {
			continue
		}
		enabled = true
		ttl = ss.Docker.PruneTTL
		if ttl == 0 {
			ttl = config.DefaultDockerPruneTTL
		}
		interval = ss.Docker.PruneInterval
		if interval <= 0 {
			interval = config.DefaultDockerPruneInterval
		}
		cacheMaxAge = ss.Docker.BuildCacheMaxAge
		if cacheMaxAge == 0 {
			cacheMaxAge = config.DefaultDockerBuildCacheMaxAge
		}
		budgetGB = ss.Docker.BuildCacheBudgetGB
		break
	}
	if !enabled {
		return
	}

	logger.Info("Docker runtime prune enabled",
		slog.Duration("ttl", ttl),
		slog.Duration("interval", interval),
		slog.Duration("build_cache_max_age", cacheMaxAge),
		slog.Int("build_cache_budget_gb", budgetGB),
	)

	// Run an initial sweep so a bloated daemon is reclaimed promptly at startup
	// rather than after a full interval.
	go func() {
		if err := backend.PruneDockerRuntime(ctx, client, ttl, cacheMaxAge, budgetGB, logger); err != nil {
			logger.Warn("Initial docker runtime prune failed", slog.Any("error", err))
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := backend.PruneDockerRuntime(ctx, client, ttl, cacheMaxAge, budgetGB, logger); err != nil {
					logger.Warn("Periodic docker runtime prune failed", slog.Any("error", err))
				}
			}
		}
	}()
}

// startTartCacheCleanup launches one background goroutine per unique TART_HOME
// among the Tart-backed scalesets, running `tart prune` on a timer to reclaim
// OCI/IPSW layers left behind by image updates. When two scalesets share a
// TART_HOME, the first one wins (with a warn if its config differs) — two
// sweepers on the same cache would just race. Enabled by default; no-op when
// no Tart scaleset has cleanup enabled.
func startTartCacheCleanup(ctx context.Context, scaleSets []config.ScaleSetConfig, logger *slog.Logger) {
	type sweeper struct {
		home          string
		maxAge        time.Duration
		spaceBudgetGB int
		interval      time.Duration
	}

	// Group by TART_HOME (empty string is a valid key — tart's default ~/.tart).
	picked := make(map[string]sweeper)
	for _, ss := range scaleSets {
		if !ss.IsTart() || !ss.IsTartCacheCleanupEnabled() {
			continue
		}
		maxAge := ss.Tart.CacheMaxAge
		if maxAge <= 0 {
			maxAge = config.DefaultTartCacheMaxAge
		}
		interval := ss.Tart.CacheCleanupInterval
		if interval <= 0 {
			interval = config.DefaultTartCacheCleanupInterval
		}
		s := sweeper{
			home:          ss.Tart.Home,
			maxAge:        maxAge,
			spaceBudgetGB: ss.Tart.CacheSpaceBudgetGB,
			interval:      interval,
		}
		if existing, ok := picked[ss.Tart.Home]; ok {
			if existing != s {
				logger.Warn("Conflicting tart cache cleanup settings for TART_HOME, keeping first",
					slog.String("home", ss.Tart.Home),
					slog.Duration("kept_max_age", existing.maxAge),
					slog.Int("kept_budget_gb", existing.spaceBudgetGB),
					slog.Duration("kept_interval", existing.interval),
					slog.Duration("ignored_max_age", s.maxAge),
					slog.Int("ignored_budget_gb", s.spaceBudgetGB),
					slog.Duration("ignored_interval", s.interval),
				)
			}
			continue
		}
		picked[ss.Tart.Home] = s
	}

	for _, s := range picked {
		s := s
		logger.Info("Tart cache cleanup enabled",
			slog.String("home", s.home),
			slog.Duration("max_age", s.maxAge),
			slog.Int("space_budget_gb", s.spaceBudgetGB),
			slog.Duration("interval", s.interval),
		)
		go func() {
			// Run an initial sweep so users don't wait `interval` for the
			// first cleanup after startup (especially after a crash leaves
			// the cache bloated).
			if err := backend.PruneTartCache(ctx, s.home, s.maxAge, s.spaceBudgetGB, logger); err != nil {
				logger.Warn("Initial tart cache cleanup failed", slog.Any("error", err))
			}

			ticker := time.NewTicker(s.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := backend.PruneTartCache(ctx, s.home, s.maxAge, s.spaceBudgetGB, logger); err != nil {
						logger.Warn("Periodic tart cache cleanup failed", slog.Any("error", err))
					}
				}
			}
		}()
	}
}

// listenOnce creates a message session and listener, then runs until
// disconnection or context cancellation. Callers retry on transient errors.
func listenOnce(ctx context.Context, client *scaleset.Client, scaleSetID int, sessionID string, maxRunners int, s *scaler.Scaler, recorder *metrics.Recorder, logger *slog.Logger, onReady func()) error {
	sessionClient, err := client.MessageSessionClient(ctx, scaleSetID, sessionID)
	if err != nil {
		return fmt.Errorf("failed to create message session: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sessionClient.Close(closeCtx); err != nil {
			logger.Warn("Failed to close message session", slog.Any("error", err))
		}
	}()

	l, err := listener.New(sessionClient, listener.Config{
		ScaleSetID: scaleSetID,
		MaxRunners: maxRunners,
		Logger:     logger,
	}, listener.WithMetricsRecorder(recorder))
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}
	if onReady != nil {
		onReady()
	}

	return l.Run(ctx, s)
}
