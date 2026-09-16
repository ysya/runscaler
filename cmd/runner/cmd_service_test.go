package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/config"
)

func TestDetectProviderSupportsCanonicalAndLegacyKeys(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "canonical tart", body: `provider = "tart"`, want: "tart"},
		{name: "legacy tart", body: `backend = "tart"`, want: "tart"},
		{name: "mixed providers need docker service", body: `
[[scaleset]]
provider = "tart"
[[scaleset]]
provider = "docker"
`, want: "docker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := detectProvider(path); got != tt.want {
				t.Errorf("detectProvider() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSystemdUnitUserModeOmitsDockerDependency(t *testing.T) {
	// User-level units cannot reference system units: systemd fails with
	// "Unit docker.service not found" and the service never starts.
	unit, err := renderSystemdUnit(installOpts{
		binaryPath: "/home/test/runner/runner",
		configPath: "/home/test/runner/config.toml",
		provider:   "docker",
		user:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(unit, "docker.service") {
		t.Errorf("user-level unit must not reference system unit docker.service:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("user-level unit should be wanted by default.target:\n%s", unit)
	}
}

func TestSystemdUnitSystemModeKeepsDockerDependency(t *testing.T) {
	unit, err := renderSystemdUnit(installOpts{
		binaryPath: "/usr/local/bin/runner",
		configPath: "/etc/runner/config.toml",
		provider:   "docker",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"After=docker.service",
		"Requires=docker.service",
		"WantedBy=multi-user.target",
		"ProtectSystem=strict",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("system-level unit missing %q:\n%s", want, unit)
		}
	}
	if !strings.Contains(unit, "run --config") {
		t.Errorf("ExecStart must invoke the `run` subcommand:\n%s", unit)
	}
}

func unitValues(unit, key string) []string {
	var values []string
	for line := range strings.SplitSeq(unit, "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			values = append(values, strings.Fields(value)...)
		}
	}
	return values
}

func TestSystemdUnitSystemModeSandbox(t *testing.T) {
	unit, err := renderSystemdUnit(installOpts{
		binaryPath: "/usr/local/bin/runner",
		configPath: "/etc/runner/config.toml",
		provider:   "docker",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := unitValues(unit, "ReadWritePaths"); !slices.Equal(got, []string{"/tmp"}) {
		t.Errorf("ReadWritePaths = %q, want only /tmp (lock); config and socket stay read-only", got)
	}
	if got := unitValues(unit, "LogsDirectory"); !slices.Equal(got, []string{"runner"}) {
		t.Errorf("LogsDirectory = %q, want runner", got)
	}
	for _, want := range []string{
		"ProtectSystem=strict",
		"NoNewPrivileges=true",
		`Environment="RUNNER_SERVICE_VERSION=2"`,
		`Environment="RUNNER_SERVICE_STOP_TIMEOUT=7260"`,
		`ExecStart="/usr/local/bin/runner" run --config "/etc/runner/config.toml"`,
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("system unit missing %q:\n%s", want, unit)
		}
	}
}

func TestSystemdLogsDirectory(t *testing.T) {
	str := func(s string) *string { return &s }
	tests := []struct {
		name        string
		logFile     *string
		wantDir     string
		wantWarning bool
	}{
		{name: "unset", wantDir: "runner"},
		{name: "subdirectory", logFile: str("/var/log/custom/runner.log"), wantDir: "custom"},
		{name: "nested subdirectory", logFile: str("/var/log/ci/runner/runner.log"), wantDir: "ci/runner"},
		{name: "directly in var log", logFile: str("/var/log/runner.log"), wantDir: "runner", wantWarning: true},
		{name: "outside var log", logFile: str("/home/ada/runner/runner.log"), wantDir: "runner", wantWarning: true},
		{name: "relative", logFile: str("logs/runner.log"), wantDir: "runner", wantWarning: true},
		{name: "unsafe characters", logFile: str("/var/log/has space/runner.log"), wantDir: "runner", wantWarning: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, warning := systemdLogsDirectory(tt.logFile)
			if dir != tt.wantDir || (warning != "") != tt.wantWarning {
				t.Errorf("systemdLogsDirectory() = %q, %q; want %q, warning %v", dir, warning, tt.wantDir, tt.wantWarning)
			}
		})
	}
}

func TestSystemdUnitUsesCustomLogsDirectory(t *testing.T) {
	logFile := "/var/log/custom/runner.log"
	unit, err := renderSystemdUnit(installOpts{binaryPath: "/usr/local/bin/runner", configPath: "/etc/runner/config.toml", logFile: &logFile})
	if err != nil {
		t.Fatal(err)
	}
	if got := unitValues(unit, "LogsDirectory"); !slices.Equal(got, []string{"custom"}) {
		t.Errorf("LogsDirectory = %q, want custom", got)
	}
}

func TestSystemdUnitEscapesExecStartArguments(t *testing.T) {
	unit, err := renderSystemdUnit(installOpts{binaryPath: `/opt/my runner/100%/$HOME/run"er`, configPath: "/etc/runner/config.toml"})
	if err != nil {
		t.Fatal(err)
	}
	want := `ExecStart="/opt/my runner/100%%/$$HOME/run\"er" run --config "/etc/runner/config.toml"`
	if !strings.Contains(unit, want) {
		t.Errorf("unit missing %q:\n%s", want, unit)
	}
}

func TestSystemdUnitRejectsControlCharacters(t *testing.T) {
	if _, err := renderSystemdUnit(installOpts{binaryPath: "/opt/run\nner", configPath: "/etc/runner/config.toml"}); err == nil {
		t.Fatal("a newline in the binary path was written into the unit")
	}
	if _, err := renderSystemdUnit(installOpts{user: true, binaryPath: "/opt/runner", configPath: "/etc/runner/config.toml", xdgStateHome: "/xdg/\tstate"}); err == nil {
		t.Fatal("a tab in an environment value was written into the unit")
	}
}

func TestSystemdExecStartRoundTrip(t *testing.T) {
	for _, tc := range []struct{ binary, config string }{
		{"/usr/local/bin/runner", "/etc/runner/config.toml"},
		{`/opt/my runner/run"er`, `/data/100%/$HOME/c\fg.toml`},
	} {
		unit, err := renderSystemdUnit(installOpts{binaryPath: tc.binary, configPath: tc.config})
		if err != nil {
			t.Fatal(err)
		}
		invocation, err := parseSystemdServiceInvocation([]byte(unit))
		if err != nil {
			t.Fatal(err)
		}
		if invocation.BinaryPath != tc.binary || invocation.ConfigPath != tc.config {
			t.Errorf("round trip = %+v, want binary %q config %q", invocation, tc.binary, tc.config)
		}
	}
}

func TestSplitSystemdCommandRejectsUnterminatedQuote(t *testing.T) {
	if _, err := splitSystemdCommand(`"/usr/local/bin/runner run`); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestSystemdUserUnitRecordsAbsoluteXDG(t *testing.T) {
	unit, err := renderSystemdUnit(installOpts{
		user: true, binaryPath: "/home/ada/.local/bin/runner", configPath: "/home/ada/.config/runner/config.toml",
		xdgStateHome: "/xdg/state", xdgConfigHome: "relative/config",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, `Environment="XDG_STATE_HOME=/xdg/state"`) || strings.Contains(unit, "XDG_CONFIG_HOME") {
		t.Errorf("user unit XDG environment wrong:\n%s", unit)
	}
	if strings.Contains(unit, "LogsDirectory=") || strings.Contains(unit, "ReadWritePaths=") {
		t.Errorf("user unit must not carry the system sandbox:\n%s", unit)
	}
}

func TestLaunchdPlistDiscardsStdoutAndRecordsEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plist, err := renderLaunchdPlist(installOpts{
		user: true, binaryPath: "/Users/ada/.local/bin/runner", configPath: "/Users/ada/.config/runner/config.toml",
		xdgStateHome: "/xdg/state",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<key>StandardOutPath</key>\n    <string>/dev/null</string>",
		"<key>StandardErrorPath</key>\n    <string>" + filepath.Join(home, "Library", "Logs", "runner", "stderr.log") + "</string>",
		"<key>RUNNER_SERVICE_VERSION</key>\n        <string>2</string>",
		"<key>RUNNER_SERVICE_STOP_TIMEOUT</key>\n        <string>7260</string>",
		"<key>XDG_STATE_HOME</key>\n        <string>/xdg/state</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestLaunchdPlistEscapesXML(t *testing.T) {
	binary := "/Users/a&b/<bin>/runner"
	plist, err := renderLaunchdPlist(installOpts{user: true, binaryPath: binary, configPath: "/Users/a&b/config.toml"})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := parseLaunchdServiceInvocation([]byte(plist))
	if err != nil {
		t.Fatalf("plist is not parseable: %v\n%s", err, plist)
	}
	if invocation.BinaryPath != binary || invocation.ConfigPath != "/Users/a&b/config.toml" {
		t.Errorf("XML round trip = %+v", invocation)
	}
	decoder := xml.NewDecoder(strings.NewReader(plist))
	decoder.Strict = false // the DOCTYPE is not resolved
	for {
		if _, err := decoder.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("plist is not well-formed: %v", err)
		}
	}
}

func TestLaunchdPlistPassesPlutil(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plutil is macOS-only")
	}
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not found")
	}
	plist, err := renderLaunchdPlist(installOpts{user: true, binaryPath: "/Users/a&b/runner", configPath: "/Users/a b/config.toml"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runner.plist")
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v\n%s", err, out)
	}
}

func TestLaunchdPlistInvokesRunSubcommand(t *testing.T) {
	plist, err := renderLaunchdPlist(installOpts{
		binaryPath: "/usr/local/bin/runner",
		configPath: "/etc/runner/config.toml",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist, "<string>run</string>") {
		t.Errorf("ProgramArguments must include the `run` subcommand:\n%s", plist)
	}
	if !strings.Contains(plist, "io.github.ysya.runner") {
		t.Errorf("launchd label should be io.github.ysya.runner:\n%s", plist)
	}
}

func TestSystemdTemplateBoundsStopByDrainTimeout(t *testing.T) {
	unit, err := renderSystemdUnit(installOpts{
		user: true, configPath: "/etc/runner/config.toml", binaryPath: "/usr/local/bin/runner",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := int((config.DefaultDrainTimeout + time.Minute).Seconds())
	if !strings.Contains(unit, fmt.Sprintf("TimeoutStopSec=%d", want)) {
		t.Errorf("TimeoutStopSec must exceed the drain budget (%v), unit was:\n%s",
			config.DefaultDrainTimeout, unit)
	}
}

func TestLaunchdTemplateSetsExitTimeOut(t *testing.T) {
	plist, err := renderLaunchdPlist(installOpts{
		configPath: "/Users/admin/runner/config.toml", binaryPath: "/usr/local/bin/runner",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := int((config.DefaultDrainTimeout + time.Minute).Seconds())
	for _, fragment := range []string{"<key>ExitTimeOut</key>", fmt.Sprintf("<integer>%d</integer>", want)} {
		if !strings.Contains(plist, fragment) {
			t.Errorf("launchd plist missing %q:\n%s", fragment, plist)
		}
	}
}

func TestServiceTemplatesHonorConfiguredDrainTimeout(t *testing.T) {
	custom := 3 * time.Hour
	unit, err := renderSystemdUnit(installOpts{configPath: "/etc/runner/config.toml", drainTimeout: &custom})
	if err != nil {
		t.Fatal(err)
	}
	want := int((custom + time.Minute).Seconds())
	if !strings.Contains(unit, fmt.Sprintf("TimeoutStopSec=%d", want)) {
		t.Errorf("custom drain timeout not reflected in unit:\n%s", unit)
	}

	disabled := time.Duration(0)
	plist, err := renderLaunchdPlist(installOpts{configPath: "/etc/runner/config.toml", drainTimeout: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist, "<integer>60</integer>") {
		t.Errorf("disabled drain should leave one minute for immediate cleanup:\n%s", plist)
	}
}

func TestDetectDrainTimeoutPreservesUnsetAndExplicitZero(t *testing.T) {
	dir := t.TempDir()
	writeConfig := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	unset, err := detectDrainTimeout(writeConfig("unset.toml", `name = "runner"`))
	if err != nil {
		t.Fatal(err)
	}
	if unset != nil {
		t.Fatalf("omitted drain-timeout = %v, want nil/default", *unset)
	}

	explicit, err := detectDrainTimeout(writeConfig("zero.toml", `drain-timeout = "0s"`))
	if err != nil {
		t.Fatal(err)
	}
	if explicit == nil || *explicit != 0 {
		t.Fatalf("explicit zero = %v, want non-nil zero", explicit)
	}
}
