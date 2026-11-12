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
	"runtime"
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

	"github.com/ysya/runscaler/internal/actionscache"
	"github.com/ysya/runscaler/internal/backoff"
	"github.com/ysya/runscaler/internal/cachestore"
	"github.com/ysya/runscaler/internal/capacity"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/controller"
	"github.com/ysya/runscaler/internal/diagnostics"
	"github.com/ysya/runscaler/internal/diskguard"
	"github.com/ysya/runscaler/internal/health"
	"github.com/ysya/runscaler/internal/layout"
	runnerlock "github.com/ysya/runscaler/internal/lock"
	"github.com/ysya/runscaler/internal/metrics"
	"github.com/ysya/runscaler/internal/provider"
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

	// Per-scaleset (also used by flat single mode)
	flags.String("url", "", "Registration URL (e.g. https://github.com/org)")
	flags.String("name", "", "Name of the scale set (also used as runs-on label)")
	flags.String("token", "", "Personal access token")
	registerGitHubAppFlags(flags)
	flags.Int("max-runners", config.DefaultMaxRunners, "Maximum number of runners")
	flags.Int("min-runners", 0, "Minimum number of runners")
	flags.StringSlice("labels", nil, "Runner labels (comma-separated)")
	flags.String("runner-group", config.DefaultRunnerGroup, "Runner group name")
	flags.String("runner-image", config.DefaultRunnerImage, "Docker image for runners")
	flags.String("provider", config.DefaultProvider, "Instance provider (docker or tart)")

	// Docker provider
	flags.String("docker-socket", config.DefaultDockerSocket, "Path to Docker socket")
	flags.Bool("dind", config.DefaultDinD, "Mount Docker socket into runner containers (Docker-in-Docker)")
	flags.String("shared-volume", "", "Shared Docker volume mounted into all runners (container path, e.g. /shared)")
	flags.Int("docker-memory", 0, "Memory limit in MB for each Docker runner container (0 = unlimited)")
	flags.Int("docker-cpu", 0, "CPU cores for each Docker runner container (0 = unlimited)")
	flags.String("docker-platform", "", "Force container platform (e.g. linux/amd64)")

	// Tart provider
	flags.String("tart-runner-dir", "", "Runner binary path inside VM")
	flags.Int("tart-cpu", 0, "Number of CPU cores for each VM (0 = use image default)")
	flags.Int("tart-memory", 0, "Memory in MB for each VM (0 = use image default)")
	flags.Int("tart-pool-size", 0, "Number of pre-warmed VMs to keep ready (0 = disabled)")
	flags.String("tart-net-bridged", "", "Host interface to bridge each VM to instead of Tart's NAT (e.g. en0)")

	// Global
	flags.String("log-level", config.DefaultLogLevel, "Log level (debug, info, warn, error)")
	flags.String("log-format", config.DefaultLogFormat, "Log format (text, json)")
	flags.Int("concurrent", 0, "Maximum runner instances across all scale sets (0 = sum of max-runners)")

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
	viper.BindPFlag("log-level", flags.Lookup("log-level"))
	viper.BindPFlag("log-format", flags.Lookup("log-format"))
	viper.BindPFlag("concurrent", flags.Lookup("concurrent"))
	viper.BindPFlag("dry-run", flags.Lookup("dry-run"))
	viper.BindPFlag("health-port", flags.Lookup("health-port"))
	viper.BindPFlag("health-address", flags.Lookup("health-address"))

	// Nested keys (flag name → nested viper key for provider sub-structs):
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
	viper.BindPFlag("tart.net-bridged", flags.Lookup("tart-net-bridged"))

	// Register subcommands
	cmd.AddCommand(validateCmd, statusCmd, doctorCmd, versionCmd, serviceCmd, cacheCmd, runCommand)
}

func main() {
	ensureHomebrewPath()
	os.Exit(execute(cmd, os.Stderr))
}

