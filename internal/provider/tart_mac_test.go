package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testHostMAC = "00:11:22:33:44:55"

func writeTestVMConfig(t *testing.T, p *TartProvider, name string) string {
	t.Helper()
	if p.home == "" {
		p.home = t.TempDir()
	}
	path := filepath.Join(p.home, "vms", name, "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"macAddress":"52:54:00:12:34:56","cpuCount":4}`), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTestVMMAC(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MAC string `json:"macAddress"`
		CPU int    `json:"cpuCount"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.CPU != 4 {
		t.Fatalf("MAC update changed CPU count to %d, want 4", cfg.CPU)
	}
	return cfg.MAC
}

func newBridgedTestTartProvider(t *testing.T, cmd *mockCommandRunner) *TartProvider {
	t.Helper()
	p := newTestTartProvider(cmd)
	p.netBridged = "en0"
	cmd.results["/sbin/ifconfig en0"] = cmdResult{output: []byte("en0: flags=8863<UP,BROADCAST,RUNNING> mtu 1500\n\tether " + testHostMAC + "\n")}
	writeTestVMConfig(t, p, "runner-abc")
	return p
}

func TestTartProvider_MACPreservesNATSlots(t *testing.T) {
	for slot, want := range []string{"02:00:00:00:00:01", "02:00:00:00:00:02"} {
		p := newTestTartProvider(&mockCommandRunner{})
		path := writeTestVMConfig(t, p, "runner-abc")
		if err := p.setVMMAC(context.Background(), "runner-abc", slot); err != nil {
			t.Fatal(err)
		}
		if got := readTestVMMAC(t, path); got != want {
			t.Fatalf("NAT slot %d MAC = %q, want %q", slot, got, want)
		}
	}
}

func TestTartProvider_BridgedMACIsStableAndHostScoped(t *testing.T) {
	assign := func(hostMAC string, slot int) string {
		t.Helper()
		cmd := networkCmdRunner(routeGetHostNoVPN)
		p := newBridgedTestTartProvider(t, cmd)
		cmd.results["/sbin/ifconfig en0"] = cmdResult{output: []byte("en0: flags=8863<UP,BROADCAST,RUNNING> mtu 1500\n\tether " + hostMAC + "\n")}
		if err := p.setVMMAC(context.Background(), "runner-abc", slot); err != nil {
			t.Fatal(err)
		}
		return readTestVMMAC(t, filepath.Join(p.home, "vms", "runner-abc", "config.json"))
	}

	first := assign(testHostMAC, 0)
	if again := assign(testHostMAC, 0); again != first {
		t.Fatalf("same host and slot changed MAC: %q then %q", first, again)
	}
	seen := map[string]bool{}
	for _, hostMAC := range []string{testHostMAC, "00:11:22:33:44:66"} {
		for slot := range 2 {
			mac := assign(hostMAC, slot)
			if seen[mac] {
				t.Fatalf("bridged MAC %q reused across hosts or slots", mac)
			}
			seen[mac] = true
			parsed, err := net.ParseMAC(mac)
			if err != nil || len(parsed) != 6 || parsed[0]&3 != 2 {
				t.Fatalf("MAC %q must be a locally administered unicast Ethernet address", mac)
			}
		}
	}
}

func TestTartProvider_BridgedMACFailureCleansUpBeforeBoot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lookup  cmdResult
		badJSON bool
	}{
		{name: "lookup failed", lookup: cmdResult{err: errors.New("interface unavailable")}},
		{name: "missing hardware address", lookup: cmdResult{output: []byte("en0: flags=UP\n")}},
		{name: "invalid hardware address", lookup: cmdResult{output: []byte("\tether invalid\n")}},
		{name: "zero hardware address", lookup: cmdResult{output: []byte("\tether 00:00:00:00:00:00\n")}},
		{name: "multicast hardware address", lookup: cmdResult{output: []byte("\tether 01:11:22:33:44:55\n")}},
		{name: "invalid VM config", lookup: cmdResult{output: []byte("\tether " + testHostMAC + "\n")}, badJSON: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := networkCmdRunner(routeGetHostNoVPN)
			p := newBridgedTestTartProvider(t, cmd)
			p.coordinator = NewTartHostCoordinator(1)
			cmd.results["/sbin/ifconfig en0"] = tc.lookup
			if tc.badJSON {
				path := filepath.Join(p.home, "vms", "runner-abc", "config.json")
				if err := os.WriteFile(path, []byte("invalid"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err == nil {
				t.Fatal("bridged VM started without a safe MAC address")
			}
			calls := cmd.getCalls()
			if cmd.callCount("tart run") != 0 || callIndex(calls, "tart", "delete", "runner-abc") < 0 {
				t.Fatalf("failed bridged VM must be deleted before boot; calls: %v", calls)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			slot, err := p.coordinator.acquire(ctx, TartGuestMacOS)
			if err != nil {
				t.Fatalf("failed bridged VM retained its host slot: %v", err)
			}
			p.coordinator.release(slot)
		})
	}
}
