package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (execCommandRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
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
	done    <-chan struct{}     // closed when `tart run` exits (VM died)
	slotIdx int                // index into vmSlots for deterministic MAC assignment
}

// TartBackend runs GitHub Actions runners as ephemeral Tart macOS VMs.
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
// StartRunner picks a warm VM from the pool (near-instant) instead of
// cold-starting one (~30s). The pool is refilled in the background.
type TartBackend struct {
	baseImage  string
	runnerDir  string
	cpu        int // 0 = use image default
	memory     int // MB, 0 = use image default
	poolSize   int
	maxRunners int
	logger     *slog.Logger
	cmd        CommandRunner

	// VM pool
	pool     chan *warmVM
	poolCtx  context.Context
	poolStop context.CancelFunc
	poolWg   sync.WaitGroup

	// vmSlots is a pool of slot indices limiting total concurrent VMs.
	// Each slot has a deterministic MAC address to prevent DHCP lease exhaustion.
	// Apple Silicon enforces a max of 2 concurrent macOS VMs per host.
	vmSlots     chan int
	activeSlots sync.Map // resourceID -> slotIdx, for releasing on RemoveRunner
}

// NewTartBackend creates a TartBackend from scale set config.
func NewTartBackend(ss config.ScaleSetConfig, logger *slog.Logger) *TartBackend {
	b := &TartBackend{
		baseImage:  ss.Tart.Image,
		runnerDir:  ss.Tart.RunnerDir,
		cpu:        ss.Tart.CPU,
		memory:     ss.Tart.Memory,
		poolSize:   ss.Tart.PoolSize,
		maxRunners: ss.MaxRunners,
		logger:     logger,
		cmd:        execCommandRunner{},
		vmSlots:    make(chan int, ss.MaxRunners),
	}
	// Pre-fill slot indices: each slot gets a deterministic MAC address
	for i := 0; i < ss.MaxRunners; i++ {
		b.vmSlots <- i
	}
	return b
}

// StartPool begins pre-warming VMs in the background.
// Call this after EnsureImage and before the listener starts.
func (b *TartBackend) StartPool(ctx context.Context) {
	if b.poolSize <= 0 {
		return
	}
	b.pool = make(chan *warmVM, b.poolSize)
	b.poolCtx, b.poolStop = context.WithCancel(ctx)

	b.logger.Info("Starting VM warm pool", slog.Int("poolSize", b.poolSize))
	for i := 0; i < b.poolSize; i++ {
		b.poolWg.Add(1)
		go func(idx int) {
			defer b.poolWg.Done()
			b.fillPool(idx)
		}(i)
	}
}