// execute runs root and returns the exit status. Root silences Cobra's own
// error output, so this is the one place a failure is reported.
func execute(root *cobra.Command, stderr io.Writer) int {
	if err := root.Execute(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// startManager loads config, sets up signal handling, and runs the runner manager.
// Used by `runner run`. var (not func) so
// tests can stub it.
var startManager = func(cmd *cobra.Command) error {
	// Runtime failures are not usage errors; the full flag list buried the
	// actual cause in service logs.
	cmd.SilenceUsage = true
	if err := refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "run runner", pathExists); err != nil {
		return err
	}

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
		return explainLockError(err, currentServiceFixCommand())
	}
	defer releaseLock()

	runCtx, cancelRun := context.WithCancel(cmd.Context())
	defer cancelRun()

	drain := make(chan struct{})
	sigCh := make(chan os.Signal, 3)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(sigCh)

	signalCtx, stopSignals := context.WithCancel(cmd.Context())
	defer stopSignals()
	go handleShutdownSignals(signalCtx, sigCh, func() { close(drain) }, cancelRun, func() {
		fmt.Fprintln(os.Stderr, "\nForce exit")
		os.Exit(1)
	})

	return runManager(runCtx, cfg, drain)
}

// explainLockError adds the operator action to lock failures that have one.
func explainLockError(err error, fix string) error {
	switch {
	case errors.Is(err, runnerlock.ErrAlreadyRunning):
		return fmt.Errorf("%w\n\n  Only one runner may run per machine.\n  To manage multiple organizations, use multiple [[scaleset]] entries in one config", err)
	case errors.Is(err, syscall.EROFS):
		// A binary update cannot rewrite an installed unit, and system units
		// from older releases keep /tmp read-only under ProtectSystem=strict.
		return fmt.Errorf("%w\n\n  The service sandbox keeps /tmp read-only (a unit from an older release). Regenerate it:\n    %s", err, fix)
	default:
		return err
	}
}

// handleShutdownSignals implements the three-stage shutdown sequence.
// SIGINT means stop now by terminal convention. SIGTERM and SIGQUIT request
// a drain; a second signal cancels normal work immediately, and a third
// bypasses cleanup entirely. forceExit is injected so the sequence is
// testable without terminating the test process.
func handleShutdownSignals(ctx context.Context, signals <-chan os.Signal, requestDrain, cancelRun, forceExit func()) {
	next := func() (os.Signal, bool) {
		select {
		case <-ctx.Done():
			return nil, false
		case sig, ok := <-signals:
			return sig, ok
		}
	}

	first, ok := next()
	if !ok {
		return
	}
	if first == os.Interrupt {
		cancelRun()
	} else {
		requestDrain()
	}

	if _, ok := next(); !ok {
		return
	}
	cancelRun()

	if _, ok := next(); !ok {
		return
	}
	forceExit()
}

var cmd = &cobra.Command{
	Use:     "runner",
	Version: version,
	Short:   "GitHub Actions Runner Manager",
	Long: `Dynamically scales GitHub Actions self-hosted runners as Docker containers
or Tart VMs using the actions/scaleset library. Runners are ephemeral — each
handles one job and is removed upon completion.

Supports multiple scale sets via [[scaleset]] entries in TOML config,
or a single scale set via 'runner run' CLI flags.`,
	Example: `  # Build config.toml at https://runscaler.ysya.dev/configure/
  runner validate --config config.toml   # Verify configuration
  runner run --config config.toml        # Start scaling

  # Using CLI flags
  runner run --url https://github.com/org --name my-runners --token ghp_xxx`,
	// execute prints a returned error once; Cobra's own copy would repeat it.
	SilenceErrors: true,
	// Cobra parses flags and checks arguments before this hook, so those
	// mistakes still print usage. A later error is a runtime failure, and the
	// flag list would bury its cause. Cobra checks required flags and flag
	// groups after this hook, and a subcommand's own PersistentPreRun
	// replaces it.
	PersistentPreRun: func(c *cobra.Command, _ []string) {
		c.SilenceUsage = true
	},
	RunE: func(cmd *cobra.Command, args []string) error {
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
		return startManager(cmd)
	},
}

