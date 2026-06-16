package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"golang.org/x/term"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/backend"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/health"
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
			dockerclient.WithHost("unix://"+cfg.Defaults.Docker.Socket),
			dockerclient.WithAPIVersionNegotiation(),
		)
		if err != nil {
			return fmt.Errorf("failed to create docker client: %w", err)
		}
		defer dockerClient.Close()

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
			pullKey := ss.RunnerImage + "|" + ss.Docker.Platform
			if ss.IsTart() || pulled[pullKey] {
				continue
			}
			logger.Info("Pulling runner image", slog.String("image", ss.RunnerImage), slog.String("platform", ss.Docker.Platform))
			pullCtx, pullCancel := context.WithTimeout(ctx, 30*time.Minute)
			pull, err := dockerClient.ImagePull(pullCtx, ss.RunnerImage, image.PullOptions{Platform: ss.Docker.Platform})
			if err != nil {
				pullCancel()
				return fmt.Errorf("failed to pull runner image %s: %w", ss.RunnerImage, err)
			}
			fd := os.Stdout.Fd()
			pullErr := jsonmessage.DisplayJSONMessagesStream(pull, os.Stdout, fd, term.IsTerminal(int(fd)), nil)
			pull.Close()
			pullCancel()
			if pullErr != nil {
				return fmt.Errorf("failed to pull runner image %s: %w", ss.RunnerImage, pullErr)
			}
			pulled[pullKey] = true
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
			if !ss.IsTart() || pulled[ss.RunnerImage] {
				continue
			}
			tb := backend.NewTartBackend(ss, logger)
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
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.HealthPort))
		if err != nil {
			return fmt.Errorf("failed to start health server on port %d: %w", cfg.HealthPort, err)
		}
		go func() {
			if err := healthServer.Serve(ln); err != nil && err != http.ErrServerClosed {
				logger.Error("Health server error", slog.Any("error", err))
			}
		}()
		defer func() { _ = healthServer.Shutdown(context.WithoutCancel(ctx)) }()
		logger.Info("Health check server started", slog.Int("port", cfg.HealthPort))
	}

	// Start periodic shared-volume TTL cleanup (one shared sweeper per process —
	// all Docker scalesets share the same `runscaler-shared` volume, so the
	// first matching scaleset wins and others are ignored).
	startSharedVolumeCleanup(ctx, dockerClient, scaleSets, logger)

	// Start periodic buildx builder cleanup (orphaned BuildKit builders are
	// global to the shared Docker daemon, so one sweeper covers all scalesets).
	startBuildxCleanup(ctx, dockerClient, scaleSets, logger)

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

	for i, ss := range scaleSets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ssLogger := config.NewScaleSetLogger(cfg.LogLevel, cfg.LogFormat, ss.ScaleSetName, i)
			if err := runScaleSet(ctx, ss, dockerClient, ssLogger, healthServer); err != nil {
				errs <- fmt.Errorf("scaleset %q: %w", ss.ScaleSetName, err)
			}
		}()
	}

	wg.Wait()
	close(errs)

	// Clean up shared Docker resources once, after all Docker-backed scale
	// sets have finished shutting down. Doing this per-backend races with
	// container removal and concurrent prune operations.
	if needsDocker && dockerClient != nil {
		removeVolume := false
		for _, ss := range scaleSets {
			if !ss.IsTart() && ss.Docker.SharedVolume != "" {
				removeVolume = true
				break
			}
		}
		backend.CleanupSharedDocker(context.WithoutCancel(ctx), dockerClient, removeVolume, logger)
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
		listenErr := listenOnce(ctx, scalesetClient, scaleSet.ID, sessionID, ss.MaxRunners, s, recorder, logger)
		if listenErr == nil || ctx.Err() != nil || errors.Is(listenErr, context.Canceled) {
			// Clean exit: either listener finished normally or the parent
			// context was canceled (user sent SIGTERM/SIGINT).
			return nil
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

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// startSharedVolumeCleanup launches a background goroutine that runs the
// shared-volume TTL sweeper periodically. The first Docker scaleset with a
// shared volume and TTL > 0 wins — runscaler uses one global `runscaler-shared`
// volume, so a single sweeper covers all scalesets. No-op when no scaleset
// enables TTL or when the Docker client is unavailable.
func startSharedVolumeCleanup(ctx context.Context, client *dockerclient.Client, scaleSets []config.ScaleSetConfig, logger *slog.Logger) {
	if client == nil {
		return
	}

	var (
		ttl         time.Duration
		interval    time.Duration
		mountPath   string
		helperImage string
	)
	for _, ss := range scaleSets {
		if ss.IsTart() || ss.Docker.SharedVolume == "" || ss.Docker.SharedVolumeTTL <= 0 {
			continue
		}
		ttl = ss.Docker.SharedVolumeTTL
		interval = ss.Docker.SharedVolumeCleanupInterval
		if interval <= 0 {
			interval = config.DefaultSharedVolumeCleanupInterval
		}
		mountPath = ss.Docker.SharedVolume
		helperImage = ss.RunnerImage
		break
	}
	if ttl <= 0 {
		return
	}

	logger.Info("Shared volume TTL cleanup enabled",
		slog.Duration("ttl", ttl),
		slog.Duration("interval", interval),
		slog.String("path", mountPath),
	)

	// Run an initial sweep so users don't wait `interval` for the first cleanup
	// after startup (especially relevant after a crash leaves the volume bloated).
	go func() {
		if err := backend.CleanupSharedVolumeStale(ctx, client, helperImage, mountPath, ttl, logger); err != nil {
			logger.Warn("Initial shared volume cleanup failed", slog.Any("error", err))
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := backend.CleanupSharedVolumeStale(ctx, client, helperImage, mountPath, ttl, logger); err != nil {
					logger.Warn("Periodic shared volume cleanup failed", slog.Any("error", err))
				}
			}
		}
	}()
}

// startBuildxCleanup launches a background goroutine that periodically removes
// orphaned buildx BuildKit builder containers (and their state volumes) from
// the shared Docker daemon. Builders are global to the daemon — like the
// shared volume — so a single sweeper covers all Docker scalesets; the first
// Docker scaleset with cleanup enabled provides the settings. Enabled by
// default; no-op when explicitly disabled or when Docker is unavailable.
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
func listenOnce(ctx context.Context, client *scaleset.Client, scaleSetID int, sessionID string, maxRunners int, s *scaler.Scaler, recorder *metrics.Recorder, logger *slog.Logger) error {
	sessionClient, err := client.MessageSessionClient(ctx, scaleSetID, sessionID)
	if err != nil {
		return fmt.Errorf("failed to create message session: %w", err)
	}
	defer sessionClient.Close(context.Background())

	l, err := listener.New(sessionClient, listener.Config{
		ScaleSetID: scaleSetID,
		MaxRunners: maxRunners,
		Logger:     logger,
	}, listener.WithMetricsRecorder(recorder))
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}

	return l.Run(ctx, s)
}
