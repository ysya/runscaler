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
	"sort"
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
	"github.com/ysya/runscaler/internal/bytesize"
	"github.com/ysya/runscaler/internal/cachestore"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/diskguard"
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
	cmd.AddCommand(initCmd, validateCmd, statusCmd, doctorCmd, versionCmd, serviceCmd, cacheCmd, runCommand)
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

	// Build every cachestore.CacheStore exactly once — keyed by socket (and,
	// for Tart, by TART_HOME) rather than flattened up front, so the
	// sweepers below can address a specific store directly instead of
	// searching a flat list by Name() (not socket-qualified — see
	// dockerSocketStores' doc comment — so ambiguous once more than one
	// Docker socket is configured). Both the sweepers and the disk guard
	// reclaim through these exact same instances: constructing them twice
	// (once for the guard's flat list, once more for sweeper-wiring) used
	// to also log every "Conflicting ... settings" warning twice at
	// startup. buildCacheStores (used by callers outside run(), e.g. Task
	// 8's `runner cache`) is intentionally not called here for the same
	// reason.
	//
	// Cleanup ownership is per daemon. Group scale sets so distinct sockets
	// never share a client or a global-daemon sweeper.
	dockerSets := groupDockerScaleSets(scaleSets)
	storesBySocket := make(map[string]dockerSocketStores, len(dockerSets))
	for socket, sets := range dockerSets {
		storesBySocket[socket] = dockerCacheStoresForSocket(sets, dockerClients[socket], logger)
	}
	tartTargets := tartCacheStores(scaleSets, logger)

	var cacheStores []cachestore.CacheStore
	for _, socketStores := range storesBySocket {
		cacheStores = append(cacheStores, socketStores.flatten()...)
		startSharedVolumeCleanup(ctx, socketStores.sharedVolume, logger)
		startBuildxCleanup(ctx, socketStores, logger)
		startDockerPrune(ctx, socketStores, logger)
	}
	for _, t := range tartTargets {
		cacheStores = append(cacheStores, t.store)
	}

	// Wire the health endpoint's disk section to the same cacheStores set
	// the guard reclaims from — statfs only, never Measure (see
	// health.DiskStatus's doc comment: /healthz can be polled, so this must
	// stay cheap on every call, unlike 'runner cache').
	if healthServer != nil {
		healthServer.SetDiskProvider(func() []health.DiskStatus { return diskStatusesFor(cacheStores) })
	}

	// Start periodic Tart cache cleanup (one sweeper per unique TART_HOME, so
	// scalesets sharing a TART_HOME share a sweeper and won't race).
	startTartCacheCleanup(ctx, tartTargets, logger)

	// Start the periodic disk-pressure guard over every store built above.
	// The same *diskguard.Guard is threaded into each scale set below as
	// its pre-job-start check (see runScaleSet, scaler.WithDiskChecker)
	// instead of constructing a second one.
	guard := startDiskGuard(ctx, cacheStores, cfg, logger)

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
			if err := runScaleSet(runCtx, ss, dockerClients[ss.Docker.Socket], ssLogger, healthServer, tartCoordinator, guard); err != nil {
				errs <- fmt.Errorf("scaleset %q: %w", ss.ScaleSetName, err)
				cancelRun()
			}
		}()
	}

	wg.Wait()
	close(errs)

	// Prune dangling Docker images once, after all Docker-backed scale sets
	// have finished shutting down. Doing this per-backend races with
	// container removal and concurrent prune operations. The shared volume
	// is deliberately left alone here — see CleanupSharedDocker's doc
	// comment: it holds handoff data for in-flight runs, so only the
	// max-age sweep and disk guard reclaim it, never exit.
	if needsDocker {
		for socket := range dockerSets {
			dockerClient := dockerClients[socket]
			// Build cache belongs to the daemon, not this process. Retention is
			// only performed by the explicit runtime-prune setting.
			backend.CleanupSharedDocker(context.WithoutCancel(ctx), dockerClient, false, logger)
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

// runScaleSet manages the lifecycle of a single scale set. guard is the
// shared disk-pressure guard built once in run() (nil when the disk guard
// is disabled); it is wired into this scale set's Scaler as its
// pre-job-start check, see scaler.WithDiskChecker below.
func runScaleSet(ctx context.Context, ss config.ScaleSetConfig, dockerClient *dockerclient.Client, logger *slog.Logger, h *health.HealthServer, tartCoordinator *backend.TartHostCoordinator, guard *diskguard.Guard) error {
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

	// guard is a *diskguard.Guard; only wrap it into the scaler.DiskChecker
	// option when non-nil so a disabled guard doesn't become a non-nil
	// interface holding a nil pointer (which would panic the first time
	// startRunner called NeedsReclaim on it).
	var scalerOpts []scaler.Option
	if guard != nil {
		scalerOpts = append(scalerOpts, scaler.WithDiskChecker(guard))
	}
	s := scaler.NewScaler(scaleSet.ID, ss.MinRunners, ss.MaxRunners, b, scalesetClient, logger, scalerOpts...)
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

// --- cachestore.CacheStore construction ---
//
// Both the periodic sweepers below and the disk guard (startDiskGuard) must
// reclaim through stores configured identically: two independently derived
// copies of "which scaleset's settings win" for the same store would drift
// the way the pre-Task-7 backend.PruneDockerRuntime / the new
// cachestore.dockerBuildCacheStore duplication did. Every store this
// process constructs — whether for a sweeper or for the disk guard — goes
// through exactly one of the functions below; run() and buildCacheStores
// both call them, so a store's configuration always traces back to one
// selection, never two that could disagree.

// execCommandRunner runs real host commands via os/exec, implementing
// backend.CommandRunner for the Tart cache store's Measure (a plain
// `du -sb <resolved TART_HOME>/cache` — see cachestore.tartStore.Measure).
// backend.execCommandRunner (used for `tart` CLI invocations, which do need
// TART_HOME injected into the child's environment) is unexported, so it
// cannot be reused here; this one needs no such injection because
// tartStore.Path() already resolves TART_HOME to an absolute path before
// this runner ever sees it. cachestore/volume.go's shellQuote sets the same
// precedent for a tiny helper duplicated across this package boundary.
type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// RunStreaming is never called on tartStore's path (only Run is, for
// `du`) but is implemented for real, not stubbed, to satisfy
// backend.CommandRunner honestly for any future caller.
func (execCommandRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// dockerSocketStores holds the cachestore.CacheStore instances scoped to
// one Docker socket, plus the config/interval each store's own sweeper
// needs to log and tick on (not recoverable from a constructed store —
// CacheStore's interface exposes Enabled()/Kind()/etc, not the settings
// behind them). cachestore.CacheStore.Name() is not socket-qualified (e.g.
// "docker-garbage" repeats verbatim across sockets in a multi-daemon
// config), so a flat, unscoped slice cannot be searched by name without
// ambiguity — the sweepers below address a field on this struct directly
// instead of searching.
type dockerSocketStores struct {
	garbage       cachestore.CacheStore
	garbageCfg    cachestore.DockerGarbageConfig
	buildCache    cachestore.CacheStore
	buildCacheCfg cachestore.DockerBuildCacheConfig
	pruneInterval time.Duration

	buildx         cachestore.CacheStore
	buildxCfg      cachestore.BuildxConfig
	buildxInterval time.Duration

	sharedVolume []sharedVolumeSweepTarget
	cacheVolume  []cachestore.CacheStore
}

// flatten returns every store in s as one slice, for the disk guard (which
// only needs a flat set — it groups by filesystem via Path(), not by
// socket or name; see diskguard.Guard.statByFilesystem) and for
// buildCacheStores' return value.
func (s dockerSocketStores) flatten() []cachestore.CacheStore {
	if s.garbage == nil {
		return nil // client was nil; see dockerCacheStoresForSocket
	}
	stores := make([]cachestore.CacheStore, 0, 3+len(s.sharedVolume)+len(s.cacheVolume))
	stores = append(stores, s.garbage, s.buildCache, s.buildx)
	for _, t := range s.sharedVolume {
		stores = append(stores, t.store)
	}
	stores = append(stores, s.cacheVolume...)
	return stores
}

// dockerCacheStoresForSocket builds every Docker-daemon-scoped store for one
// socket's scale sets: the garbage and build-cache stores (always — a
// disabled prune setting is carried as Enabled()==false rather than the
// store not existing, so the disk guard can still name it in a shortfall
// warning), the buildx store, one store per unique shared-volume name, and
// one store per unique cache-volume name.
func dockerCacheStoresForSocket(sets []config.ScaleSetConfig, client *dockerclient.Client, logger *slog.Logger) dockerSocketStores {
	if client == nil {
		return dockerSocketStores{}
	}

	garbageCfg, buildCacheCfg, pruneInterval := dockerPruneSettingsFor(sets)
	garbage := cachestore.NewDockerGarbageStore(client, garbageCfg)
	buildCache := cachestore.NewDockerBuildCacheStore(client, buildCacheCfg)
	// Both stores above resolve the daemon's root dir themselves (one Info()
	// call each); reused here for the buildx/shared-volume/cache-volume
	// configs below instead of paying a third daemon round trip.
	rootDir := garbage.Path()

	buildxCfg, buildxInterval := buildxConfigFor(sets, rootDir)

	return dockerSocketStores{
		garbage:        garbage,
		garbageCfg:     garbageCfg,
		buildCache:     buildCache,
		buildCacheCfg:  buildCacheCfg,
		pruneInterval:  pruneInterval,
		buildx:         cachestore.NewBuildxStore(client, buildxCfg),
		buildxCfg:      buildxCfg,
		buildxInterval: buildxInterval,
		sharedVolume:   sharedVolumeSweepTargetsFor(sets, client, rootDir, logger),
		cacheVolume:    cacheVolumeStoresFor(sets, client, rootDir),
	}
}

// dockerSettingsSource picks which scaleset in sets provides settings for a
// daemon-global store: the first with the routine sweep enabled, so its
// values (and the periodic sweeper's interval) match what the sweeper
// itself would use. If none enables it, settings still come from the first
// Docker scaleset in sets (defaults applied the same way) rather than a
// bare zero value — the disk guard reclaims through these stores
// regardless of Enabled() (2026-08-14 revision: "各 store 的啟用開關只約束例行清理",
// see docs/superpowers/specs/2026-08-13-cache-architecture-design.md), so
// it needs real settings even when no scaleset opted into the periodic
// sweep — a bare zero-value PruneTTL/MaxAge would leave the guard just as
// unable to reclaim as respecting Enabled() did. enabled reports whether
// the *periodic sweeper* should run at all; it is independent of whether
// the returned scaleset's own settings are populated.
func dockerSettingsSource(sets []config.ScaleSetConfig, isEnabled func(config.ScaleSetConfig) bool) (source config.ScaleSetConfig, enabled bool, ok bool) {
	var dockerSets []config.ScaleSetConfig
	for _, ss := range sets {
		if !ss.IsTart() {
			dockerSets = append(dockerSets, ss)
		}
	}
	if len(dockerSets) == 0 {
		return config.ScaleSetConfig{}, false, false
	}

	source = dockerSets[0]
	for _, ss := range dockerSets {
		if isEnabled(ss) {
			return ss, true, true
		}
	}
	return source, false, true
}

// dockerPruneSettingsFor picks the settings for one socket's garbage and
// build-cache stores, plus the interval their shared sweep ticks on — see
// dockerSettingsSource for the selection rule. Only a PruneTTL of exactly 0
// (not negative) falls back to the default: a negative value is an
// explicit "disable the container/image portion, keep build-cache
// retention" signal (see DockerConfig.PruneTTL's doc comment) that
// dockerGarbageStore.Reclaim's own `PruneTTL <= 0` no-op check honors.
func dockerPruneSettingsFor(sets []config.ScaleSetConfig) (cachestore.DockerGarbageConfig, cachestore.DockerBuildCacheConfig, time.Duration) {
	source, enabled, ok := dockerSettingsSource(sets, func(ss config.ScaleSetConfig) bool { return ss.IsDockerPruneEnabled() })
	if !ok {
		return cachestore.DockerGarbageConfig{}, cachestore.DockerBuildCacheConfig{}, 0
	}

	ttl := source.Docker.PruneTTL
	if ttl == 0 {
		ttl = config.DefaultDockerPruneTTL
	}
	interval := source.Docker.PruneInterval
	if interval <= 0 {
		interval = config.DefaultDockerPruneInterval
	}
	cacheMaxAge := source.Docker.BuildCacheMaxAge
	if cacheMaxAge == 0 {
		cacheMaxAge = config.DefaultDockerBuildCacheMaxAge
	}
	return cachestore.DockerGarbageConfig{Enabled: enabled, PruneTTL: ttl},
		cachestore.DockerBuildCacheConfig{Enabled: enabled, MaxAge: cacheMaxAge, BudgetGB: source.Docker.BuildCacheBudgetGB},
		interval
}

// buildxConfigFor picks the settings for one socket's buildx store and the
// interval its own sweep ticks on — see dockerSettingsSource for the
// selection rule (ttl <= 0, not == 0, falls back to the default here:
// buildx-cleanup-ttl has no "negative disables just one portion" meaning
// the way prune-ttl does, so any non-positive value is simply unset).
func buildxConfigFor(sets []config.ScaleSetConfig, rootDir string) (cachestore.BuildxConfig, time.Duration) {
	source, enabled, ok := dockerSettingsSource(sets, func(ss config.ScaleSetConfig) bool { return ss.IsBuildxCleanupEnabled() })
	if !ok {
		return cachestore.BuildxConfig{RootDir: rootDir}, 0
	}

	ttl := source.Docker.BuildxCleanupTTL
	if ttl <= 0 {
		ttl = config.DefaultBuildxCleanupTTL
	}
	interval := source.Docker.BuildxCleanupInterval
	if interval <= 0 {
		interval = config.DefaultBuildxCleanupInterval
	}
	return cachestore.BuildxConfig{Enabled: enabled, MaxAge: ttl, RootDir: rootDir}, interval
}

// sharedVolumeSweepTarget pairs a shared-volume store with the settings its
// own periodic sweep logs and ticks on. These fields are sweeper
// orchestration, not part of cachestore.CacheStore (the same split the
// [disk] guard's own Interval makes from the store interface), so they
// cannot be recovered from store after construction — both it and they come
// from the same selection pass in sharedVolumeSweepTargetsFor, so they can
// never disagree.
type sharedVolumeSweepTarget struct {
	store      cachestore.CacheStore
	volumeName string
	mountPath  string
	ttl        time.Duration
	interval   time.Duration
}

// sharedVolumeSweepTargetsFor selects one target per unique shared-volume
// name among sets, in first-occurrence order. This is startSharedVolumeCleanup's
// pre-Task-7 dedup ("first scaleset per volume name wins, warn on a
// conflicting later one") extracted so both that sweeper and
// dockerCacheStoresForSocket (and so, transitively, the disk guard) select
// from it identically.
//
// Deliberately NOT given the "construct regardless, default the settings"
// treatment dockerPruneSettingsFor/buildxConfigFor/tartCacheStoreFor now
// get (2026-08-14 Enabled() revision): those stores have a boolean routine-
// cleanup switch plus separate TTL/MaxAge fields with sensible always-on
// defaults, so a disabled switch still leaves a real, safe value to reclaim
// by. A shared volume has no such separate default — SharedVolumeTTL *is*
// the retention policy, and 0 means "no policy configured" with no
// project-wide fallback (see DockerConfig.SharedVolumeTTL's doc comment).
// Constructing a store here with MaxAge=0 would make Reclaim(Tier3)'s own
// `days < 1 { days = 1 }` floor silently invent a 1-day TTL nobody
// configured, deleting handoff files an operator never opted into a
// retention policy for at all. So: no TTL configured still means no store,
// exactly as before this revision.
func sharedVolumeSweepTargetsFor(sets []config.ScaleSetConfig, client *dockerclient.Client, rootDir string, logger *slog.Logger) []sharedVolumeSweepTarget {
	type settings struct {
		volumeName, mountPath, helperImage string
		ttl, interval                      time.Duration
	}

	picked := make(map[string]settings)
	var order []string // insertion order — map iteration is not stable
	for _, ss := range sets {
		if ss.IsTart() || ss.Docker.SharedVolume == "" || ss.Docker.SharedVolumeTTL <= 0 {
			continue
		}
		interval := ss.Docker.SharedVolumeCleanupInterval
		if interval <= 0 {
			interval = config.DefaultSharedVolumeCleanupInterval
		}
		s := settings{
			volumeName:  ss.SharedVolumeName(),
			mountPath:   ss.Docker.SharedVolume,
			helperImage: ss.RunnerImage,
			ttl:         ss.Docker.SharedVolumeTTL,
			interval:    interval,
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
		order = append(order, s.volumeName)
	}

	targets := make([]sharedVolumeSweepTarget, 0, len(order))
	for _, name := range order {
		s := picked[name]
		store := cachestore.NewSharedVolumeStore(client, cachestore.SharedVolumeConfig{
			VolumeName:  s.volumeName,
			MountPath:   s.mountPath,
			HelperImage: s.helperImage,
			RootDir:     rootDir,
			MaxAge:      s.ttl,
		})
		targets = append(targets, sharedVolumeSweepTarget{
			store: store, volumeName: s.volumeName, mountPath: s.mountPath, ttl: s.ttl, interval: s.interval,
		})
	}
	return targets
}

// cacheVolumeStoresFor builds one cache-volume store per unique cache
// volume name referenced across sets (the same volume may be named by more
// than one scaleset sharing this socket — see DockerConfig.CacheVolumes'
// doc comment on shared-vs-isolated caches). There is no pre-existing
// sweeper to match settings against: cache volumes were "never removed at
// exit and never swept by any TTL" before this task (same doc comment), so
// the disk guard's Tier4 is the first reclaim path they get. MountPath only
// matters inside the throwaway helper container this store's Measure/Reclaim
// run `du`/`find` in (see cachestore's runVolumeHelper) — never to the
// runner containers that mount the volume — so the first scaleset to name a
// given volume picks that path arbitrarily; ParseCacheVolumes errors are
// ignored here the same way NewDockerBackend ignores them, since
// ScaleSetConfig.Validate already surfaced them before startup.
func cacheVolumeStoresFor(sets []config.ScaleSetConfig, client *dockerclient.Client, rootDir string) []cachestore.CacheStore {
	seen := make(map[string]bool)
	var stores []cachestore.CacheStore
	for _, ss := range sets {
		if ss.IsTart() {
			continue
		}
		mounts, err := ss.Docker.ParseCacheVolumes()
		if err != nil {
			continue
		}
		for _, m := range mounts {
			if seen[m.Volume] {
				continue
			}
			seen[m.Volume] = true
			stores = append(stores, cachestore.NewCacheVolumeStore(client, cachestore.CacheVolumeConfig{
				VolumeName:  m.Volume,
				MountPath:   m.Path,
				HelperImage: ss.RunnerImage,
				RootDir:     rootDir,
			}))
		}
	}
	return stores
}

// tartCacheSweepTarget pairs a Tart-cache store with the settings its own
// periodic sweep logs and ticks on, and whether that periodic sweep is
// itself enabled. enabled is tracked separately from the store's own
// Enabled() (which the disk guard does not consult — see store.go's
// Enabled doc comment): cmd/runner's sweeper-launching code still needs to
// know whether to start a goroutine for this home at all — every home gets
// a target now (see tartCacheStores), not just the ones with a periodic
// sweep turned on.
type tartCacheSweepTarget struct {
	store         cachestore.CacheStore
	home          string
	maxAge        time.Duration
	spaceBudgetGB int
	interval      time.Duration
	enabled       bool
}

// tartCacheStores builds one target per unique TART_HOME across scaleSets —
// every Tart scaleset contributes its home, regardless of whether
// cache-cleanup is enabled there. This is a deliberate difference from
// pre-Task-7 (which built nothing for a disabled home): the disk guard
// reclaims through these stores regardless of Enabled() (2026-08-14
// revision — see dockerSettingsSource's identical rationale, which this
// mirrors for Tart), so it needs to see every configured home, not just
// the ones with a periodic sweep turned on.
func tartCacheStores(scaleSets []config.ScaleSetConfig, logger *slog.Logger) map[string]tartCacheSweepTarget {
	homes := make(map[string][]config.ScaleSetConfig)
	for _, ss := range scaleSets {
		if ss.IsTart() {
			homes[ss.Tart.Home] = append(homes[ss.Tart.Home], ss)
		}
	}

	targets := make(map[string]tartCacheSweepTarget, len(homes))
	for home, sets := range homes {
		targets[home] = tartCacheStoreFor(home, sets, logger)
	}
	return targets
}

// tartCacheStoreFor builds the target for one TART_HOME: settings come from
// the first scaleset sharing it with cache-cleanup enabled — matching
// startTartCacheCleanup's pre-Task-7 "first enabled wins, warn on a
// conflicting later enabled one" dedup exactly, which also determines what
// the periodic sweeper itself uses — or, if no scaleset sharing this home
// enables it, from the first scaleset's own values (still defaulted)
// purely so the disk guard has something real to reclaim by; see
// tartCacheStores' doc comment.
func tartCacheStoreFor(home string, sets []config.ScaleSetConfig, logger *slog.Logger) tartCacheSweepTarget {
	type settings struct {
		maxAge, interval time.Duration
		budgetGB         int
	}

	var chosen *settings
	for _, ss := range sets {
		if !ss.IsTartCacheCleanupEnabled() {
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
		s := settings{maxAge: maxAge, interval: interval, budgetGB: ss.Tart.CacheSpaceBudgetGB}
		if chosen == nil {
			chosen = &s
			continue
		}
		if *chosen != s {
			logger.Warn("Conflicting tart cache cleanup settings for TART_HOME, keeping first",
				slog.String("home", home),
				slog.Duration("kept_max_age", chosen.maxAge),
				slog.Int("kept_budget_gb", chosen.budgetGB),
				slog.Duration("kept_interval", chosen.interval),
				slog.Duration("ignored_max_age", s.maxAge),
				slog.Int("ignored_budget_gb", s.budgetGB),
				slog.Duration("ignored_interval", s.interval),
			)
		}
	}

	enabled := chosen != nil
	if chosen == nil {
		// No scaleset sharing this home enables cache-cleanup — the
		// periodic sweeper stays off, but the disk guard still needs real
		// settings to reclaim by. Fall back to the first scaleset's own
		// values, still defaulted the same way.
		maxAge := sets[0].Tart.CacheMaxAge
		if maxAge <= 0 {
			maxAge = config.DefaultTartCacheMaxAge
		}
		chosen = &settings{maxAge: maxAge, budgetGB: sets[0].Tart.CacheSpaceBudgetGB}
	}

	store := cachestore.NewTartStore(execCommandRunner{}, cachestore.TartConfig{
		Enabled:  enabled,
		Home:     home,
		MaxAge:   chosen.maxAge,
		BudgetGB: chosen.budgetGB,
	})
	return tartCacheSweepTarget{
		store: store, home: home, maxAge: chosen.maxAge, spaceBudgetGB: chosen.budgetGB,
		interval: chosen.interval, enabled: enabled,
	}
}

// buildCacheStores constructs every cachestore.CacheStore configured across
// scaleSets: the Docker-daemon-scoped stores for each unique socket (via
// dockerCacheStoresForSocket) and the Tart-scoped stores for each unique
// TART_HOME (via tartCacheStores). It is the one place this flat, host-wide
// set is assembled — used both for the disk guard's input in run() and as
// the standalone entry point for callers that only want the full set
// without also wiring the periodic sweepers: Task 8's `runner cache` and
// Task 9's pre-job disk check.
//
// NOTE: run() itself does not call this function — see its own comment at
// the call site for why (avoiding double construction and double
// "Conflicting ... settings" warnings). It is kept for Task 8/9 and is
// covered directly by this package's own tests.
func buildCacheStores(scaleSets []config.ScaleSetConfig, dockerClients map[string]*dockerclient.Client, logger *slog.Logger) []cachestore.CacheStore {
	var stores []cachestore.CacheStore
	for socket, sets := range groupDockerScaleSets(scaleSets) {
		stores = append(stores, dockerCacheStoresForSocket(sets, dockerClients[socket], logger).flatten()...)
	}
	for _, t := range tartCacheStores(scaleSets, logger) {
		stores = append(stores, t.store)
	}
	return stores
}

// diskStatusesFor reports capacity for every distinct filesystem stores
// live on, deduplicated by filesystem so a host with many stores on one
// disk (the common case) reports one row, not one per store — matching
// diskguard.Guard.statByFilesystem's own per-filesystem grouping, though
// this never reclaims or logs, it only reads. Statfs only, never Measure
// (see health.DiskStatus's doc comment) — this is called on every /healthz
// request, so it must stay that cheap. A store whose Path() fails statfs is
// silently omitted rather than warned about: unlike diskguard.Guard (which
// sweeps a handful of times an hour on its own schedule), this can run on
// every poll from an external monitor, and logging on every one of those
// would spam the log for a condition the response already reflects (that
// filesystem's entry is simply missing).
func diskStatusesFor(stores []cachestore.CacheStore) []health.DiskStatus {
	seen := make(map[string]bool)
	var statuses []health.DiskStatus
	for _, s := range stores {
		path := s.Path()
		st, err := diskguard.StatFor(path)
		if err != nil || seen[st.ID] {
			continue
		}
		seen[st.ID] = true

		var freePercent float64
		if st.TotalBytes > 0 {
			freePercent = float64(st.FreeBytes) / float64(st.TotalBytes) * 100
		}
		statuses = append(statuses, health.DiskStatus{
			Filesystem:  path,
			FreePercent: freePercent,
			FreeBytes:   st.FreeBytes,
			TotalBytes:  st.TotalBytes,
		})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Filesystem < statuses[j].Filesystem })
	return statuses
}

// logReclaimResult logs the outcome of one store's Reclaim call: Info with
// the reclaimed total when it freed anything — so a data-deleting sweep
// leaves a record an operator can find, restoring the completion logging
// the pre-Task-7 backend.PruneDockerRuntime ("Docker runtime prune
// reclaimed disk") and CleanupSharedVolumeStale ("Shared volume cleanup
// completed") used to do before cachestore.CacheStore.Reclaim's uniform
// (bytes, error) return replaced their own bespoke summaries — Debug when
// it ran but found nothing to reclaim, and Warn when it failed outright.
// action names what ran (e.g. "Docker garbage prune"); store names which
// store — dockerSocketStores' sweep reclaims through two stores in one
// pass, so it calls this once per store rather than logging one combined
// line that would hide which one actually did anything.
func logReclaimResult(logger *slog.Logger, action, store string, freed uint64, err error) {
	if err != nil {
		logger.Warn(action+" failed", slog.String("store", store), slog.Any("error", err))
		return
	}
	if freed > 0 {
		logger.Info(action+" reclaimed disk", slog.String("store", store), slog.String("reclaimed", backend.FormatBytes(freed)))
		return
	}
	logger.Debug(action+" found nothing to reclaim", slog.String("store", store))
}

// startSharedVolumeCleanup launches one background goroutine per shared-volume
// sweep target (see sharedVolumeSweepTargetsFor for the dedup/settings
// selection), running the TTL sweeper periodically through the shared-volume
// store's own Reclaim(Tier3) — the same reclaim path the disk guard uses,
// so there is exactly one implementation of "delete stale shared-volume
// files" in this process (see cachestore.sharedVolumeStore.Reclaim).
// No-op when targets is empty (no scaleset enables TTL, or the Docker
// client was unavailable — see dockerCacheStoresForSocket).
func startSharedVolumeCleanup(ctx context.Context, targets []sharedVolumeSweepTarget, logger *slog.Logger) {
	for _, t := range targets {
		t := t
		logger.Info("Shared volume TTL cleanup enabled",
			slog.String("volume", t.volumeName),
			slog.Duration("ttl", t.ttl),
			slog.Duration("interval", t.interval),
			slog.String("path", t.mountPath),
		)

		sweep := func() {
			freed, err := t.store.Reclaim(ctx, cachestore.Tier3)
			logReclaimResult(logger, "Shared volume cleanup", t.store.Name(), freed, err)
		}

		// Run an initial sweep so users don't wait `interval` for the first cleanup
		// after startup (especially relevant after a crash leaves the volume bloated).
		go func() {
			sweep()
			ticker := time.NewTicker(t.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					sweep()
				}
			}
		}()
	}
}

// startBuildxCleanup launches a background goroutine that periodically removes
// orphaned buildx BuildKit builder containers (and their state volumes) from
// the shared Docker daemon, through the buildx store's own Reclaim(Tier1) —
// the same reclaim path the disk guard uses (see
// cachestore.buildxStore.Reclaim, which itself still delegates to
// backend.CleanupOrphanedBuildxBuilders — see this task's report for why
// that backend function was kept rather than retired). Builders are global
// to the daemon — like the shared volume — so a single sweeper covers all
// Docker scalesets; stores.buildxCfg/buildxInterval were selected from the
// first Docker scaleset with cleanup enabled (see buildxConfigFor). This is
// opt-in because the daemon may contain persistent or unrelated builders.
// No-op when it is not enabled for this socket.
func startBuildxCleanup(ctx context.Context, stores dockerSocketStores, logger *slog.Logger) {
	if !stores.buildxCfg.Enabled {
		return
	}

	logger.Info("Buildx builder cleanup enabled",
		slog.Duration("max_age", stores.buildxCfg.MaxAge),
		slog.Duration("interval", stores.buildxInterval),
	)

	sweep := func() {
		freed, err := stores.buildx.Reclaim(ctx, cachestore.Tier1)
		logReclaimResult(logger, "Buildx builder cleanup", stores.buildx.Name(), freed, err)
	}

	// Run an initial sweep so a bloated daemon is reclaimed promptly at startup
	// rather than after a full interval.
	go func() {
		sweep()
		ticker := time.NewTicker(stores.buildxInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}

// startDockerPrune launches a background goroutine that periodically reclaims
// disk on the shared Docker daemon: stopped containers and dangling images
// older than the TTL (the garbage store's Reclaim(Tier1)), plus age/budget-
// based build cache retention (the build-cache store's Reclaim(Tier2)) — the
// same two reclaim paths the disk guard uses (see cachestore.dockerGarbageStore
// and cachestore.dockerBuildCacheStore). With DooD, job-created garbage lands
// directly on the host daemon, so an exit-time prune alone would never
// reclaim disk on a long-running process; a single sweeper covers all Docker
// scalesets sharing this socket, using the settings selected in
// dockerPruneSettingsFor (the first Docker scaleset with prune enabled).
// This is opt-in because the target daemon may also contain non-runner
// workloads. No-op when prune is not enabled for this socket.
//
// build-cache-budget only takes effect through dockerBuildCacheStore.Reclaim
// (Tier2) — the only place BudgetGB is consumed (see that store's Budget()
// doc comment on why it is deliberately not surfaced to the disk guard's
// separate, statfs-triggered budget-enforcement pass too, which would
// otherwise double-drive the same number). Reclaim(Tier2) is reached two
// ways: this sweep (only when prune is enabled here) and the disk guard's
// own tier ladder under disk pressure, which — since the 2026-08-14 Enabled()
// revision (see store.go's Enabled doc comment) — runs regardless of
// whether prune is enabled, as long as max-tier >= 2 (the default is 3).
// So: with prune disabled, a configured build-cache-budget is enforced only
// once the guard's tier ladder reaches Tier2 under real disk pressure, not
// on this sweep's own schedule — it is not "unenforced entirely" the way it
// was before that revision.
func startDockerPrune(ctx context.Context, stores dockerSocketStores, logger *slog.Logger) {
	if !stores.garbageCfg.Enabled {
		return
	}

	logger.Info("Docker runtime prune enabled",
		slog.Duration("ttl", stores.garbageCfg.PruneTTL),
		slog.Duration("interval", stores.pruneInterval),
		slog.Duration("build_cache_max_age", stores.buildCacheCfg.MaxAge),
		slog.Int("build_cache_budget_gb", stores.buildCacheCfg.BudgetGB),
	)

	sweep := func() {
		freed, err := stores.garbage.Reclaim(ctx, cachestore.Tier1)
		logReclaimResult(logger, "Docker garbage prune", stores.garbage.Name(), freed, err)

		freed, err = stores.buildCache.Reclaim(ctx, cachestore.Tier2)
		logReclaimResult(logger, "Docker build-cache prune", stores.buildCache.Name(), freed, err)
	}

	// Run an initial sweep so a bloated daemon is reclaimed promptly at startup
	// rather than after a full interval.
	go func() {
		sweep()
		ticker := time.NewTicker(stores.pruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}

// startTartCacheCleanup launches one background goroutine per enabled Tart
// cache target (see tartCacheStores for the per-home dedup/settings
// selection — targets includes every configured TART_HOME, so this
// function filters for enabled itself), running `tart prune` on a timer
// through the tart store's own Reclaim(Tier2) — the same reclaim path the
// disk guard uses (see cachestore.tartStore.Reclaim, which itself still
// delegates to backend.PruneTartCache — see this task's report for why
// that backend function was kept rather than retired). Enabled by default;
// no-op for any home where no Tart scaleset has cleanup enabled.
func startTartCacheCleanup(ctx context.Context, targets map[string]tartCacheSweepTarget, logger *slog.Logger) {
	for _, s := range targets {
		if !s.enabled {
			continue
		}
		s := s
		logger.Info("Tart cache cleanup enabled",
			slog.String("home", s.home),
			slog.Duration("max_age", s.maxAge),
			slog.Int("space_budget_gb", s.spaceBudgetGB),
			slog.Duration("interval", s.interval),
		)

		sweep := func() {
			freed, err := s.store.Reclaim(ctx, cachestore.Tier2)
			logReclaimResult(logger, "Tart cache cleanup", s.store.Name(), freed, err)
		}

		go func() {
			// Run an initial sweep so users don't wait `interval` for the
			// first cleanup after startup (especially after a crash leaves
			// the cache bloated).
			sweep()
			ticker := time.NewTicker(s.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					sweep()
				}
			}
		}()
	}
}

// startDiskGuard launches a background goroutine that periodically reclaims
// disk space host-wide once a filesystem's free space drops below
// cfg.Disk's min-free threshold, walking cachestore's tier ladder up to
// max-tier (see diskguard.Guard.Sweep). Structured exactly like
// startDockerPrune: an early return when disabled, an Info log of the
// effective settings, an initial sweep before the ticker starts so a host
// already under pressure at startup doesn't wait a full interval, and a
// Warn (not a fatal error) if a sweep fails — Guard.Sweep itself already
// never fails outright (every per-store error is logged and skipped, see
// its own doc comment), so an error here can only come from the guard
// construction path below.
//
// Returns the constructed Guard, or nil when the guard is disabled or
// misconfigured. The caller (run()) threads this same instance into every
// scale set's pre-job-start check (see runScaleSet, scaler.WithDiskChecker)
// instead of building a second Guard over the same stores.
func startDiskGuard(ctx context.Context, stores []cachestore.CacheStore, cfg config.Config, logger *slog.Logger) *diskguard.Guard {
	if !cfg.IsDiskGuardEnabled() {
		return nil
	}

	minFree := cfg.Disk.MinFree
	if minFree == "" {
		minFree = config.DefaultDiskMinFree
	}
	targetFree := cfg.Disk.TargetFree
	if targetFree == "" {
		targetFree = config.DefaultDiskTargetFree
	}
	// ValidateGlobal (called before startup — see run()) already rejected an
	// unparseable threshold or an inverted min/target pair, so these two
	// parses cannot fail here; handled anyway rather than ignored, matching
	// this file's error-handling style everywhere else.
	minThreshold, err := bytesize.ParseThreshold(minFree)
	if err != nil {
		logger.Warn("Disk guard: invalid min-free, guard disabled", slog.Any("error", err))
		return nil
	}
	targetThreshold, err := bytesize.ParseThreshold(targetFree)
	if err != nil {
		logger.Warn("Disk guard: invalid target-free, guard disabled", slog.Any("error", err))
		return nil
	}

	interval := cfg.Disk.Interval
	if interval <= 0 {
		interval = config.DefaultDiskGuardInterval
	}
	maxTier := cfg.Disk.MaxTier
	if maxTier == 0 {
		maxTier = config.DefaultDiskMaxTier
	}

	guard := diskguard.New(diskguard.Config{
		Enabled:    true,
		MinFree:    minThreshold,
		TargetFree: targetThreshold,
		MaxTier:    cachestore.Tier(maxTier),
	}, stores, diskguard.StatFor, logger)

	logger.Info("Disk guard enabled",
		slog.String("min_free", minFree),
		slog.String("target_free", targetFree),
		slog.Duration("interval", interval),
		slog.Int("max_tier", maxTier),
		slog.Int("stores", len(stores)),
	)

	go func() {
		if err := guard.Sweep(ctx); err != nil {
			logger.Warn("Initial disk guard sweep failed", slog.Any("error", err))
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := guard.Sweep(ctx); err != nil {
					logger.Warn("Periodic disk guard sweep failed", slog.Any("error", err))
				}
			}
		}
	}()

	return guard
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
