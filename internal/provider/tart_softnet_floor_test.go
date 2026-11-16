package provider

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ysya/runscaler/internal/config"
)

func TestNewTartProvider_NetSoftnetFromScaleSetConfig(t *testing.T) {
	ss := config.ScaleSetConfig{
		RunnerImage: "macos-base:latest",
		MaxRunners:  1,
		Tart:        config.TartConfig{RunnerDir: "/Users/admin/actions-runner", NetSoftnet: true},
	}
	p := NewTartProviderWithCoordinator(ss, slog.New(slog.DiscardHandler), NewTartHostCoordinator(2), TartGuestMacOS)
	want := []string{"run", "runner-abc", "--no-graphics", "--net-softnet"}
	if got := p.runArgs("runner-abc"); !slices.Equal(got, want) {
		t.Errorf("runArgs = %q, want %q", got, want)
	}
}

func TestTartProvider_RunArgsWithoutSoftnetKeepNAT(t *testing.T) {
	p := newTestTartProvider(&mockCommandRunner{})
	for _, a := range p.runArgs("runner-abc") {
		if strings.HasPrefix(a, "--net-") {
			t.Errorf("default runArgs carry a network flag %q, want Tart's NAT", a)
		}
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

func TestSoftnetMACAvoidsReservedAndIsStable(t *testing.T) {
	seen := map[string]int{}
	for i := 0; i < 64; i++ {
		m := softnetMAC(i)
		if m == softnetReservedMAC {
			t.Fatalf("softnetMAC(%d) is Softnet's reserved DHCP address", i)
		}
		hw, err := net.ParseMAC(m)
		if err != nil || len(hw) != 6 || hw[0]&1 != 0 || hw[0]&2 == 0 {
			t.Fatalf("softnetMAC(%d) = %q is not a locally administered unicast address", i, m)
		}
		if j, dup := seen[m]; dup {
			t.Fatalf("softnetMAC(%d) = softnetMAC(%d) = %q", i, j, m)
		}
		seen[m] = i
		if again := softnetMAC(i); again != m {
			t.Fatalf("softnetMAC(%d) not stable: %q then %q", i, m, again)
		}
	}
	if natMAC(0) != softnetReservedMAC {
		t.Fatalf("natMAC(0) = %q; this test guards the collision with %q", natMAC(0), softnetReservedMAC)
	}
}

func TestTartProvider_SoftnetWritesSoftnetMAC(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "vms", "runner-abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := newTestTartProvider(&mockCommandRunner{})
	p.home = home
	p.netSoftnet = true
	if err := p.writeVMMAC(context.Background(), "runner-abc", 0, map[string]any{}); err != nil {
		t.Fatalf("writeVMMAC: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "vms", "runner-abc", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg["macAddress"]; got != softnetMAC(0) || got == softnetReservedMAC {
		t.Errorf("macAddress = %v, want softnetMAC(0) = %s", got, softnetMAC(0))
	}
}

func TestNewTartProvider_SoftnetRulesReachTartRun(t *testing.T) {
	ss := config.ScaleSetConfig{
		RunnerImage: "macos-base:latest",
		MaxRunners:  1,
		Tart: config.TartConfig{
			RunnerDir:       "/Users/admin/actions-runner",
			NetSoftnet:      true,
			NetSoftnetAllow: "out proto udp @host port 53,out proto tcp @host port 53",
			NetSoftnetBlock: "out @host",
		},
	}
	p := NewTartProviderWithCoordinator(ss, slog.New(slog.DiscardHandler), NewTartHostCoordinator(2), TartGuestMacOS)
	want := []string{"run", "runner-abc", "--no-graphics", "--net-softnet",
		"--net-softnet-allow=out proto udp @host port 53,out proto tcp @host port 53",
		"--net-softnet-block=out @host"}
	if got := p.runArgs("runner-abc"); !slices.Equal(got, want) {
		t.Errorf("runArgs = %q, want %q", got, want)
	}
}

// With [tart].home empty, tart clone writes under $TART_HOME, so the floor
// must read that filesystem, not $HOME/.tart.
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
