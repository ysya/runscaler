package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ysya/runscaler/internal/config"
)

// CommandRunner abstracts shell command execution for testability.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// RunStreaming executes a command with stdout/stderr piped to the terminal.
	// Used for long-running commands where progress output is important (e.g. tart pull).
	RunStreaming(ctx context.Context, name string, args ...string) error
}

// execCommandRunner executes real shell commands via os/exec.
// extraEnv is appended to os.Environ() for every spawned child (e.g. TART_HOME).
type execCommandRunner struct {
	extraEnv []string
}

func (r execCommandRunner) buildCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(r.extraEnv) > 0 {
		cmd.Env = append(os.Environ(), r.extraEnv...)
	}
	return cmd
}

func (r execCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := r.buildCmd(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (r execCommandRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	cmd := r.buildCmd(ctx, name, args...)
	cmd.Stdin = os.Stdin // inherit TTY so child process detects interactive terminal
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// warmVM represents a pre-booted VM ready to accept a runner.
type warmVM struct {
	name    string
	cancel  context.CancelFunc // cancels the `tart run` goroutine
	done    <-chan struct{}    // closed when `tart run` exits (VM died)
	slotIdx int                // index into vmSlots for deterministic MAC assignment
	release sync.Once          // host slot must be returned exactly once
}

// TartHostCoordinator enforces the Apple host-wide VM limit and assigns MAC
// slots uniquely across every Tart-backed scale set in this process.
type TartHostCoordinator struct {
	slots chan int
}

func NewTartHostCoordinator(limit int) *TartHostCoordinator {
	if limit < 1 {
		limit = 1
	}
	c := &TartHostCoordinator{slots: make(chan int, limit)}
	for i := range limit {
		c.slots <- i
	}
	return c
}

func (c *TartHostCoordinator) acquire(ctx context.Context) (int, error) {
	select {
	case idx := <-c.slots:
		return idx, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (c *TartHostCoordinator) release(idx int) { c.slots <- idx }

// TartProvider runs GitHub Actions runners as ephemeral Tart macOS VMs.
//
// Lifecycle per runner:
//  1. tart clone <baseImage> <name>       — APFS CoW clone (< 1 sec)
//  2. tart set <name> --cpu/--memory      — configure VM resources (if specified)
//  3. Set deterministic MAC address       — prevent DHCP lease exhaustion
//  4. tart run <name> --no-graphics       — boot VM in background goroutine
//  5. tart exec <name> true               — poll until Guest Agent is ready
//  6. tart exec <name> ... run.sh         — start runner with JIT config
//  7. tart stop + tart delete on removal
//
// When poolSize > 0, VMs are pre-booted and kept ready in a pool.
// StartInstance picks a warm VM from the pool (near-instant) instead of
// cold-starting one (~30s). The pool is refilled in the background.
type TartProvider struct {
	baseImage   string
	runnerDir   string
	home        string // TART_HOME ("" = tart default ~/.tart)
	cpu         int    // 0 = use image default
	memory      int    // MB, 0 = use image default
	poolSize    int
	maxRunners  int
	logger      *slog.Logger
	cmd         CommandRunner
	coordinator *TartHostCoordinator

	// VM pool
	pool     chan *warmVM
	poolCtx  context.Context
	poolStop context.CancelFunc
	poolWg   sync.WaitGroup

	// vmSlots is a pool of slot indices limiting total concurrent VMs.
	// Each slot has a deterministic MAC address to prevent DHCP lease exhaustion.
	// Apple Silicon enforces a max of 2 concurrent macOS VMs per host.
	activeVMs sync.Map // instanceID -> *warmVM, for exit watching and cleanup
}

// NewTartProvider creates a TartProvider from scale set config.
func NewTartProvider(ss config.ScaleSetConfig, logger *slog.Logger) *TartProvider {
	return NewTartProviderWithCoordinator(ss, logger, NewTartHostCoordinator(2))
}

func NewTartProviderWithCoordinator(ss config.ScaleSetConfig, logger *slog.Logger, coordinator *TartHostCoordinator) *TartProvider {
	if coordinator == nil {
		coordinator = NewTartHostCoordinator(2)
	}
	var extraEnv []string
	if ss.Tart.Home != "" {
		extraEnv = append(extraEnv, "TART_HOME="+ss.Tart.Home)
	}
	p := &TartProvider{
		baseImage:   ss.RunnerImage,
		runnerDir:   ss.Tart.RunnerDir,
		home:        ss.Tart.Home,
		cpu:         ss.Tart.CPU,
		memory:      ss.Tart.Memory,
		poolSize:    ss.Tart.PoolSize,
		maxRunners:  ss.MaxRunners,
		logger:      logger,
		cmd:         execCommandRunner{extraEnv: extraEnv},
		coordinator: coordinator,
	}
	return p
}

// StartPool begins pre-warming VMs in the background.
// Call this after EnsureImage and before the listener starts.
func (p *TartProvider) StartPool(ctx context.Context) {
	if p.poolSize <= 0 {
		return
	}
	p.pool = make(chan *warmVM, p.poolSize)
	p.poolCtx, p.poolStop = context.WithCancel(ctx)

	p.logger.Info("Starting VM warm pool", slog.Int("poolSize", p.poolSize))
	for i := range p.poolSize {
		p.poolWg.Add(1)
		go func() {
			defer p.poolWg.Done()
			p.fillPool(i)
		}()
	}
}

// fillPool creates one warm VM and puts it in the pool.
// When a VM is consumed, the pool refill goroutine creates a replacement.
func (p *TartProvider) fillPool(slot int) {
	for {
		if p.poolCtx.Err() != nil {
			return
		}

		vm, err := p.bootVM(p.poolCtx, fmt.Sprintf("pool-%d-%d", slot, time.Now().UnixMilli()))
		if err != nil {
			if p.poolCtx.Err() != nil {
				return
			}
			p.logger.Warn("Failed to create warm VM, retrying in 5s",
				slog.Int("slot", slot),
				slog.Any("error", err),
			)
			select {
			case <-p.poolCtx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		p.logger.Debug("Warm VM ready",
			slog.String("name", vm.name),
			slog.Int("slot", slot),
		)

		// Put VM in pool; block until consumed, VM dies, or shutdown
		select {
		case p.pool <- vm:
			// VM was consumed by StartInstance, loop to create replacement
		case <-vm.done:
			// VM died while waiting in the pool — clean up and recreate
			p.logger.Warn("Warm VM died in pool, replacing",
				slog.String("name", vm.name),
				slog.Int("slot", slot),
			)
			p.destroyVM(vm)
			// loop back to create a new one
		case <-p.poolCtx.Done():
			// Shutdown — clean up this warm VM
			p.destroyVM(vm)
			return
		}
	}
}

// EnsureImage checks if the base image exists locally, and pulls it if not.
func (p *TartProvider) EnsureImage(ctx context.Context) error {
	// `tart list` outputs one VM per line; check if baseImage is already local
	out, err := p.cmd.Run(ctx, "tart", "list", "--format", "json")
	if err != nil {
		// If list fails, try pulling anyway
		p.logger.Warn("Failed to list local images, will attempt pull", slog.Any("error", err))
	} else if strings.Contains(string(out), p.baseImage) {
		p.logger.Debug("Base image already exists locally", slog.String("image", p.baseImage))
		return nil
	}

	p.logger.Info("Pulling base image (this may take a while on first run)...", slog.String("image", p.baseImage))
	if err := p.cmd.RunStreaming(ctx, "tart", "pull", p.baseImage); err != nil {
		return fmt.Errorf("failed to pull image %s: %w", p.baseImage, err)
	}
	p.logger.Info("Base image pulled successfully", slog.String("image", p.baseImage))
	return nil
}

// bootVM clones and boots a Tart VM, waits for Guest Agent readiness.
// Returns a warmVM that is ready for runner injection via `tart exec`.
func (p *TartProvider) bootVM(ctx context.Context, name string) (*warmVM, error) {
	// 0. Acquire a VM slot (blocks if all slots are in use)
	slotIdx, err := p.coordinator.acquire(ctx)
	if err != nil {
		return nil, err
	}

	releaseSlot := func() { p.coordinator.release(slotIdx) }

	// 1. Clone base image (APFS copy-on-write, near-instant)
	if _, err := p.cmd.Run(ctx, "tart", "clone", p.baseImage, name); err != nil {
		releaseSlot()
		return nil, fmt.Errorf("failed to clone VM: %w", err)
	}

	// 2. Configure VM resources (CPU/memory) if specified
	if p.cpu > 0 || p.memory > 0 {
		args := []string{"set", name}
		if p.cpu > 0 {
			args = append(args, "--cpu", strconv.Itoa(p.cpu))
		}
		if p.memory > 0 {
			args = append(args, "--memory", strconv.Itoa(p.memory))
		}
		if _, err := p.cmd.Run(ctx, "tart", args...); err != nil {
			_, _ = p.cmd.Run(context.WithoutCancel(ctx), "tart", "delete", name)
			releaseSlot()
			return nil, fmt.Errorf("failed to configure VM resources: %w", err)
		}
		p.logger.Debug("Configured VM resources",
			slog.String("name", name),
			slog.Int("cpu", p.cpu),
			slog.Int("memory", p.memory),
		)
	}

	// 3. Set deterministic MAC address to prevent DHCP lease exhaustion.
	//    Each slot always gets the same MAC → same DHCP lease → no IP waste.
	if err := p.setVMMAC(name, slotIdx); err != nil {
		p.logger.Warn("Failed to set fixed MAC (will use random)",
			slog.String("name", name), slog.Any("error", err))
	}

	// 4. Start VM in background (tart run blocks until VM shuts down)
	vmCtx, vmCancel := context.WithCancel(ctx)
	vmDone := make(chan struct{})
	go func() {
		defer vmCancel()
		defer close(vmDone)
		if _, err := p.cmd.Run(vmCtx, "tart", "run", name, "--no-graphics"); err != nil {
			if vmCtx.Err() == nil {
				p.logger.Error("VM exited unexpectedly", slog.String("name", name), slog.Any("error", err))
			}
		}
	}()

	// 5. Wait for Guest Agent to be ready (tart exec <name> true)
	if err := p.waitForExec(ctx, name); err != nil {
		vmCancel()
		_, _ = p.cmd.Run(context.WithoutCancel(ctx), "tart", "stop", name)
		_, _ = p.cmd.Run(context.WithoutCancel(ctx), "tart", "delete", name)
		releaseSlot()
		return nil, fmt.Errorf("guest agent not ready on %s: %w", name, err)
	}

	return &warmVM{name: name, cancel: vmCancel, done: vmDone, slotIdx: slotIdx}, nil
}

// setVMMAC writes a deterministic MAC address to the VM's config.json.
// Uses locally administered unicast addresses (02:00:00:00:00:XX) so
// the DHCP server always assigns the same IP to the same slot index.
func (p *TartProvider) setVMMAC(name string, slotIdx int) error {
	tartHome := p.home
	if tartHome == "" {
		tartHome = os.Getenv("TART_HOME")
	}
	if tartHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("get home dir: %w", err)
		}
		tartHome = filepath.Join(home, ".tart")
	}
	configPath := filepath.Join(tartHome, "vms", name, "config.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read VM config: %w", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse VM config: %w", err)
	}

	mac := fmt.Sprintf("02:00:00:00:00:%02x", slotIdx+1)
	cfg["macAddress"] = mac

	out, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal VM config: %w", err)
	}

	if err := os.WriteFile(configPath, out, 0644); err != nil {
		return fmt.Errorf("write VM config: %w", err)
	}

	p.logger.Debug("Set fixed MAC address",
		slog.String("name", name),
		slog.String("mac", mac),
		slog.Int("slot", slotIdx),
	)
	return nil
}

func vmExited(vm *warmVM) bool {
	select {
	case <-vm.done:
		return true
	default:
		return false
	}
}

func (p *TartProvider) releaseVM(vm *warmVM) {
	vm.release.Do(func() { p.coordinator.release(vm.slotIdx) })
}

// destroyVM stops and deletes a warm VM. The slot is released only after the
// VM is known to be stopped; reusing it after failed cleanup could exceed the
// Apple host-wide two-VM limit.
func (p *TartProvider) destroyVM(vm *warmVM) {
	vm.cancel()
	cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, stopErr := p.cmd.Run(cleanCtx, "tart", "stop", vm.name)
	_, deleteErr := p.cmd.Run(cleanCtx, "tart", "delete", vm.name)
	if stopErr == nil || deleteErr == nil || vmExited(vm) {
		p.releaseVM(vm)
		return
	}
	p.logger.Error("VM cleanup failed; retaining host slot for safety",
		slog.String("name", vm.name),
		slog.Any("stop_error", stopErr),
		slog.Any("delete_error", deleteErr),
	)
	go func() {
		<-vm.done
		p.releaseVM(vm)
	}()
}

// StartInstance starts a GitHub Actions runner in a Tart VM.
// If a warm pool is available, picks a pre-booted VM (near-instant).
// Otherwise, cold-starts a new VM (~30s).
func (p *TartProvider) StartInstance(ctx context.Context, name string, jitConfig string) (string, error) {
	// Try to get a warm VM from the pool.
	// When pool is enabled, we MUST wait for it rather than cold-starting,
	// because pool VMs already hold VM slots — cold-starting would deadlock
	// if all slots are occupied by pool VMs being booted.
	if p.pool != nil {
		for {
			var vm *warmVM
			select {
			case vm = <-p.pool:
			case <-ctx.Done():
				return "", ctx.Err()
			}

			// Check if the VM is still alive (tart run process hasn't exited)
			select {
			case <-vm.done:
				p.logger.Warn("Warm VM already dead, discarding",
					slog.String("vmName", vm.name),
				)
				p.destroyVM(vm)
				continue // wait for next pool VM
			default:
			}

			p.logger.Debug("Using warm VM from pool",
				slog.String("vmName", vm.name),
				slog.String("runner", name),
			)
			if err := p.runRunner(ctx, vm.name, jitConfig); err != nil {
				p.destroyVM(vm)
				return "", fmt.Errorf("failed to start runner: %w", err)
			}
			p.activeVMs.Store(vm.name, vm)
			p.logger.Debug("Runner started (warm)", slog.String("name", vm.name))
			return vm.name, nil
		}
	}

	// Cold start: boot a new VM from scratch
	vm, err := p.bootVM(ctx, name)
	if err != nil {
		return "", err
	}

	if err := p.runRunner(ctx, vm.name, jitConfig); err != nil {
		p.destroyVM(vm)
		return "", fmt.Errorf("failed to start runner: %w", err)
	}

	p.activeVMs.Store(name, vm)
	p.logger.Debug("Runner started (cold)",
		slog.String("name", name),
		slog.String("baseImage", p.baseImage),
	)
	return name, nil
}

// RemoveInstance stops and deletes a Tart VM, releasing its VM slot.
func (p *TartProvider) RemoveInstance(ctx context.Context, instanceID string) error {
	// Use background context for cleanup so it completes even if parent is cancelled
	cleanCtx := context.WithoutCancel(ctx)

	_, stopErr := p.cmd.Run(cleanCtx, "tart", "stop", instanceID)
	if stopErr != nil {
		p.logger.Warn("Failed to stop VM (may already be stopped)", slog.String("name", instanceID), slog.Any("error", stopErr))
	}
	_, deleteErr := p.cmd.Run(cleanCtx, "tart", "delete", instanceID)
	value, tracked := p.activeVMs.Load(instanceID)
	knownStopped := stopErr == nil || deleteErr == nil
	if tracked {
		knownStopped = knownStopped || vmExited(value.(*warmVM))
	}
	if tracked && knownStopped {
		if value, ok := p.activeVMs.LoadAndDelete(instanceID); ok {
			vm := value.(*warmVM)
			vm.cancel()
			p.releaseVM(vm)
		}
	}
	if deleteErr != nil {
		return fmt.Errorf("failed to delete VM %s: %w", instanceID, deleteErr)
	}
	return nil
}

func (p *TartProvider) WaitInstance(ctx context.Context, instanceID string) error {
	value, ok := p.activeVMs.Load(instanceID)
	if !ok {
		return fmt.Errorf("unknown Tart VM %s", instanceID)
	}
	vm := value.(*warmVM)
	select {
	case <-vm.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops the warm pool and cleans up any idle VMs.
func (p *TartProvider) Shutdown(ctx context.Context) {
	if p.poolStop != nil {
		p.poolStop()
		// Drain remaining warm VMs from the pool
		for {
			select {
			case vm := <-p.pool:
				p.logger.Debug("Cleaning up warm VM", slog.String("name", vm.name))
				p.destroyVM(vm)
			default:
				p.poolWg.Wait()
				return
			}
		}
	}
}

// waitForExec polls `tart exec <name> true` until the Guest Agent is ready.
// This replaces the old waitForIP + waitForSSH flow — tart exec uses Virtio
// gRPC, bypassing the network stack entirely.
func (p *TartProvider) waitForExec(ctx context.Context, name string) error {
	timeout := 2 * time.Minute
	deadline := time.Now().Add(timeout)
	wait := 1 * time.Second

	for {
		_, err := p.cmd.Run(ctx, "tart", "exec", name, "true")
		if err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for guest agent on %s: %w", timeout, name, err)
		}

		p.logger.Debug("Guest agent not ready, retrying",
			slog.String("name", name),
			slog.Duration("retry_in", wait),
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}

		wait = min(wait*2, 5*time.Second)
	}
}

// PruneTartCache runs `tart prune --entries caches` against the given TART_HOME,
// trimming OCI/IPSW caches. Local VMs are never touched. Two criteria can apply:
// age (maxAge, via --older-than) reclaims layers left behind by image updates,
// and an optional size cap (spaceBudgetGB, via --space-budget) bounds the total.
// A no-op when both criteria are off (maxAge <= 0 and spaceBudgetGB <= 0).
//
// tartHome may be empty, meaning use tart's default ($HOME/.tart). It is passed
// through TART_HOME so a single binary can prune multiple independent caches.
func PruneTartCache(ctx context.Context, tartHome string, maxAge time.Duration, spaceBudgetGB int, logger *slog.Logger) error {
	if maxAge <= 0 && spaceBudgetGB <= 0 {
		return nil
	}
	var extraEnv []string
	if tartHome != "" {
		extraEnv = append(extraEnv, "TART_HOME="+tartHome)
	}
	return pruneTartCacheWith(ctx, execCommandRunner{extraEnv: extraEnv}, tartHome, maxAge, spaceBudgetGB, logger)
}

// pruneTartCacheWith is the testable core: it issues the prune command via the
// supplied CommandRunner. The exported wrapper above provides the default
// execCommandRunner wired with TART_HOME.
func pruneTartCacheWith(ctx context.Context, runner CommandRunner, tartHome string, maxAge time.Duration, spaceBudgetGB int, logger *slog.Logger) error {
	args := []string{"prune", "--entries", "caches"}
	attrs := []any{slog.String("home", tartHome)}

	if maxAge > 0 {
		// tart --older-than granularity is whole days. Floor sub-day windows to
		// 1 day: --older-than=0 would mean "older than 0 days" and wipe the
		// entire cache, which is never the intent.
		days := int(maxAge.Hours() / 24)
		if days < 1 {
			days = 1
		}
		args = append(args, "--older-than", strconv.Itoa(days))
		attrs = append(attrs, slog.Int("older_than_days", days))
	}
	if spaceBudgetGB > 0 {
		args = append(args, "--space-budget", strconv.Itoa(spaceBudgetGB))
		attrs = append(attrs, slog.Int("space_budget_gb", spaceBudgetGB))
	}
	if len(args) == 3 {
		// No pruning criterion — `tart prune` requires at least one and would
		// error. Treat as a disabled sweep rather than failing.
		return nil
	}

	logger.Info("Pruning Tart cache", attrs...)
	if _, err := runner.Run(ctx, "tart", args...); err != nil {
		return fmt.Errorf("tart prune (home=%q): %w", tartHome, err)
	}
	return nil
}

// runRunner starts the GitHub Actions runner inside the VM via `tart exec`.
// runnerStartCmd builds the shell command that launches the runner from the
// JIT config staged at /tmp/jitconfig.
//
// The read and the delete both happen in the foreground, before the runner is
// backgrounded. Putting `rm` after the `&` instead is a race: everything left
// of `&` runs in an async subshell, so the command substitution reading the
// file can lose to the foreground `rm`. The runner then starts with an empty
// ACTIONS_RUNNER_INPUT_JITCONFIG and dies with "Not configured", leaving the
// job queued forever.
//
// Deleting the file still matters — the JIT config is a registration
// credential that should not outlive startup — so it is removed here, once
// its value is safely held in a shell variable that the background subshell
// inherits.
func runnerStartCmd(runScript, jitPath string) string {
	return fmt.Sprintf(
		"JIT=$(cat %[2]s); rm -f %[2]s; "+
			"ACTIONS_RUNNER_INPUT_JITCONFIG=\"$JIT\" nohup %[1]s > /tmp/runner.log 2>&1 &",
		shellQuote(runScript), shellQuote(jitPath),
	)
}

// jitConfigPath is where the JIT config is staged inside the VM before the
// runner is started. It is deleted as soon as its value has been read.
const jitConfigPath = "/tmp/jitconfig"

func writeJITCmd(path, jitConfig string) string {
	return fmt.Sprintf("umask 077; printf %%s %s > %s", shellQuote(jitConfig), shellQuote(path))
}

// Uses Virtio gRPC (Guest Agent) instead of SSH — no network dependency.
func (p *TartProvider) runRunner(ctx context.Context, vmName, jitConfig string) error {
	// Verify runner binary exists before attempting to start
	runScript := p.runnerDir + "/run.sh"
	if _, err := p.cmd.Run(ctx, "tart", "exec", vmName, "test", "-x", runScript); err != nil {
		return fmt.Errorf("runner not found at %s on %s: %w", runScript, vmName, err)
	}

	// Quote the complete payload instead of embedding it in a heredoc, whose
	// delimiter could otherwise be injected by a malicious response.
	writeJIT := writeJITCmd(jitConfigPath, jitConfig)
	if _, err := p.cmd.Run(ctx, "tart", "exec", vmName, "sh", "-c", writeJIT); err != nil {
		return fmt.Errorf("failed to write JIT config on %s: %w", vmName, err)
	}

	if _, err := p.cmd.Run(ctx, "tart", "exec", vmName, "sh", "-c", runnerStartCmd(runScript, jitConfigPath)); err != nil {
		return fmt.Errorf("failed to start runner on %s: %w", vmName, err)
	}

	// Wait briefly and verify the runner process is still alive
	time.Sleep(2 * time.Second)
	if _, err := p.cmd.Run(ctx, "tart", "exec", vmName, "pgrep", "-f", "Runner.Listener"); err != nil {
		// Grab log output to help diagnose the failure
		logOut, _ := p.cmd.Run(ctx, "tart", "exec", vmName, "tail", "-20", "/tmp/runner.log")
		return fmt.Errorf("runner process died on %s, log:\n%s", vmName, string(logOut))
	}

	return nil
}