func runManager(ctx context.Context, cfg config.Config, drain <-chan struct{}) error {
	if err := cfg.ValidateGlobal(); err != nil {
		return fmt.Errorf("invalid global configuration: %w", err)
	}
	id := layout.CurrentIdentity()
	logFile, openedLog, logWarnings := openRunLog(id, resolveLogFile(cfg, id))
	for _, w := range logWarnings {
		fmt.Fprintln(os.Stderr, "Warning: "+w)
	}
	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}
	console := logConsole(stdoutIsDevNull(), logFile != nil)
	logger := config.NewLoggerWithWriter(cfg.LogLevel, cfg.LogFormat, console, logWriter(logFile))
	var diagnosticStore *diagnostics.Store
	if cfg.Diagnostics.IsEnabled() {
		if dir, err := diagnosticDirectory(id, openedLog); err == nil {
			diagnosticStore = diagnostics.NewStore(dir, cfg.Diagnostics.CaptureLogs)
		} else {
			logger.Warn("Failure diagnostics unavailable: cannot resolve log directory")
		}
	}

	// A self-updated deployment with an older config must keep starting.
	// Strict warnings fail `runner validate`; deprecations remain advisory.
	for _, w := range cfg.Warnings {
		logger.Warn(w)
	}
	for _, d := range cfg.Deprecations {
		logger.Warn(d)
	}
	warnOutdatedService(logger, cfg.EffectiveDrainTimeout())

	scaleSets := cfg.ResolveScaleSets()
	if err := checkScaleSets(cfg, scaleSets); err != nil {
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
	var tartCoordinator *provider.TartHostCoordinator
	// Verify Tart binary exists if any scale set uses the Tart provider.
	if needsTart {
		tartCoordinator = provider.NewTartHostCoordinator(config.MaxTartVMsPerHost)
		if _, err := exec.LookPath("tart"); err != nil {
			return fmt.Errorf("tart binary not found in PATH: %w\n\n"+
				"  Install Tart: brew install openai/tools/tart", err)
		}
		logger.Info("Tart provider enabled")

		// Ensure Tart images are available locally (auto-pull if missing)
		pulled := make(map[string]bool)
		for _, ss := range scaleSets {
			if !ss.IsTart() || pulled[ss.RunnerImage] {
				continue
			}
			tb := provider.NewTartProviderWithCoordinator(ss, logger, tartCoordinator)
			if err := tb.EnsureImage(ctx); err != nil {
				return err
			}
			pulled[ss.RunnerImage] = true
		}
	}

	if cfg.DryRun {
		logger.Info("Dry run complete — configuration and connectivity are valid",
			slog.Int("scaleSetCount", len(scaleSets)),
		)
		return nil
	}

	// The machine-wide lock is acquired by startManager before runManager is called,
	// so resources created by runner but still present now belong to a dead
	// previous process. Reconcile them before any new scale set can start.
	// Dry-run returns above because it must not perform destructive cleanup.
	var tartHomes []string
	var workspaceRoots []string
	for _, ss := range scaleSets {
		if ss.IsTart() {
			tartHomes = append(tartHomes, ss.Tart.Home)
		} else if ss.Docker.WorkspaceRoot != "" {
			workspaceRoots = append(workspaceRoots, ss.Docker.WorkspaceRoot)
		}
	}
	reconcileOrphans(ctx, dockerClients, tartHomes, logger, workspaceRoots...)

	// Keep the local Results receiver available until every scale set finishes
	// draining. Its separate listener has no streaming write timeout.
	var nativeCache *actionscache.Service
	if cfg.ActionsCache.Enabled {
		var err error
		nativeCache, err = actionscache.NewService(cfg.ActionsCache, logger)
		if err != nil {
			return fmt.Errorf("native cache startup: %w", err)
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := nativeCache.Shutdown(shutdownCtx); err != nil {
				logger.Error("Native cache shutdown failed", slog.Any("error", err))
			}
		}()
		if err := nativeCache.Start(ctx); err != nil {
			return fmt.Errorf("native cache startup: %w", err)
		}
		logger.Info("Native Actions cache started", slog.String("address", cfg.ActionsCache.ListenAddress))
	}

	// Start health check server
	var healthServer *health.HealthServer
	if cfg.HealthPort > 0 {
		healthServer = health.NewHealthServer(cfg.HealthPort, version, logger)
		if nativeCache != nil {
			healthServer.SetCacheProvider(func() health.CacheStatus {
				status := nativeCache.Status()
				return health.CacheStatus{Enabled: true, Ready: status.Ready, Degraded: status.Degraded, Entries: status.Entries, Bytes: status.Bytes, RetentionTargetBytes: cfg.ActionsCache.MaxSizeBytes}
			})
		}
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
	// (once for the guard's flat list, once more for sweeper-wiring) would
	// also log every "Conflicting ... settings" warning twice at
	// startup. buildCacheStores (used by callers outside runManager(),
	// e.g. `runner cache`) is intentionally not called here for the same
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
	// its pre-job-start check (see runScaleSetController, controller.WithDiskChecker)
	// instead of constructing a second one.
	guard := startDiskGuard(ctx, cacheStores, cfg, logger)
	capacityLimit := cfg.EffectiveConcurrent(scaleSets)
	capacityBroker := capacity.NewBroker(capacityLimit)
	capacityAllocators := make([]*capacity.Allocator, len(scaleSets))
	for i, ss := range scaleSets {
		// Include the config index because scale-set names are only guaranteed
		// unique within GitHub registration scope, not necessarily process-wide.
		capacityAllocators[i] = capacityBroker.Register(fmt.Sprintf("%d:%s", i, ss.ScaleSetName), ss.MinRunners)
	}
	if healthServer != nil {
		healthServer.SetCapacityProvider(func() health.CapacityStatus {
			return health.CapacityStatus{Limit: capacityBroker.Limit(), InUse: capacityBroker.InUse()}
		})
	}

	logger.Info("Starting scale sets",
		slog.Int("count", len(scaleSets)),
		slog.Int("concurrent", capacityLimit),
	)

	// Non-blocking version check at startup
	go func() {
		release, err := versioncheck.Latest(ctx)
		if err != nil {
			return
		}
		if versioncheck.IsNewer(version, release.TagName) {
			logAvailableUpdate(logger, version, release.TagName)
		}
	}()

	// Run each scale set in its own goroutine
	var wg sync.WaitGroup
	errs := make(chan error, len(scaleSets)+1)
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var cacheMonitorDone <-chan struct{}
	if nativeCache != nil {
		cacheMonitorDone = watchNativeCacheFailure(runCtx, nativeCache.Failures(), cancelRun, errs)
	}

	for i, ss := range scaleSets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ssLogger := config.NewScaleSetLoggerWithWriter(cfg.LogLevel, cfg.LogFormat, ss.ScaleSetName, i, console, logWriter(logFile))
			if err := runScaleSetController(runCtx, drain, cfg.EffectiveDrainTimeout(), ss, dockerClients[ss.Docker.Socket], ssLogger, healthServer, tartCoordinator, guard, capacityAllocators[i], diagnosticStore, cfg.Diagnostics.CaptureLogs); err != nil {
				errs <- fmt.Errorf("scaleset %q: %w", ss.ScaleSetName, err)
				cancelRun()
			}
		}()
	}

	wg.Wait()
	cancelRun()
	if cacheMonitorDone != nil {
		<-cacheMonitorDone
	}
	close(errs)

	// Nothing is reclaimed here at exit, by design. Every store has a
	// lifecycle that outlives this process: the shared volume holds handoff
	// data for runs still in flight across a restart, and images, build
	// cache and cache volumes belong to the daemon or to the next job.
	// Reclamation belongs to the periodic sweepers and the disk guard
	// above, which is also what keeps a self-update from deleting anything
	// a workflow is mid-way through using.

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

