package provider

import (
	"context"
	"errors"
	"github.com/ysya/runscaler/internal/config"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTartPreflightKeepsReadyWarmVMUsableBelowFloor(t *testing.T) {
	p := newTestTartProvider(&mockCommandRunner{})
	p.cloneFloor = CloneFloor{MinFreeBytes: 1, FreeBytes: func(string) (uint64, error) { return 0, nil }}
	if err := p.CheckStart(context.Background()); !errors.Is(err, ErrBelowCloneFloor) {
		t.Fatalf("cold start admitted: %v", err)
	}
	p.pool = make(chan *warmVM, 1)
	p.pool <- &warmVM{}
	if err := p.CheckStart(context.Background()); err != nil {
		t.Fatalf("ready warm VM rejected: %v", err)
	}
}
func TestNewTartProvider_CloneMinFreeGB(t *testing.T) {
	ss := config.ScaleSetConfig{
		RunnerImage: "macos-base:latest",
		MaxRunners:  1,
		Tart:        config.TartConfig{CloneMinFreeGB: 170},
	}
	p := NewTartProviderWithCoordinator(ss, slog.New(slog.DiscardHandler), NewTartHostCoordinator(2), TartGuestMacOS)
	if want := uint64(170) << 30; p.cloneFloor.MinFreeBytes != want {
		t.Errorf("cloneFloor.MinFreeBytes = %d, want %d", p.cloneFloor.MinFreeBytes, want)
	}
}

func cloneCalls(cmd *mockCommandRunner) int {
	n := 0
	for _, c := range cmd.getCalls() {
		if c.name == "tart" && len(c.args) > 0 && c.args[0] == "clone" {
			n++
		}
	}
	return n
}

func TestTartProvider_CloneFloor(t *testing.T) {
	const floor = uint64(170) << 30
	cases := []struct {
		name      string
		free      uint64
		readErr   error
		wantClone bool
		wantErr   string
	}{
		{name: "above floor clones", free: floor + 1, wantClone: true},
		{name: "at floor clones", free: floor, wantClone: true},
		{name: "below floor refuses", free: floor - 1, wantErr: "below the floor"},
		{name: "unreadable refuses", readErr: errors.New("statfs: boom"), wantErr: "cannot read free space"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &mockCommandRunner{results: map[string]cmdResult{
				"tart clone": {}, "tart run": {}, "tart exec": {},
			}}
			p := newTestTartProvider(cmd)
			p.home = "/Volumes/VMs/tart"
			var readPath string
			p.cloneFloor = CloneFloor{MinFreeBytes: floor, FreeBytes: func(path string) (uint64, error) {
				readPath = path
				return tc.free, tc.readErr
			}}
			_, err := p.StartInstance(context.Background(), "runner-abc", "jit")
			if readPath != "/Volumes/VMs/tart" {
				t.Errorf("free space read on %q, want TART_HOME", readPath)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("StartInstance() error = %v, want it to contain %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("StartInstance() error: %v", err)
			}
			if got := cloneCalls(cmd) > 0; got != tc.wantClone {
				t.Errorf("clone ran = %v, want %v", got, tc.wantClone)
			}
			// A refusal must release the host permit.
			if tc.wantErr != "" {
				// macOSPermits holds the free permits: all must be back.
				if free, all := len(p.coordinator.macOSPermits), cap(p.coordinator.macOSPermits); free != all {
					t.Errorf("%d of %d macOS permits still held after a refusal", all-free, all)
				}
			}
		})
	}
}

func TestTartProvider_NoFloorNeverReadsDisk(t *testing.T) {
	cmd := &mockCommandRunner{results: map[string]cmdResult{"tart clone": {}, "tart run": {}, "tart exec": {}}}
	p := newTestTartProvider(cmd)
	p.cloneFloor = CloneFloor{FreeBytes: func(string) (uint64, error) {
		t.Fatal("free space read with no floor set")
		return 0, nil
	}}
	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}
}

func TestStatfsFreeBytesReadsARealFilesystem(t *testing.T) {
	free, err := statfsFreeBytes(t.TempDir())
	if err != nil || free == 0 {
		t.Fatalf("statfsFreeBytes(tempdir) = %d, %v; want > 0, nil", free, err)
	}
}

func TestCloneFloor_ReadsTartHomeEnvWhenHomeUnset(t *testing.T) {
	envHome := t.TempDir()
	t.Setenv("TART_HOME", envHome)
	const floor = uint64(170) << 30
	free := map[string]uint64{envHome: 0}
	if userHome, err := os.UserHomeDir(); err == nil {
		free[filepath.Join(userHome, ".tart")] = floor * 2
	}
	var readPath string
	f := CloneFloor{MinFreeBytes: floor, FreeBytes: func(path string) (uint64, error) {
		readPath = path
		v, ok := free[path]
		if !ok {
			return 0, errors.New("unexpected path")
		}
		return v, nil
	}}
	err := f.Check("")
	if readPath != envHome {
		t.Errorf("floor read %q, want $TART_HOME %q", readPath, envHome)
	}
	if !errors.Is(err, ErrBelowCloneFloor) {
		t.Fatalf("Check() = %v, want a refusal (TART_HOME volume has 0 bytes free)", err)
	}

	cmd := &mockCommandRunner{results: map[string]cmdResult{"tart clone": {}, "tart run": {}, "tart exec": {}}}
	p := newTestTartProvider(cmd)
	p.home = ""
	p.cloneFloor = f
	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); !errors.Is(err, ErrBelowCloneFloor) {
		t.Fatalf("StartInstance() = %v, want a floor refusal", err)
	}
	if n := cloneCalls(cmd); n != 0 {
		t.Errorf("%d clone calls below the floor, want 0", n)
	}
}

// The startup guest inspection clones too; it must pass the same floor.
func TestDetectTartGuest_RefusesBelowCloneFloor(t *testing.T) {
	home := t.TempDir()
	cmd := &mockCommandRunner{results: map[string]cmdResult{"tart clone": {}, "tart delete": {}}}
	floor := CloneFloor{MinFreeBytes: 1 << 30, FreeBytes: func(string) (uint64, error) { return 0, nil }}
	_, err := DetectTartGuest(context.Background(), cmd, home, "example/image:latest", floor)
	if !errors.Is(err, ErrBelowCloneFloor) {
		t.Fatalf("DetectTartGuest() = %v, want a floor refusal", err)
	}
	if n := cloneCalls(cmd); n != 0 {
		t.Errorf("%d clone calls below the floor, want 0", n)
	}
}