// fillPool creates one warm VM and puts it in the pool.
// When a VM is consumed, the pool refill goroutine creates a replacement.
func (b *TartBackend) fillPool(slot int) {
	for {
		if b.poolCtx.Err() != nil {
			return
		}

		vm, err := b.bootVM(b.poolCtx, fmt.Sprintf("pool-%d-%d", slot, time.Now().UnixMilli()))
		if err != nil {
			if b.poolCtx.Err() != nil {
				return
			}
			b.logger.Warn("Failed to create warm VM, retrying in 5s",
				slog.Int("slot", slot),
				slog.Any("error", err),
			)
			select {
			case <-b.poolCtx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		b.logger.Debug("Warm VM ready",
			slog.String("name", vm.name),
			slog.Int("slot", slot),
		)

		// Put VM in pool; block until consumed, VM dies, or shutdown
		select {
		case b.pool <- vm:
			// VM was consumed by StartRunner, loop to create replacement
		case <-vm.done:
			// VM died while waiting in the pool — clean up and recreate
			b.logger.Warn("Warm VM died in pool, replacing",
				slog.String("name", vm.name),
				slog.Int("slot", slot),
			)
			b.destroyVM(vm)
			// loop back to create a new one
		case <-b.poolCtx.Done():
			// Shutdown — clean up this warm VM
			b.destroyVM(vm)
			return
		}
	}
}

// EnsureImage checks if the base image exists locally, and pulls it if not.
func (b *TartBackend) EnsureImage(ctx context.Context) error {
	// `tart list` outputs one VM per line; check if baseImage is already local
	out, err := b.cmd.Run(ctx, "tart", "list", "--format", "json")
	if err != nil {
		// If list fails, try pulling anyway
		b.logger.Warn("Failed to list local images, will attempt pull", slog.Any("error", err))
	} else if strings.Contains(string(out), b.baseImage) {
		b.logger.Debug("Base image already exists locally", slog.String("image", b.baseImage))
		return nil
	}

	b.logger.Info("Pulling base image (this may take a while on first run)...", slog.String("image", b.baseImage))
	if err := b.cmd.RunStreaming(ctx, "tart", "pull", b.baseImage); err != nil {
		return fmt.Errorf("failed to pull image %s: %w", b.baseImage, err)
	}
	b.logger.Info("Base image pulled successfully", slog.String("image", b.baseImage))
	return nil
}

// bootVM clones and boots a Tart VM, waits for Guest Agent readiness.
// Returns a warmVM that is ready for runner injection via `tart exec`.
func (b *TartBackend) bootVM(ctx context.Context, name string) (*warmVM, error) {
	// 0. Acquire a VM slot (blocks if all slots are in use)
	var slotIdx int
	select {
	case slotIdx = <-b.vmSlots:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	releaseSlot := func() { b.vmSlots <- slotIdx }

	// 1. Clone base image (APFS copy-on-write, near-instant)
	if _, err := b.cmd.Run(ctx, "tart", "clone", b.baseImage, name); err != nil {
		releaseSlot()
		return nil, fmt.Errorf("failed to clone VM: %w", err)
	}

	// 2. Configure VM resources (CPU/memory) if specified
	if b.cpu > 0 || b.memory > 0 {
		args := []string{"set", name}
		if b.cpu > 0 {
			args = append(args, "--cpu", fmt.Sprintf("%d", b.cpu))
		}
		if b.memory > 0 {
			args = append(args, "--memory", fmt.Sprintf("%d", b.memory))
		}
		if _, err := b.cmd.Run(ctx, "tart", args...); err != nil {
			releaseSlot()
			return nil, fmt.Errorf("failed to configure VM resources: %w", err)
		}
		b.logger.Debug("Configured VM resources",
			slog.String("name", name),
			slog.Int("cpu", b.cpu),
			slog.Int("memory", b.memory),
		)
	}

	// 3. Set deterministic MAC address to prevent DHCP lease exhaustion.
	//    Each slot always gets the same MAC → same DHCP lease → no IP waste.
	if err := b.setVMMAC(name, slotIdx); err != nil {
		b.logger.Warn("Failed to set fixed MAC (will use random)",
			slog.String("name", name), slog.Any("error", err))
	}

	// 4. Start VM in background (tart run blocks until VM shuts down)
	vmCtx, vmCancel := context.WithCancel(ctx)
	vmDone := make(chan struct{})
	go func() {
		defer vmCancel()
		defer close(vmDone)
		if _, err := b.cmd.Run(vmCtx, "tart", "run", name, "--no-graphics"); err != nil {
			if vmCtx.Err() == nil {
				b.logger.Error("VM exited unexpectedly", slog.String("name", name), slog.Any("error", err))
			}
		}
	}()

	// 5. Wait for Guest Agent to be ready (tart exec <name> true)
	if err := b.waitForExec(ctx, name); err != nil {
		vmCancel()
		_, _ = b.cmd.Run(context.WithoutCancel(ctx), "tart", "stop", name)
		_, _ = b.cmd.Run(context.WithoutCancel(ctx), "tart", "delete", name)
		releaseSlot()
		return nil, fmt.Errorf("guest agent not ready on %s: %w", name, err)
	}

	return &warmVM{name: name, cancel: vmCancel, done: vmDone, slotIdx: slotIdx}, nil
}

// setVMMAC writes a deterministic MAC address to the VM's config.json.
// Uses locally administered unicast addresses (02:00:00:00:00:XX) so
// the DHCP server always assigns the same IP to the same slot index.
func (b *TartBackend) setVMMAC(name string, slotIdx int) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}
	configPath := filepath.Join(home, ".tart", "vms", name, "config.json")

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

	b.logger.Debug("Set fixed MAC address",
		slog.String("name", name),
		slog.String("mac", mac),
		slog.Int("slot", slotIdx),
	)
	return nil
}

// destroyVM stops and deletes a warm VM, releasing its VM slot.
func (b *TartBackend) destroyVM(vm *warmVM) {
	vm.cancel()
	cleanCtx := context.Background()
	_, _ = b.cmd.Run(cleanCtx, "tart", "stop", vm.name)
	_, _ = b.cmd.Run(cleanCtx, "tart", "delete", vm.name)
	b.vmSlots <- vm.slotIdx
}

// StartRunner starts a GitHub Actions runner in a Tart VM.
// If a warm pool is available, picks a pre-booted VM (near-instant).
// Otherwise, cold-starts a new VM (~30s).
func (b *TartBackend) StartRunner(ctx context.Context, name string, jitConfig string) (string, error) {
	// Try to get a warm VM from the pool.
	// When pool is enabled, we MUST wait for it rather than cold-starting,
	// because pool VMs already hold VM slots — cold-starting would deadlock
	// if all slots are occupied by pool VMs being booted.
	if b.pool != nil {
		for {
			var vm *warmVM
			select {
			case vm = <-b.pool:
			case <-ctx.Done():
				return "", ctx.Err()
			}

			// Check if the VM is still alive (tart run process hasn't exited)
			select {
			case <-vm.done:
				b.logger.Warn("Warm VM already dead, discarding",
					slog.String("vmName", vm.name),
				)
				b.destroyVM(vm)
				continue // wait for next pool VM
			default:
			}

			b.logger.Debug("Using warm VM from pool",
				slog.String("vmName", vm.name),
				slog.String("runner", name),
			)
			if err := b.runRunner(ctx, vm.name, jitConfig); err != nil {
				b.destroyVM(vm)
				return "", fmt.Errorf("failed to start runner: %w", err)
			}
			b.activeSlots.Store(vm.name, vm.slotIdx)
			b.logger.Debug("Runner started (warm)", slog.String("name", vm.name))
			return vm.name, nil
		}
	}

	// Cold start: boot a new VM from scratch
	vm, err := b.bootVM(ctx, name)
	if err != nil {
		return "", err
	}

	if err := b.runRunner(ctx, vm.name, jitConfig); err != nil {
		b.destroyVM(vm)
		return "", fmt.Errorf("failed to start runner: %w", err)
	}

	b.activeSlots.Store(name, vm.slotIdx)
	b.logger.Debug("Runner started (cold)",
		slog.String("name", name),
		slog.String("baseImage", b.baseImage),
	)
	return name, nil
}

// RemoveRunner stops and deletes a Tart VM, releasing its VM slot.
func (b *TartBackend) RemoveRunner(ctx context.Context, resourceID string) error {
	// Use background context for cleanup so it completes even if parent is cancelled
	cleanCtx := context.WithoutCancel(ctx)

	if _, err := b.cmd.Run(cleanCtx, "tart", "stop", resourceID); err != nil {
		b.logger.Warn("Failed to stop VM (may already be stopped)", slog.String("name", resourceID), slog.Any("error", err))
	}
	if _, err := b.cmd.Run(cleanCtx, "tart", "delete", resourceID); err != nil {
		return fmt.Errorf("failed to delete VM %s: %w", resourceID, err)
	}
	// Release VM slot back to the pool
	if idx, ok := b.activeSlots.LoadAndDelete(resourceID); ok {
		b.vmSlots <- idx.(int)
	}
	return nil
}

// Shutdown stops the warm pool and cleans up any idle VMs.
func (b *TartBackend) Shutdown(ctx context.Context) {
	if b.poolStop != nil {
		b.poolStop()
		// Drain remaining warm VMs from the pool
		for {
			select {
			case vm := <-b.pool:
				b.logger.Debug("Cleaning up warm VM", slog.String("name", vm.name))
				b.destroyVM(vm)
			default:
				b.poolWg.Wait()
				return
			}
		}
	}
}

// waitForExec polls `tart exec <name> true` until the Guest Agent is ready.
// This replaces the old waitForIP + waitForSSH flow — tart exec uses Virtio
// gRPC, bypassing the network stack entirely.
func (b *TartBackend) waitForExec(ctx context.Context, name string) error {
	timeout := 2 * time.Minute
	deadline := time.Now().Add(timeout)
	wait := 1 * time.Second

	for {
		_, err := b.cmd.Run(ctx, "tart", "exec", name, "true")
		if err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for guest agent on %s: %w", timeout, name, err)
		}

		b.logger.Debug("Guest agent not ready, retrying",
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

// runRunner starts the GitHub Actions runner inside the VM via `tart exec`.
// Uses Virtio gRPC (Guest Agent) instead of SSH — no network dependency.
func (b *TartBackend) runRunner(ctx context.Context, vmName, jitConfig string) error {
	// Verify runner binary exists before attempting to start
	runScript := b.runnerDir + "/run.sh"
	if _, err := b.cmd.Run(ctx, "tart", "exec", vmName, "test", "-x", runScript); err != nil {
		return fmt.Errorf("runner not found at %s on %s: %w", runScript, vmName, err)
	}

	// Write JIT config to a temp file to avoid shell argument length limits
	writeJIT := fmt.Sprintf("cat > /tmp/jitconfig <<'JITEOF'\n%s\nJITEOF", jitConfig)
	if _, err := b.cmd.Run(ctx, "tart", "exec", vmName, "sh", "-c", writeJIT); err != nil {
		return fmt.Errorf("failed to write JIT config on %s: %w", vmName, err)
	}

	// Start runner in background, reading JIT config from file
	startCmd := fmt.Sprintf(
		"ACTIONS_RUNNER_INPUT_JITCONFIG=$(cat /tmp/jitconfig) nohup %s > /tmp/runner.log 2>&1 &",
		runScript,
	)
	if _, err := b.cmd.Run(ctx, "tart", "exec", vmName, "sh", "-c", startCmd); err != nil {
		return fmt.Errorf("failed to start runner on %s: %w", vmName, err)
	}

	// Wait briefly and verify the runner process is still alive
	time.Sleep(2 * time.Second)
	if _, err := b.cmd.Run(ctx, "tart", "exec", vmName, "pgrep", "-f", "Runner.Listener"); err != nil {
		// Grab log output to help diagnose the failure
		logOut, _ := b.cmd.Run(ctx, "tart", "exec", vmName, "tail", "-20", "/tmp/runner.log")
		return fmt.Errorf("runner process died on %s, log:\n%s", vmName, string(logOut))
	}

	return nil
}