func watchNativeCacheFailure(ctx context.Context, failures <-chan error, cancelRun context.CancelFunc, errs chan<- error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case err, ok := <-failures:
			if ok && err != nil {
				errs <- fmt.Errorf("native cache listener: %w", err)
				cancelRun()
			}
		case <-ctx.Done():
		}
	}()
	return done
}

// runScaleSetController manages the lifecycle of a single scale set. guard is the
// shared disk-pressure guard built once in runManager() (nil when the disk guard
// is disabled); it is wired into this ScaleSetController as its
// pre-job-start check, see controller.WithDiskChecker below.
func runScaleSetController(ctx context.Context, drain <-chan struct{}, drainTimeout time.Duration, ss config.ScaleSetConfig, dockerClient *dockerclient.Client, logger *slog.Logger, h *health.HealthServer, tartCoordinator *provider.TartHostCoordinator, guard *diskguard.Guard, capacityAllocator *capacity.Allocator, diagnosticStore *diagnostics.Store, captureLogs bool) error {
	// Create scaleset client
	scalesetClient, err := ss.NewScalesetClient(logger)
	if err != nil {
		return fmt.Errorf("failed to create scaleset client: %w", err)
	}

	// The scale set is registered once and kept across restarts (see
	// ensureScaleSet), so it is deliberately not deleted on exit.
	scaleSet, err := ensureScaleSet(ctx, scalesetClient, ss, logger)
	if err != nil {
		return err
	}

	// Set user agent info
	scalesetClient.SetSystemInfo(config.NewSystemInfo(scaleSet.ID, version))

	// Create the instance provider once; it persists across reconnections.
	var instanceProvider provider.InstanceProvider
	if ss.IsTart() {
		tb := provider.NewTartProviderWithCoordinator(ss, logger, tartCoordinator)
		tb.StartPool(ctx) // starts warm pool if tart-pool-size > 0
		instanceProvider = tb
	} else {
		dp := provider.NewDockerProvider(ss, dockerClient, logger)
		dp.EnableDiagnosticLogs(captureLogs && diagnosticStore != nil)
		instanceProvider = dp
	}

	// guard is a *diskguard.Guard; only wrap it into the controller.DiskChecker
	// option when non-nil so a disabled guard doesn't become a non-nil
	// interface holding a nil pointer (which would panic the first time
	// startInstance called NeedsReclaim on it).
	var controllerOpts []controller.Option
	if guard != nil {
		controllerOpts = append(controllerOpts, controller.WithDiskChecker(guard))
	}
	controllerOpts = append(controllerOpts, controller.WithCapacityAllocator(capacityAllocator))
	controllerOpts = append(controllerOpts, controller.WithDiagnostics(diagnosticStore, ss.ScaleSetName, ss.Provider, ss.Token), controller.WithRetry(ss.EffectiveRetry()))
	s := controller.NewScaleSetController(scaleSet.ID, ss.MinRunners, ss.MaxRunners, instanceProvider, scalesetClient, logger, controllerOpts...)
	defer s.Shutdown(context.WithoutCancel(ctx))

	// The listener must keep delivering JobCompleted messages while Drain
	// waits. Only cancel its context after Drain returns (or immediately when
	// draining is explicitly disabled); otherwise busy runners can never reach
	// zero and every graceful stop degenerates into a timeout.
	listenCtx, cancelListen := context.WithCancel(ctx)
	defer cancelListen()
	go watchScaleSetDrain(listenCtx, drain, drainTimeout, s, cancelListen, logger)

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
		h.RegisterController(ss.ScaleSetName, s)
		h.RegisterMetrics(ss.ScaleSetName, recorder)
		defer h.UnregisterController(ss.ScaleSetName)
	}

	// Session ID for message session
	hostname, err := os.Hostname()
	if err != nil {
		hostname = uuid.NewString()
	}
	sessionID := fmt.Sprintf("%s-%s", hostname, ss.ScaleSetName)

	// Reconnection loop: recreate session + listener on transient failures
	var reconnect listenerBackoff

	for {
		logger.Info("Listening for jobs",
			slog.Int("maxRunners", ss.MaxRunners),
			slog.Int("minRunners", ss.MinRunners),
		)

		listenStart := time.Now()
		if h != nil {
			h.MarkDisconnected(ss.ScaleSetName, "connecting")
		}
		listenErr := listenOnce(listenCtx, scalesetClient, scaleSet.ID, sessionID, ss.MaxRunners, s, recorder, logger, func() {
			if h != nil {
				h.MarkConnected(ss.ScaleSetName)
			}
		})
		if listenCtx.Err() != nil || errors.Is(listenErr, context.Canceled) {
			// Clean exit: the parent context was canceled (SIGTERM/SIGINT or
			// another scale set encountered a permanent failure).
			return nil
		}
		if listenErr == nil {
			return fmt.Errorf("listener stopped unexpectedly")
		}

		// A listener that stayed connected for a while failed afresh, so the
		// ramp starts over and the reconnect is quick.
		delay := reconnect.next(time.Since(listenStart), listenErr)
		logger.Warn("Listener disconnected, will reconnect",
			slog.Any("error", listenErr),
			slog.Duration("backoff", delay),
		)
		if h != nil {
			h.MarkDisconnected(ss.ScaleSetName, listenErr.Error())
		}
		if backoff.Sleep(listenCtx, delay) != nil {
			return nil
		}
	}
}

