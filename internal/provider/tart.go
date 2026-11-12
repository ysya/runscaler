package provider

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ysya/runscaler/internal/backoff"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/shell"
)

// CommandRunner abstracts shell command execution for testability.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// RunStreaming executes a command with stdout/stderr piped to the terminal.
	// Used for long-running commands where progress output is important (e.g. tart pull).
	RunStreaming(ctx context.Context, name string, args ...string) error
}

// TartCommandRunner runs tart against home, or against tart's default
// ($HOME/.tart) when home is empty.
func TartCommandRunner(home string) CommandRunner {
	if home == "" {
		return execCommandRunner{}
	}
	return execCommandRunner{extraEnv: []string{"TART_HOME=" + home}}
}

// TartVM is one entry of `tart list --format json`.
type TartVM struct {
	Name   string `json:"Name"`
	Source string `json:"Source"` // "local" for VMs, "OCI" for cached images
	State  string `json:"State"`
}

// ListTartVMs lists every VM and cached image visible through runner. extra
// narrows the listing with further `tart list` flags, such as
// "--source", "oci".
func ListTartVMs(ctx context.Context, runner CommandRunner, extra ...string) ([]TartVM, error) {
	out, err := runner.Run(ctx, "tart", append([]string{"list", "--format", "json"}, extra...)...)
	if err != nil {
		return nil, err
	}
	return decodeTartList(out)
}

func decodeTartList(out []byte) ([]TartVM, error) {
	var vms []TartVM
	if err := json.Unmarshal(out, &vms); err != nil {
		return nil, fmt.Errorf("parse tart list: %w", err)
	}
	return vms, nil
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
		// Tart and the guest command can both exit 1; retain stderr to tell them apart.
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitErr.Stderr = bytes.Clone(stderr.Bytes())
		}
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
//     (--net-bridged=<interface> when configured)
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
	retryConfig config.RetryConfig

	// Guest network, applied before each runner starts (see TartConfig).
	mtu        int      // 0 = auto, -1 = off, otherwise a fixed MTU
	dns        []string // guest resolvers; empty = keep what Tart's NAT hands out
	netBridged string   // host interface to bridge VMs to; empty = Tart's NAT
	githubHost string   // the host whose route decides the auto MTU
	mtuLogMu   sync.Mutex
	lastMTULog string // last MTU and reason logged at Info; repeats go to Debug

	// Runner start-up checks; zero means the default (see waitRunnerListening).
	runnerPoll          time.Duration
	runnerListenTimeout time.Duration
	runnerSettle        time.Duration

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

// NewTartProviderWithCoordinator creates a TartProvider from scale set config.
// coordinator is shared by every Tart scale set on the host so MAC slots and
// the two-VM limit are host-wide; nil gets a private one.
func NewTartProviderWithCoordinator(ss config.ScaleSetConfig, logger *slog.Logger, coordinator *TartHostCoordinator) *TartProvider {
	if coordinator == nil {
		coordinator = NewTartHostCoordinator(config.MaxTartVMsPerHost)
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
		cmd:         TartCommandRunner(ss.Tart.Home),
		coordinator: coordinator,
		retryConfig: ss.EffectiveRetry(),
		mtu:         ss.Tart.MTU,
		dns:         ss.Tart.DNS,
		netBridged:  ss.Tart.NetBridged,
		githubHost:  registrationHost(ss.RegistrationURL),
	}
	return p
}

