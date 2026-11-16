package provider

import (
	"context"
	"encoding/json"
	"github.com/ysya/runscaler/internal/config"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
