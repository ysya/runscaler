package provider

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/ysya/runscaler/internal/config"
)

// routeGetHostNoVPN is `route -n get github.com` on a Mac Studio host with no
// VPN running: a cloned host route through the LAN gateway.
const routeGetHostNoVPN = `   route to: 20.27.177.113
destination: 20.27.177.113
    gateway: 192.168.88.1
  interface: en0
      flags: <UP,GATEWAY,HOST,DONE,WASCLONED,IFSCOPE,IFREF,GLOBAL>
 recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire
       0         0         0         0         0         0      1500         0
`

// routeGetHostTunnel is `route -n get github.com` on a host whose VPN routes
// everything through a utun interface (captured with a 4000-byte tunnel).
const routeGetHostTunnel = `   route to: 20.27.177.113
destination: default
       mask: default
  interface: utun4
      flags: <UP,DONE,CLONING,STATIC,GLOBAL>
 recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire
       0         0         0         0         0         0      4000         0
`

// routeGetHostWARP has the tunnel layout above with Cloudflare WARP's
// 1380-byte utun MTU, the case that stalls TLS from the guest.
const routeGetHostWARP = `   route to: 20.27.177.113
destination: default
       mask: default
  interface: utun5
      flags: <UP,DONE,CLONING,STATIC,GLOBAL>
 recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire
       0         0         0         0         0         0      1380         0
`

func TestParseRouteGet(t *testing.T) {
	tests := []struct {
		name      string
		out       string
		wantIface string
		wantMTU   int
		wantErr   bool
	}{
		{name: "LAN route", out: routeGetHostNoVPN, wantIface: "en0", wantMTU: 1500},
		{name: "large tunnel", out: routeGetHostTunnel, wantIface: "utun4", wantMTU: 4000},
		{name: "WARP tunnel", out: routeGetHostWARP, wantIface: "utun5", wantMTU: 1380},
		{
			name:      "no metrics table leaves MTU unknown",
			out:       "   route to: 10.0.0.1\n  interface: en0\n",
			wantIface: "en0",
			wantMTU:   0,
		},
		{name: "no route", out: "route: writing to routing socket: not in table\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRouteGet([]byte(tt.out))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseRouteGet() = %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRouteGet() error: %v", err)
			}
			if got.Interface != tt.wantIface || got.MTU != tt.wantMTU {
				t.Errorf("parseRouteGet() = %+v, want interface %q MTU %d", got, tt.wantIface, tt.wantMTU)
			}
		})
	}
}

// serviceOrderOutput is `networksetup -listnetworkserviceorder` from a Mac
// with several wired ports.
const serviceOrderOutput = `An asterisk (*) denotes that a network service is disabled.
(1) USB 10/100/1000 LAN
(Hardware Port: USB 10/100/1000 LAN, Device: en7)

(2) USB 10/100/1G/2.5G LAN
(Hardware Port: USB 10/100/1G/2.5G LAN, Device: en9)

(3) Thunderbolt Bridge
(Hardware Port: Thunderbolt Bridge, Device: bridge0)

(4) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)
`

func TestNetworkServiceForDevice(t *testing.T) {
	tests := []struct {
		device string
		want   string
		found  bool
	}{
		{device: "en0", want: "Wi-Fi", found: true},
		{device: "en9", want: "USB 10/100/1G/2.5G LAN", found: true},
		{device: "en1", found: false},
		{device: "en", found: false},
	}
	for _, tt := range tests {
		t.Run(tt.device, func(t *testing.T) {
			got, ok := networkServiceForDevice([]byte(serviceOrderOutput), tt.device)
			if ok != tt.found || got != tt.want {
				t.Errorf("networkServiceForDevice(%q) = (%q, %v), want (%q, %v)", tt.device, got, ok, tt.want, tt.found)
			}
		})
	}
}

// guestServiceOrder is `networksetup -listnetworkserviceorder` inside a Tart
// macOS guest, whose only service is the virtio NIC.
const guestServiceOrder = `An asterisk (*) denotes that a network service is disabled.
(1) Ethernet
(Hardware Port: Ethernet, Device: en0)
`

// networkCmdRunner answers the commands StartInstance issues, with the
// host's route to GitHub and the guest's default route as given.
func networkCmdRunner(hostRoute string) *mockCommandRunner {
	return &mockCommandRunner{
		results: map[string]cmdResult{
			"tart clone":                    {},
			"tart run":                      {},
			"tart exec":                     {},
			"/sbin/route -n get github.com": {output: []byte(hostRoute)},
			"tart exec runner-abc /sbin/route -n get default":                      {output: []byte(routeGetHostNoVPN)},
			"tart exec runner-abc /usr/sbin/networksetup -listnetworkserviceorder": {output: []byte(guestServiceOrder)},
		},
	}
}