// Listener reconnects: 5s doubling to a minute, starting over once a
// connection has stayed up longer than the cap. After a rate limit the wait
// is at least what GitHub asked for, a minute when it gave no usable wait,
// and at most an hour.
const (
	listenerReconnectInitial = 5 * time.Second
	listenerReconnectMax     = time.Minute
	listenerRateLimitDefault = time.Minute
	listenerRateLimitMax     = time.Hour
)

type listenerBackoff struct{ failures int }

// next returns how long to wait before reconnecting a listener that had
// been connected for connectedFor and then failed with err.
func (b *listenerBackoff) next(connectedFor time.Duration, err error) time.Duration {
	if connectedFor > listenerReconnectMax {
		b.failures = 0
	}
	b.failures++
	delay := backoff.Exponential(b.failures, listenerReconnectInitial, listenerReconnectMax)
	var rateLimited *scaleset.RateLimitError
	if errors.As(err, &rateLimited) {
		delay = max(delay, rateLimitWait(rateLimited))
	}
	return delay
}

// rateLimitWait is how long GitHub asked runner to stay away. Without a
// usable wait it is a minute, as the SDK advises for secondary limits; the
// cap keeps a bad header from parking the listener indefinitely.
func rateLimitWait(rl *scaleset.RateLimitError) time.Duration {
	if rl.RetryAfter <= 0 {
		return listenerRateLimitDefault
	}
	return min(rl.RetryAfter, listenerRateLimitMax)
}

