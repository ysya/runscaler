package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ysya/runscaler/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cloningRunner mirrors the observable part of tart clone for guest detection.
type cloningRunner struct {
	home          string
	config        string
	cloneErr      error
	afterCloneErr error
	onClone       func()
	calls         []cmdCall
	deleteCtxErr  error
}

func (r *cloningRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, cmdCall{name: name, args: args})
	switch {
	case name == "tart" && len(args) == 3 && args[0] == "clone":
		if r.cloneErr != nil {
			return nil, r.cloneErr
		}
		if r.config != "" {
			dir := filepath.Join(r.home, "vms", args[2])
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(r.config), 0644); err != nil {
				return nil, err
			}
		}
		if r.onClone != nil {
			r.onClone()
		}
		return nil, r.afterCloneErr
	case name == "tart" && len(args) == 2 && args[0] == "delete":
		r.deleteCtxErr = ctx.Err()
	}
	return nil, nil
}

func (r *cloningRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	_, err := r.Run(ctx, name, args...)
	return err
}

func TestDetectTartGuest(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		want    TartGuest
		wantErr bool
	}{
		{name: "macOS image", config: `{"os":"darwin"}`, want: TartGuestMacOS},
		{name: "linux image", config: `{"os":"linux"}`, want: TartGuestLinux},
		{name: "image without os", config: `{"cpuCount":4}`, want: TartGuestMacOS},
		{name: "unknown os", config: `{"os":"windows"}`, wantErr: true},
		{name: "malformed config", config: "invalid", wantErr: true},
		{name: "no config file", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &cloningRunner{home: t.TempDir(), config: tt.config}
			got, err := DetectTartGuest(context.Background(), r, r.home, "example/image:latest")
			if (err != nil) != tt.wantErr || (err == nil && got != tt.want) {
				t.Fatalf("got %v, %v; want %v, error %v", got, err, tt.want, tt.wantErr)
			}
			if len(r.calls) == 0 {
				t.Fatal("clone was not called")
			}
			clone := r.calls[0]
			if clone.name != "tart" || len(clone.args) != 3 || clone.args[0] != "clone" || clone.args[1] != "example/image:latest" || !keepVMNamePattern.MatchString(clone.args[2]) {
				t.Fatalf("first call = %v, want a clone into a keep-marker VM", clone)
			}
			if callIndex(r.calls, "tart", "delete", clone.args[2]) < 0 {
				t.Errorf("throwaway VM %s was not deleted; calls: %v", clone.args[2], r.calls)
			}
		})
	}
}

func TestDetectTartGuest_CloneFailureNamesTheImage(t *testing.T) {
	r := &cloningRunner{home: t.TempDir(), cloneErr: errors.New("no space left")}
	_, err := DetectTartGuest(context.Background(), r, r.home, "example/image:latest")
	if err == nil || !strings.Contains(err.Error(), "example/image:latest") {
		t.Fatalf("error = %v, want it to name the image", err)
	}
	if len(r.calls) != 2 || callIndex(r.calls, "tart", "delete", r.calls[0].args[2]) < 0 {
		t.Fatalf("failed clone must still be cleaned up: %v", r.calls)
	}
}

func TestDetectTartGuest_DeletesTheCloneAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &cloningRunner{home: t.TempDir(), config: `{"os":"linux"}`, onClone: cancel}
	_, _ = DetectTartGuest(ctx, r, r.home, "example/image:latest")
	if len(r.calls) < 2 || callIndex(r.calls, "tart", "delete", r.calls[0].args[2]) < 0 || r.deleteCtxErr != nil {
		t.Fatalf("delete must run with a live context after cancellation; calls: %v, ctx error: %v", r.calls, r.deleteCtxErr)
	}
}