// callIndex returns the position of the first call with exactly this argv,
// or -1.
func callIndex(calls []cmdCall, name string, args ...string) int {
	want := name + " " + strings.Join(args, " ")
	for i, c := range calls {
		if c.name+" "+strings.Join(c.args, " ") == want {
			return i
		}
	}
	return -1
}

// runnerStartIndex returns the position of the `tart exec ... sh -c` call
// that launches run.sh, or -1.
func runnerStartIndex(calls []cmdCall) int {
	for i, c := range calls {
		if c.name == "tart" && len(c.args) == 5 && c.args[0] == "exec" && c.args[2] == "sh" &&
			strings.Contains(c.args[4], "ACTIONS_RUNNER_INPUT_JITCONFIG") {
			return i
		}
	}
	return -1
}

func countCallsContaining(calls []cmdCall, substr string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c.name+" "+strings.Join(c.args, " "), substr) {
			n++
		}
	}
	return n
}

func TestTartProvider_AutoMTUMatchesHostTunnelBeforeRunnerStarts(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostWARP)
	p := newTestTartProvider(cmd)

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}

	calls := cmd.getCalls()
	setMTU := callIndex(calls, "tart", "exec", "runner-abc", "/usr/bin/sudo", "-n", "/sbin/ifconfig", "en0", "mtu", "1380")
	if setMTU < 0 {
		t.Fatalf("guest MTU was not set to the WARP tunnel's 1380; calls: %v", calls)
	}
	if start := runnerStartIndex(calls); start < 0 || setMTU > start {
		t.Errorf("MTU set at call %d, runner started at call %d; the MTU must be set first", setMTU, start)
	}
}

func TestTartProvider_AutoMTULeavesGuestAloneOnFullSizeRoute(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostNoVPN)
	p := newTestTartProvider(cmd)

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}

	calls := cmd.getCalls()
	if n := countCallsContaining(calls, "ifconfig"); n != 0 {
		t.Errorf("ifconfig called %d times on a 1500-byte route, want 0", n)
	}
	if n := cmd.callCount("tart exec"); n != 5 {
		t.Errorf("tart exec called %d times, want the usual 5 when nothing needs adjusting", n)
	}
}

func TestTartProvider_MTUOffSkipsHostRouteLookup(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostWARP)
	p := newTestTartProvider(cmd)
	p.mtu = -1

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}

	calls := cmd.getCalls()
	if n := countCallsContaining(calls, "/sbin/route"); n != 0 {
		t.Errorf("route looked up %d times with mtu = -1, want 0", n)
	}
	if n := countCallsContaining(calls, "ifconfig"); n != 0 {
		t.Errorf("ifconfig called %d times with mtu = -1, want 0", n)
	}
}

func TestTartProvider_FixedMTUAppliedWithoutHostRouteLookup(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostNoVPN)
	p := newTestTartProvider(cmd)
	p.mtu = 1280

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}

	calls := cmd.getCalls()
	if i := callIndex(calls, "/sbin/route", "-n", "get", "github.com"); i >= 0 {
		t.Errorf("host route looked up despite a fixed mtu")
	}
	if callIndex(calls, "tart", "exec", "runner-abc", "/usr/bin/sudo", "-n", "/sbin/ifconfig", "en0", "mtu", "1280") < 0 {
		t.Errorf("guest MTU was not set to the configured 1280; calls: %v", calls)
	}
}

func TestTartProvider_DNSSetOnDefaultInterfaceService(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostNoVPN)
	p := newTestTartProvider(cmd)
	p.mtu = -1
	p.dns = []string{"1.1.1.1", "8.8.8.8"}

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}

	calls := cmd.getCalls()
	setDNS := callIndex(calls, "tart", "exec", "runner-abc", "/usr/bin/sudo", "-n",
		"/usr/sbin/networksetup", "-setdnsservers", "Ethernet", "1.1.1.1", "8.8.8.8")
	if setDNS < 0 {
		t.Fatalf("guest DNS was not set on the Ethernet service; calls: %v", calls)
	}
	if start := runnerStartIndex(calls); start < 0 || setDNS > start {
		t.Errorf("DNS set at call %d, runner started at call %d; DNS must be set first", setDNS, start)
	}
}

func TestTartProvider_GuestMTUFailureStillStartsRunner(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostWARP)
	cmd.results["tart exec runner-abc /usr/bin/sudo -n /sbin/ifconfig en0 mtu 1380"] = cmdResult{
		err: errors.New("sudo: a password is required"),
	}
	p := newTestTartProvider(cmd)

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v; a guest that cannot be adjusted must still run", err)
	}
	if runnerStartIndex(cmd.getCalls()) < 0 {
		t.Error("runner was not started after the MTU adjustment failed")
	}
}