// runArgs is the `tart run` command line that boots a VM.
func (p *TartProvider) runArgs(name string) []string {
	args := []string{"run", name, "--no-graphics"}
	if p.netBridged != "" {
		args = append(args, "--net-bridged="+p.netBridged)
	}
	return args
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
	retry := poolRetry{settings: p.retryConfig}
	for {
		if p.poolCtx.Err() != nil {
			return
		}

		vm, err := p.bootVM(p.poolCtx, poolVMName(slot, time.Now()))
		if err != nil {
			if p.poolCtx.Err() != nil {
				return
			}
			delay := retry.failure()
			p.logger.Warn("Failed to create warm VM; retrying",
				slog.Int("slot", slot),
				slog.Duration("retryIn", delay),
				slog.Any("error", err),
			)
			if backoff.Sleep(p.poolCtx, delay) != nil {
				return
			}
			continue
		}
		retry.reset()

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
	out, err := p.cmd.Run(ctx, "tart", "list", "--format", "json")
	if err == nil {
		var names map[string]bool
		if names, err = tartListNames(out); err == nil && names[p.baseImage] {
			p.logger.Debug("Base image already exists locally", slog.String("image", p.baseImage))
			return nil
		}
	}
	if err != nil {
		// If list fails, try pulling anyway
		p.logger.Warn("Failed to list local images, will attempt pull", slog.Any("error", err))
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
	if err := p.setVMMAC(ctx, name, slotIdx); err != nil {
		if p.netBridged != "" {
			cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_, _ = p.cmd.Run(cleanCtx, "tart", "delete", name)
			cancel()
			releaseSlot()
			return nil, fmt.Errorf("failed to configure bridged VM MAC: %w", err)
		}
		p.logger.Warn("Failed to set fixed MAC (will use random)",
			slog.String("name", name), slog.Any("error", err))
	}

	// 4. Start VM in background (tart run blocks until VM shuts down)
	vmCtx, vmCancel := context.WithCancel(ctx)
	vmDone := make(chan struct{})
	go func() {
		defer vmCancel()
		defer close(vmDone)
		if _, err := p.cmd.Run(vmCtx, "tart", p.runArgs(name)...); err != nil {
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
// NAT keeps its original slot addresses. Bridged addresses incorporate the
// host interface's hardware address so separate hosts do not share that pool.
func (p *TartProvider) setVMMAC(ctx context.Context, name string, slotIdx int) error {
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
	if p.netBridged != "" {
		out, err := p.cmd.Run(ctx, "/sbin/ifconfig", p.netBridged)
		if err != nil {
			return fmt.Errorf("read bridged host interface %q: %w", p.netBridged, err)
		}
		var hostMAC net.HardwareAddr
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "ether" {
				hostMAC, err = net.ParseMAC(fields[1])
				if err != nil {
					return fmt.Errorf("parse bridged host interface MAC: %w", err)
				}
				break
			}
		}
		if len(hostMAC) != 6 || hostMAC[0]&1 != 0 || bytes.Equal(hostMAC, []byte{0, 0, 0, 0, 0, 0}) {
			return fmt.Errorf("bridged host interface %q has no valid unicast Ethernet address", p.netBridged)
		}
		digest := sha256.Sum256(fmt.Appendf(nil, "runscaler-tart/%s/%d", hostMAC, slotIdx))
		address := net.HardwareAddr(digest[:6])
		address[0] = address[0]&0xfe | 0x02 // locally administered, unicast
		mac = address.String()
	}
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
	_, stopErr := p.cmd.Run(ctx, "tart", "stop", instanceID)
	if stopErr != nil {
		p.logger.Warn("Failed to stop VM (may already be stopped)", slog.String("name", instanceID), slog.Any("error", stopErr))
	}
	_, deleteErr := p.cmd.Run(ctx, "tart", "delete", instanceID)
	if tartInstanceAbsent(deleteErr) {
		deleteErr = nil
	}
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

// tartInstanceAbsent recognizes the stable classes of "already gone" errors
// emitted by Tart and its storage layer. CommandRunner preserves stderr in the
// wrapped error, so matching here keeps RemoveInstance idempotent without
// hiding permission, corruption, or cancellation failures.
func tartInstanceAbsent(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") ||
		strings.Contains(message, "does not exist") ||
		strings.Contains(message, "doesn't exist") ||
		strings.Contains(message, "no such virtual machine") ||
		strings.Contains(message, "could not find")
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
// Guest-agent polling after a VM boots: 1s doubling to 5s, for up to two
// minutes.
const (
	guestAgentPollInitial = time.Second
	guestAgentPollMax     = 5 * time.Second
	guestAgentTimeout     = 2 * time.Minute
)

func (p *TartProvider) waitForExec(ctx context.Context, name string) error {
	deadline := time.Now().Add(guestAgentTimeout)
	for attempt := 1; ; attempt++ {
		_, err := p.cmd.Run(ctx, "tart", "exec", name, "true")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for guest agent on %s: %w", guestAgentTimeout, name, err)
		}
		wait := backoff.Exponential(attempt, guestAgentPollInitial, guestAgentPollMax)
		p.logger.Debug("Guest agent not ready, retrying",
			slog.String("name", name),
			slog.Duration("retry_in", wait),
		)
		if err := backoff.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// tartListNames returns the names in `tart list --format json` output. Match
// against these rather than the raw output: tart's JSON escapes "/", so an
// OCI reference never appears verbatim, and a substring test would also take
// "macos-base-old" for "macos-base".
func tartListNames(out []byte) (map[string]bool, error) {
	entries, err := decodeTartList(out)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name] = true
	}
	return names, nil
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
		shell.Quote(runScript), shell.Quote(jitPath),
	)
}

// jitConfigPath is where the JIT config is staged inside the VM before the
// runner is started. It is deleted as soon as its value has been read.
const jitConfigPath = "/tmp/jitconfig"

func writeJITCmd(path, jitConfig string) string {
	return fmt.Sprintf("umask 077; printf %%s %s > %s", shell.Quote(jitConfig), shell.Quote(path))
}

// Uses Virtio gRPC (Guest Agent) instead of SSH — no network dependency.
func (p *TartProvider) runRunner(ctx context.Context, vmName, jitConfig string) error {
	// Before the runner first dials GitHub: a pooled VM may have booted while
	// the host's VPN was in a different state.
	p.configureGuestNetwork(ctx, vmName)

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

	// Hand the VM over only once the runner holds a session with GitHub. A
	// runner that dies or never connects leaves the VM running, so nothing
	// else would notice while its job stays queued.
	return p.waitRunnerListening(ctx, vmName)
}

// defaultRunnerPoll spaces the readiness checks while a runner starts. Each
// one is a `tart exec` round trip, so Tart polls less often than Docker.
const defaultRunnerPoll = 2 * time.Second

// waitRunnerListening polls until the runner prints "Listening for Jobs",
// then checks that it survives its first poll for messages. Errors carry the
// runner's own explanation, since the VM is deleted right after.
func (p *TartProvider) waitRunnerListening(ctx context.Context, vmName string) error {
	poll := cmp.Or(p.runnerPoll, defaultRunnerPoll)
	timeout := cmp.Or(p.runnerListenTimeout, config.DefaultReadyTimeout)
	deadline := time.Now().Add(timeout)
	for {
		if err := backoff.Sleep(ctx, poll); err != nil {
			return err
		}
		alive, err := p.runnerAlive(ctx, vmName)
		if err != nil {
			return fmt.Errorf("runner readiness check failed on %s: %w", vmName, err)
		}
		if !alive {
			// Guest-agent startup and Runner.Listener creation are asynchronous.
			// A missing process before any log exists is not evidence of failure.
			if log := p.runnerLogTail(ctx, vmName); strings.TrimSpace(log) != "" {
				return fmt.Errorf("runner process exited on %s before connecting to GitHub; runner log:\n%s", vmName, log)
			}
		}
		if _, err := p.cmd.Run(ctx, "tart", "exec", vmName, "grep", "-q", readyMarker, "/tmp/runner.log"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("runner on %s did not connect to GitHub within %s; newest runner diagnostics:\n%s",
				vmName, timeout, p.runnerDiagTail(ctx, vmName))
		}
	}

	if err := backoff.Sleep(ctx, cmp.Or(p.runnerSettle, defaultReadySettle)); err != nil {
		return err
	}
	alive, err := p.runnerAlive(ctx, vmName)
	if err != nil {
		return fmt.Errorf("runner readiness check failed on %s after connecting to GitHub: %w", vmName, err)
	}
	if !alive {
		return fmt.Errorf("runner process exited on %s right after connecting to GitHub; runner log:\n%s",
			vmName, p.runnerLogTail(ctx, vmName))
	}
	return nil
}

func (p *TartProvider) runnerAlive(ctx context.Context, vmName string) (bool, error) {
	_, err := p.cmd.Run(ctx, "tart", "exec", vmName, "pgrep", "-f", "Runner.Listener")
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == 1 && len(bytes.TrimSpace(exitErr.Stderr)) == 0 {
			return false, nil
		}
		return false, err
	}
	if marker := strings.LastIndex(err.Error(), "exit status "); marker >= 0 {
		fields := strings.Fields(err.Error()[marker+len("exit status "):])
		if len(fields) > 0 {
			codeText := strings.TrimSuffix(fields[0], ":")
			if code, parseErr := strconv.Atoi(codeText); parseErr == nil && code == 1 && len(fields) == 1 {
				return false, nil
			}
		}
	}
	return false, err
}

func (p *TartProvider) runnerLogTail(ctx context.Context, vmName string) string {
	out, _ := p.cmd.Run(ctx, "tart", "exec", vmName, "tail", "-20", "/tmp/runner.log")
	return strings.TrimSpace(string(out))
}

// runnerDiagTail returns the end of the newest Runner_*.log, where the
// runner records the connection failures it never prints to its stdout.
func (p *TartProvider) runnerDiagTail(ctx context.Context, vmName string) string {
	dir := p.runnerDir + "/_diag"
	out, err := p.cmd.Run(ctx, "tart", "exec", vmName, "ls", "-t", dir)
	if err != nil {
		return fmt.Sprintf("(could not list %s: %v)", dir, err)
	}
	for _, name := range strings.Fields(string(out)) { // ls -t lists newest first
		if strings.HasPrefix(name, "Runner_") && strings.HasSuffix(name, ".log") {
			tail, _ := p.cmd.Run(ctx, "tart", "exec", vmName, "tail", "-n", "20", dir+"/"+name)
			return strings.TrimSpace(string(tail))
		}
	}
	return fmt.Sprintf("(no Runner_*.log in %s)", dir)
}