func TestTartImagePresent(t *testing.T) {
	cmd := &mockCommandRunner{results: map[string]cmdResult{"tart list --format json": {output: []byte(tartListWithLocalVMs)}}}
	for image, want := range map[string]bool{"ios-golden": true, "ghcr.io/cirruslabs/macos-golden-gate-xcode:27": true, "macos-base": false} {
		if got, err := TartImagePresent(context.Background(), cmd, image); err != nil || got != want {
			t.Errorf("TartImagePresent(%q) = %v, %v; want %v", image, got, err, want)
		}
	}
	failing := &mockCommandRunner{results: map[string]cmdResult{"tart list --format json": {err: errors.New("tart list failed")}}}
	if _, err := TartImagePresent(context.Background(), failing, "ios-golden"); err == nil {
		t.Error("a failed listing must be an error")
	}
}

func TestTartGuestZeroValueIsMacOS(t *testing.T) {
	var g TartGuest
	if g != TartGuestMacOS || g.String() != "macos" || TartGuestLinux.String() != "linux" {
		t.Fatalf("zero value = %v (%q), linux = %q", g, g, TartGuestLinux)
	}
}

func TestTartGuestFromConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     map[string]any
		want    TartGuest
		wantErr string
	}{
		{name: "darwin", cfg: map[string]any{"os": "darwin"}, want: TartGuestMacOS},
		{name: "linux", cfg: map[string]any{"os": "linux"}, want: TartGuestLinux},
		{name: "missing os is macOS, as Tart decodes it", cfg: map[string]any{"cpuCount": 4.0}, want: TartGuestMacOS},
		{name: "unknown os", cfg: map[string]any{"os": "windows"}, wantErr: `"windows"`},
		{name: "os is not a string", cfg: map[string]any{"os": 5.0}, wantErr: "os"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tartGuestFromConfig(tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %s", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestTartHomeDir(t *testing.T) {
	t.Setenv("TART_HOME", "/env/tart")
	if got, err := TartHomeDir("/explicit"); err != nil || got != "/explicit" {
		t.Errorf("explicit home = %q, %v", got, err)
	}
	if got, err := TartHomeDir(""); err != nil || got != "/env/tart" {
		t.Errorf("TART_HOME = %q, %v", got, err)
	}
	t.Setenv("TART_HOME", "")
	t.Setenv("HOME", "/Users/tester")
	if got, err := TartHomeDir(""); err != nil || got != "/Users/tester/.tart" {
		t.Errorf("default = %q, %v", got, err)
	}
}

func TestReadTartVMConfig(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "vms", "runner-abc")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"os":"linux","cpuCount":4}`), 0644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := readTartVMConfig(home, "runner-abc"); err != nil || cfg["os"] != "linux" {
		t.Fatalf("config = %v, %v", cfg, err)
	}
	if _, err := readTartVMConfig(home, "missing"); err == nil {
		t.Error("a missing VM must be an error")
	}
	if err := os.WriteFile(path, []byte("invalid"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTartVMConfig(home, "runner-abc"); err == nil {
		t.Error("malformed config must be an error")
	}
	if err := os.WriteFile(path, []byte("null"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTartVMConfig(home, "runner-abc"); err == nil {
		t.Error("null config must be an error")
	}
}

func writeTestVMConfigOS(t *testing.T, p *TartProvider, name, guestOS string) {
	t.Helper()
	if p.home == "" {
		p.home = t.TempDir()
	}
	dir := filepath.Join(p.home, "vms", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf(`{"os":%q,"macAddress":"52:54:00:12:34:56","cpuCount":4}`, guestOS)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func newLinuxTestTartProvider(cmd *mockCommandRunner, vmLimit int) *TartProvider {
	p := newTestTartProvider(cmd)
	p.guest = TartGuestLinux
	p.vmSlots = make(chan struct{}, vmLimit)
	return p
}

func TestNewTartProvider_VMBoundOnlyForLinuxGuests(t *testing.T) {
	ss := config.ScaleSetConfig{RunnerImage: "ubuntu-runner", MaxRunners: 3, Tart: config.TartConfig{PoolSize: 1}}
	logger := slog.New(slog.DiscardHandler)
	if p := NewTartProviderWithCoordinator(ss, logger, NewTartHostCoordinator(2), TartGuestLinux); cap(p.vmSlots) != 4 {
		t.Errorf("Linux VM bound = %d, want max-runners + pool-size = 4", cap(p.vmSlots))
	}
	if p := NewTartProviderWithCoordinator(ss, logger, NewTartHostCoordinator(2), TartGuestMacOS); p.vmSlots != nil {
		t.Error("macOS providers must keep today's bound and get no VM semaphore")
	}
}

func TestTartProvider_LinuxGuestStartsWhileMacOSPermitsAreHeld(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostNoVPN)
	p := newLinuxTestTartProvider(cmd, 1)
	p.coordinator = NewTartHostCoordinator(1)
	if _, err := p.coordinator.acquire(context.Background(), TartGuestMacOS); err != nil {
		t.Fatal(err)
	}
	writeTestVMConfigOS(t, p, "runner-abc", "linux")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := p.StartInstance(ctx, "runner-abc", "jit"); err != nil {
		t.Fatalf("a Linux guest waited for a macOS permit: %v", err)
	}
}

func TestTartProvider_GuestChangeFailsBeforeRun(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostNoVPN)
	p := newLinuxTestTartProvider(cmd, 1)
	writeTestVMConfigOS(t, p, "runner-abc", "darwin")

	_, err := p.StartInstance(context.Background(), "runner-abc", "jit")
	if err == nil || !strings.Contains(err.Error(), "changed guest OS") {
		t.Fatalf("error = %v, want a guest change", err)
	}
	calls := cmd.getCalls()
	if cmd.callCount("tart run") != 0 || callIndex(calls, "tart", "delete", "runner-abc") < 0 {
		t.Fatalf("the clone must be deleted and never run; calls: %v", calls)
	}
	if len(p.vmSlots) != 0 {
		t.Error("the failed start kept its VM slot")
	}
}

func TestTartProvider_LinuxStartFailsWhenCloneConfigIsUnreadable(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostNoVPN)
	p := newLinuxTestTartProvider(cmd, 1)
	p.home = t.TempDir() // the fake clone writes no config.json

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err == nil {
		t.Fatal("a Linux-detected start must fail when its guest cannot be confirmed")
	}
	if cmd.callCount("tart run") != 0 || len(p.vmSlots) != 0 {
		t.Fatalf("the VM ran or kept its slot; calls: %v", cmd.getCalls())
	}
}

func TestTartProvider_LinuxVMBoundIsReturnedOnRemove(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostNoVPN)
	cmd.results["tart stop"] = cmdResult{}
	cmd.results["tart delete"] = cmdResult{}
	p := newLinuxTestTartProvider(cmd, 1)
	writeTestVMConfigOS(t, p, "runner-a", "linux")
	writeTestVMConfigOS(t, p, "runner-b", "linux")

	if _, err := p.StartInstance(context.Background(), "runner-a", "jit"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.StartInstance(ctx, "runner-b", "jit"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second VM must wait for the bound, got %v", err)
	}
	if err := p.RemoveInstance(context.Background(), "runner-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.StartInstance(context.Background(), "runner-b", "jit"); err != nil {
		t.Fatalf("the slot was not returned on remove: %v", err)
	}
}

func TestTartProvider_LinuxVMSlotIsHeldUntilTheVMStops(t *testing.T) {
	cmd := &mockCommandRunner{results: map[string]cmdResult{
		"tart stop":   {err: errors.New("stop failed")},
		"tart delete": {err: errors.New("delete failed")},
	}}
	p := newLinuxTestTartProvider(cmd, 1)
	p.vmSlots <- struct{}{}
	slot, _ := p.coordinator.acquire(context.Background(), TartGuestLinux)
	done := make(chan struct{})
	vm := &warmVM{name: "runner-abc", cancel: func() {}, done: done, slot: slot}

	p.destroyVM(vm)
	if len(p.vmSlots) != 1 {
		t.Fatal("a VM whose cleanup failed must keep its place")
	}
	close(done)
	for deadline := time.Now().Add(time.Second); len(p.vmSlots) != 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if len(p.vmSlots) != 0 {
		t.Fatal("the place was not returned after the VM stopped")
	}
}

// One active and one warm VM, as a macOS scale set with max-runners = 1 and
// pool-size = 1 runs today.
func TestTartProvider_MacOSProviderMayHoldBothPermitsWithMaxRunnersOne(t *testing.T) {
	ss := config.ScaleSetConfig{RunnerImage: "macos-image", MaxRunners: 1, Tart: config.TartConfig{PoolSize: 1}}
	p := NewTartProviderWithCoordinator(ss, slog.New(slog.DiscardHandler), NewTartHostCoordinator(2), TartGuestMacOS)
	p.cmd = networkCmdRunner(routeGetHostNoVPN)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, name := range []string{"runner-a", "pool-0-1"} {
		if _, err := p.bootVM(ctx, name); err != nil {
			t.Fatalf("%s could not boot beside the other VM: %v", name, err)
		}
	}
}

func TestNewTartProvider_RunnerDirDefaultsByGuest(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	host := NewTartHostCoordinator(2)
	ss := config.ScaleSetConfig{RunnerImage: "image", MaxRunners: 1}
	if p := NewTartProviderWithCoordinator(ss, logger, host, TartGuestMacOS); p.runnerDir != "/Users/admin/actions-runner" {
		t.Errorf("macOS default = %q", p.runnerDir)
	}
	if p := NewTartProviderWithCoordinator(ss, logger, host, TartGuestLinux); p.runnerDir != "/home/admin/actions-runner" {
		t.Errorf("Linux default = %q", p.runnerDir)
	}
	ss.Tart.RunnerDir = "/opt/runner"
	for _, guest := range []TartGuest{TartGuestMacOS, TartGuestLinux} {
		if p := NewTartProviderWithCoordinator(ss, logger, host, guest); p.runnerDir != "/opt/runner" {
			t.Errorf("%v guest: explicit runner-dir lost, got %q", guest, p.runnerDir)
		}
	}
}

func TestDetectTartGuest_DeletesPartialCloneAfterFailure(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%v", cancelled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cloneErr := errors.New("post-clone prune failed")
			r := &cloningRunner{home: t.TempDir(), config: `{"os":"linux"}`, afterCloneErr: cloneErr}
			if cancelled {
				r.onClone = cancel
			}
			_, err := DetectTartGuest(ctx, r, r.home, "example/image:latest")
			if !errors.Is(err, cloneErr) {
				t.Fatalf("original clone error lost: %v", err)
			}
			if len(r.calls) < 2 || callIndex(r.calls, "tart", "delete", r.calls[0].args[2]) < 0 || r.deleteCtxErr != nil {
				t.Fatalf("partial clone must be deleted with a live context: calls=%v, ctx=%v", r.calls, r.deleteCtxErr)
			}
		})
	}
}

func TestTartProvider_RemoveFailureReleasesLinuxBoundWhenVMStops(t *testing.T) {
	cmd := &mockCommandRunner{results: map[string]cmdResult{
		"tart stop":   {err: errors.New("stop failed")},
		"tart delete": {err: errors.New("delete failed")},
	}}
	p := newLinuxTestTartProvider(cmd, 1)
	p.vmSlots <- struct{}{}
	slot, _ := p.coordinator.acquire(context.Background(), TartGuestLinux)
	done := make(chan struct{})
	vm := &warmVM{name: "runner-abc", cancel: func() {}, done: done, slot: slot}
	p.activeVMs.Store(vm.name, vm)
	if err := p.RemoveInstance(context.Background(), vm.name); err == nil {
		t.Fatal("cleanup failure was hidden")
	}
	if len(p.vmSlots) != 1 {
		t.Fatal("unconfirmed VM stop released the bound")
	}
	close(done)
	deadline := time.Now().Add(time.Second)
	for len(p.vmSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(p.vmSlots) != 0 {
		t.Fatal("known stopped VM kept the Linux bound after failed removal")
	}
}