func TestTartProvider_HostRouteFailureStillStartsRunner(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostWARP)
	cmd.results["/sbin/route -n get github.com"] = cmdResult{err: errors.New("route: bad address: github.com")}
	p := newTestTartProvider(cmd)

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}
	calls := cmd.getCalls()
	if n := countCallsContaining(calls, "ifconfig"); n != 0 {
		t.Errorf("ifconfig called %d times without a known host route, want 0", n)
	}
	if runnerStartIndex(calls) < 0 {
		t.Error("runner was not started after the host route lookup failed")
	}
}

func TestNewTartProvider_GuestNetworkFromScaleSetConfig(t *testing.T) {
	ss := config.ScaleSetConfig{
		RegistrationURL: "https://ghes.example.com/my-org",
		RunnerImage:     "macos-base:latest",
		MaxRunners:      1,
		Tart: config.TartConfig{
			RunnerDir: "/Users/admin/actions-runner",
			DNS:       []string{"9.9.9.9"},
		},
	}
	p := NewTartProviderWithCoordinator(ss, slog.New(slog.DiscardHandler), NewTartHostCoordinator(2))
	cmd := networkCmdRunner(routeGetHostNoVPN)
	cmd.results["/sbin/route -n get ghes.example.com"] = cmdResult{output: []byte(routeGetHostWARP)}
	p.cmd = cmd

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}

	calls := cmd.getCalls()
	if callIndex(calls, "tart", "exec", "runner-abc", "/usr/bin/sudo", "-n", "/sbin/ifconfig", "en0", "mtu", "1380") < 0 {
		t.Errorf("MTU not taken from the route to the scale set's GHES host; calls: %v", calls)
	}
	if callIndex(calls, "tart", "exec", "runner-abc", "/usr/bin/sudo", "-n",
		"/usr/sbin/networksetup", "-setdnsservers", "Ethernet", "9.9.9.9") < 0 {
		t.Errorf("DNS from [tart] dns was not applied; calls: %v", calls)
	}
}

func TestTartProvider_GuestMTUFollowsGuestDefaultInterface(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostWARP)
	cmd.results["tart exec runner-abc /sbin/route -n get default"] = cmdResult{
		output: []byte("   route to: default\n  interface: en1\n"),
	}
	p := newTestTartProvider(cmd)

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}
	if callIndex(cmd.getCalls(), "tart", "exec", "runner-abc", "/usr/bin/sudo", "-n", "/sbin/ifconfig", "en1", "mtu", "1380") < 0 {
		t.Errorf("MTU not set on the guest's default interface en1; calls: %v", cmd.getCalls())
	}
}

func TestTartProvider_GuestMTUFallsBackToEn0WithoutGuestRoute(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostWARP)
	cmd.results["tart exec runner-abc /sbin/route -n get default"] = cmdResult{err: errors.New("route: not in table")}
	p := newTestTartProvider(cmd)

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}
	if callIndex(cmd.getCalls(), "tart", "exec", "runner-abc", "/usr/bin/sudo", "-n", "/sbin/ifconfig", "en0", "mtu", "1380") < 0 {
		t.Errorf("MTU not set on en0 when the guest route lookup failed; calls: %v", cmd.getCalls())
	}
}

// recordingHandler keeps every log record so a test can count them.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler            { return h }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *recordingHandler) count(level slog.Level, msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && r.Message == msg {
			n++
		}
	}
	return n
}

// The MTU line is logged at Info only when the applied value changes, so a
// host that stays on WARP does not log it again for every job.
func TestTartProvider_MTUInfoLoggedOnlyWhenItChanges(t *testing.T) {
	cmd := networkCmdRunner(routeGetHostWARP)
	logs := &recordingHandler{}
	p := newTestTartProvider(cmd)
	p.logger = slog.New(logs)
	ctx := context.Background()

	for _, name := range []string{"runner-abc", "runner-def"} {
		if _, err := p.StartInstance(ctx, name, "jit"); err != nil {
			t.Fatalf("StartInstance(%s) error: %v", name, err)
		}
	}
	if n := logs.count(slog.LevelInfo, "Set VM MTU"); n != 1 {
		t.Fatalf("Info \"Set VM MTU\" logged %d times for two VMs at the same MTU, want 1", n)
	}

	cmd.results["/sbin/route -n get github.com"] = cmdResult{
		output: []byte(strings.Replace(routeGetHostWARP, "1380", "1280", 1)),
	}
	if _, err := p.StartInstance(ctx, "runner-ghi", "jit"); err != nil {
		t.Fatalf("StartInstance(runner-ghi) error: %v", err)
	}
	if n := logs.count(slog.LevelInfo, "Set VM MTU"); n != 2 {
		t.Errorf("Info \"Set VM MTU\" logged %d times after the MTU changed to 1280, want 2", n)
	}
}