type drainer interface {
	Drain(context.Context) error
}

// watchScaleSetDrain bridges the process-wide drain broadcast to one controller.
// It deliberately leaves listenCtx alive during Drain so the listener can
// report job completions. Once draining finishes or times out, cancelListen
// ends the listener and runScaleSetController's existing Shutdown defer removes any
// resources that remain.
func watchScaleSetDrain(listenCtx context.Context, drain <-chan struct{}, timeout time.Duration, d drainer, cancelListen context.CancelFunc, logger *slog.Logger) {
	select {
	case <-listenCtx.Done():
		return
	case <-drain:
	}

	if timeout <= 0 {
		logger.Info("Runner drain disabled; stopping immediately")
		cancelListen()
		return
	}

	drainCtx, cancelDrain := context.WithTimeout(listenCtx, timeout)
	err := d.Drain(drainCtx)
	cancelDrain()
	if errors.Is(err, context.DeadlineExceeded) {
		logger.Warn("Runner drain timed out; forcing remaining runners to stop",
			slog.Duration("timeout", timeout))
	} else if err != nil && !errors.Is(err, context.Canceled) {
		logger.Warn("Runner drain ended with an error; forcing remaining runners to stop",
			slog.Any("error", err))
	}
	cancelListen()
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

// checkScaleSets runs the offline checks every scale set must pass before
// runner starts it: each scale set's own settings, the host-wide capacity
// floor, unique names and Tart's host-wide VM limit. `runner run`, `runner
// validate` and service preparation all call it, so a config that validates
// or installs also starts.
func checkScaleSets(cfg config.Config, scaleSets []config.ScaleSetConfig) error {
	for i := range scaleSets {
		if err := scaleSets[i].Validate(); err != nil {
			return fmt.Errorf("scaleset[%d] %q: %w", i, scaleSets[i].ScaleSetName, err)
		}
		if scaleSets[i].Provider == config.DefaultProvider && scaleSets[i].Docker.NativeActionsCache {
			if !cfg.ActionsCache.Enabled {
				return fmt.Errorf("scaleset[%d] %q: docker.native-actions-cache requires actions-cache.enabled", i, scaleSets[i].ScaleSetName)
			}
			if scaleSets[i].Docker.ActionsCacheURL != cfg.ActionsCache.AdvertiseURL {
				return fmt.Errorf("scaleset[%d] %q: docker.native-actions-cache requires docker.actions-cache-url to match actions-cache.advertise-url", i, scaleSets[i].ScaleSetName)
			}
		}
	}
	if err := cfg.ValidateConcurrent(scaleSets); err != nil {
		return fmt.Errorf("invalid global capacity: %w", err)
	}
	return validateScaleSetCollection(scaleSets)
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
	if totalTartMin > config.MaxTartVMsPerHost {
		return fmt.Errorf("combined Tart min-runners is %d, exceeding the host-wide macOS VM limit of %d", totalTartMin, config.MaxTartVMsPerHost)
	}
	if totalTartPool > config.MaxTartVMsPerHost {
		return fmt.Errorf("combined Tart pool-size is %d, exceeding the host-wide macOS VM limit of %d", totalTartPool, config.MaxTartVMsPerHost)
	}
	return nil
}

// listenOnce creates a message session and listener, then runs until
// disconnection or context cancellation. Callers retry on transient errors.
func listenOnce(ctx context.Context, client *scaleset.Client, scaleSetID int, sessionID string, maxRunners int, s *controller.ScaleSetController, recorder *metrics.Recorder, logger *slog.Logger, onReady func()) error {
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
	})
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}
	if onReady != nil {
		onReady()
	}

	return l.Run(ctx, controller.NewMessageHandler(sessionClient, s, recorder, logger))
}
