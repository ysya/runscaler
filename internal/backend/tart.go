package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ysya/runscaler/internal/config"
	"golang.org/x/crypto/ssh"
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
	ip      string
	cancel  context.CancelFunc // cancels the `tart run` goroutine
	slotIdx int                // index into vmSlots for deterministic MAC assignment
}

// TartBackend runs GitHub Actions runners as ephemeral Tart macOS VMs.
//
// Lifecycle per runner:
//  1. tart clone <baseImage> <name>   — APFS CoW clone (< 1 sec)
//  2. tart run <name> --no-graphics   — boot VM in background goroutine
//  3. tart ip <name>                  — poll until VM gets an IP
//  4. ssh into VM and start runner with JIT config
//  5. tart stop + tart delete on removal
//
// When poolSize > 0, VMs are pre-booted and kept ready in a pool.
// StartRunner picks a warm VM from the pool (near-instant) instead of
// cold-starting one (~30s). The pool is refilled in the background.
type TartBackend struct {
	baseImage   string
	sshUser     string
	sshPassword string
	runnerDir  string
	poolSize   int
	maxRunners  int
	logger      *slog.Logger
	cmd         CommandRunner
	ssh         sshDialer

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
		baseImage:   ss.TartImage,
		sshUser:     ss.TartSSHUser,
		sshPassword: ss.TartSSHPass,
		runnerDir:   ss.TartRunnerDir,
		poolSize:    ss.TartPoolSize,
		maxRunners:  ss.MaxRunners,
		logger:      logger,
		cmd:     execCommandRunner{},
		ssh:     realSSHDialer{},
		vmSlots: make(chan int, ss.MaxRunners),
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
				slog.String("error", err.Error()),
			)
			select {
			case <-b.poolCtx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		b.logger.Info("Warm VM ready",
			slog.String("name", vm.name),
			slog.String("ip", vm.ip),
			slog.Int("slot", slot),
		)

		// Put VM in pool; block until consumed or shutdown
		select {
		case b.pool <- vm:
			// VM was consumed by StartRunner, loop to create replacement
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
		b.logger.Warn("Failed to list local images, will attempt pull", slog.String("error", err.Error()))
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

// bootVM clones and boots a Tart VM, waits for IP and SSH readiness.
// Returns a warmVM that is ready for runner injection.
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

	// 2. Set deterministic MAC address to prevent DHCP lease exhaustion.
	//    Each slot always gets the same MAC → same DHCP lease → no IP waste.
	if err := b.setVMMAC(name, slotIdx); err != nil {
		b.logger.Warn("Failed to set fixed MAC (will use random)",
			slog.String("name", name), slog.String("error", err.Error()))
	}

	// 3. Start VM in background (tart run blocks until VM shuts down)
	vmCtx, vmCancel := context.WithCancel(ctx)
	runArgs := []string{"run", name, "--no-graphics"}
	go func() {
		defer vmCancel()
		if _, err := b.cmd.Run(vmCtx, "tart", runArgs...); err != nil {
			if vmCtx.Err() == nil {
				b.logger.Error("VM exited unexpectedly", slog.String("name", name), slog.String("error", err.Error()))
			}
		}
	}()

	// 4. Wait for VM to get an IP
	ip, err := b.waitForIP(ctx, name)
	if err != nil {
		vmCancel()
		_, _ = b.cmd.Run(context.WithoutCancel(ctx), "tart", "stop", name)
		_, _ = b.cmd.Run(context.WithoutCancel(ctx), "tart", "delete", name)
		releaseSlot()
		return nil, fmt.Errorf("failed to get VM IP: %w", err)
	}

	// 5. Wait for SSH to be ready (just dial, don't run anything)
	if err := b.waitForSSH(ctx, ip, name); err != nil {
		vmCancel()
		_, _ = b.cmd.Run(context.WithoutCancel(ctx), "tart", "stop", name)
		_, _ = b.cmd.Run(context.WithoutCancel(ctx), "tart", "delete", name)
		releaseSlot()
		return nil, fmt.Errorf("SSH not ready on %s: %w", ip, err)
	}

	return &warmVM{name: name, ip: ip, cancel: vmCancel, slotIdx: slotIdx}, nil
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
	var ip string

	// Try to get a warm VM from the pool
	if b.pool != nil {
		select {
		case vm := <-b.pool:
			b.logger.Info("Using warm VM from pool",
				slog.String("vmName", vm.name),
				slog.String("ip", vm.ip),
				slog.String("runner", name),
			)
			ip = vm.ip
			// Start runner via SSH on the warm VM
			if err := b.runRunnerViaSSH(ctx, ip, vm.name, jitConfig); err != nil {
				b.destroyVM(vm)
				return "", fmt.Errorf("failed to start runner via SSH: %w", err)
			}
			b.activeSlots.Store(vm.name, vm.slotIdx)
			b.logger.Info("Runner started (warm)",
				slog.String("name", vm.name),
				slog.String("ip", ip),
			)
			return vm.name, nil
		default:
			b.logger.Warn("VM pool empty, cold-starting VM", slog.String("runner", name))
		}
	}

	// Cold start: boot a new VM from scratch
	vm, err := b.bootVM(ctx, name)
	if err != nil {
		return "", err
	}

	if err := b.runRunnerViaSSH(ctx, vm.ip, name, jitConfig); err != nil {
		b.destroyVM(vm)
		return "", fmt.Errorf("failed to start runner via SSH: %w", err)
	}

	b.activeSlots.Store(name, vm.slotIdx)
	b.logger.Info("Runner started (cold)",
		slog.String("name", name),
		slog.String("ip", vm.ip),
		slog.String("baseImage", b.baseImage),
	)
	return name, nil
}

// RemoveRunner stops and deletes a Tart VM, releasing its VM slot.
func (b *TartBackend) RemoveRunner(ctx context.Context, resourceID string) error {
	// Use background context for cleanup so it completes even if parent is cancelled
	cleanCtx := context.WithoutCancel(ctx)

	if _, err := b.cmd.Run(cleanCtx, "tart", "stop", resourceID); err != nil {
		b.logger.Warn("Failed to stop VM (may already be stopped)", slog.String("name", resourceID), slog.String("error", err.Error()))
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

// waitForIP polls `tart ip` until the VM has a DHCP-assigned IP address.
func (b *TartBackend) waitForIP(ctx context.Context, name string) (string, error) {
	timeout := 2 * time.Minute
	deadline := time.Now().Add(timeout)
	wait := 1 * time.Second

	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout after %s waiting for VM %s to get an IP", timeout, name)
		}

		out, err := b.cmd.Run(ctx, "tart", "ip", name)
		if err == nil {
			ip := strings.TrimSpace(string(out))
			if ip != "" {
				return ip, nil
			}
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}

		// Exponential backoff: 1s, 2s, 4s, cap at 5s
		wait = min(wait*2, 5*time.Second)
	}
}

// sshDialer abstracts SSH connections for testability.
type sshDialer interface {
	DialAndRun(ctx context.Context, addr, user, password, command string) error
}

// realSSHDialer connects via golang.org/x/crypto/ssh.
type realSSHDialer struct{}

func (realSSHDialer) DialAndRun(ctx context.Context, addr, user, password, command string) error {
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	// Respect context cancellation during dial
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr+":22")
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr+":22", config)
	if err != nil {
		conn.Close()
		return fmt.Errorf("ssh handshake: %w", err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer session.Close()

	// Start the command without waiting for it to finish (runner runs as daemon)
	if err := session.Start(command); err != nil {
		return fmt.Errorf("ssh start command: %w", err)
	}

	return nil
}

// waitForSSH polls until SSH is reachable on the VM.
func (b *TartBackend) waitForSSH(ctx context.Context, ip, name string) error {
	timeout := 60 * time.Second
	deadline := time.Now().Add(timeout)
	wait := 1 * time.Second

	for {
		// Try a simple SSH connection (run "true" as a no-op)
		err := b.ssh.DialAndRun(ctx, ip, b.sshUser, b.sshPassword, "true")
		if err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for SSH on %s: %w", timeout, ip, err)
		}

		b.logger.Debug("SSH not ready, retrying",
			slog.String("name", name),
			slog.String("ip", ip),
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

// runRunnerViaSSH connects to the VM and starts the GitHub Actions runner.
// Assumes SSH is already reachable (use waitForSSH first or retry internally).
func (b *TartBackend) runRunnerViaSSH(ctx context.Context, ip, name, jitConfig string) error {
	remoteCmd := fmt.Sprintf(
		"ACTIONS_RUNNER_INPUT_JITCONFIG=%s nohup %s/run.sh &",
		jitConfig, b.runnerDir,
	)

	// SSH should be ready (either from pool or waitForSSH), but retry briefly just in case
	timeout := 15 * time.Second
	deadline := time.Now().Add(timeout)
	wait := 1 * time.Second

	for {
		err := b.ssh.DialAndRun(ctx, ip, b.sshUser, b.sshPassword, remoteCmd)
		if err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("failed to start runner on %s: %w", ip, err)
		}

		b.logger.Debug("SSH retry for runner start",
			slog.String("name", name),
			slog.String("ip", ip),
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
