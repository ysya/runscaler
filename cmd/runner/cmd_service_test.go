package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/config"
)

func TestReadServiceConfigProviderSupportsCanonicalAndLegacyKeys(t *testing.T) {
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
			facts, err := readServiceConfig(path, true)
			if err != nil || facts.provider != tt.want {
				t.Errorf("provider = %q, %v; want %q", facts.provider, err, tt.want)
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
	unit, err := renderSystemdUnit(installOpts{
		binaryPath: `/opt/my runner/100%/$HOME/runner`,
		configPath: `/data/100%/$HOME/c\fg "x".toml`,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `ExecStart="/opt/my runner/100%%/$HOME/runner" run --config "/data/100%%/$$HOME/c\\fg \"x\".toml"`
	if !strings.Contains(unit, want) {
		t.Errorf("unit missing %q:\n%s", want, unit)
	}
}

func TestSystemdUnitRejectsUnsafeExecutablePaths(t *testing.T) {
	for _, binaryPath := range []string{
		`/opt/run"ner`,
		`/opt/run'ner`,
		`/opt/run\ner`,
		`/opt/run*ner`,
		`/opt/run?ner`,
		`/opt/run[ner`,
		"/opt/run\xffner",
	} {
		_, err := renderSystemdUnit(installOpts{binaryPath: binaryPath, configPath: "/etc/runner/config.toml"})
		if err == nil {
			t.Errorf("binaryPath %q: want error, got nil", binaryPath)
			continue
		}
		if !strings.Contains(err.Error(), "executable path") {
			t.Errorf("binaryPath %q: error %q does not mention the executable path restriction", binaryPath, err)
		}
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
		{"/opt/my runner/100%/$HOME/runner", "/data/100%/$HOME/c\\fg \"x\".toml"},
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

func TestSplitSystemdCommandKeepsDollarsInExecutable(t *testing.T) {
	got, err := splitSystemdCommand(`"/opt/a$$b/runner" run --config "/x/$$y"`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/a$$b/runner", "run", "--config", "/x/$y"}
	if !slices.Equal(got, want) {
		t.Errorf("splitSystemdCommand() = %q, want %q", got, want)
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

func TestReadServiceConfigPreservesUnsetAndExplicitZeroDrain(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	unset, err := readServiceConfig(write("unset.toml", `name = "runner"`), true)
	if err != nil || unset.drainTimeout != nil {
		t.Fatalf("omitted drain-timeout = %v, %v; want nil", unset.drainTimeout, err)
	}
	zero, err := readServiceConfig(write("zero.toml", `drain-timeout = "0s"`), true)
	if err != nil || zero.drainTimeout == nil || *zero.drainTimeout != 0 {
		t.Fatalf("explicit zero = %v, %v; want non-nil zero", zero.drainTimeout, err)
	}
}

func TestReadServiceConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	valid := write("valid.toml", "provider = \"tart\"\ndrain-timeout = \"3h\"\nlog-file = \"/var/log/custom/runner.log\"\n")
	broken := write("broken.toml", "provider = \n")
	invalid := write("invalid.toml", "drain-timeout = \"soon\"\n")
	missing := filepath.Join(dir, "missing.toml")

	facts, err := readServiceConfig(valid, true)
	if err != nil || !facts.found || facts.provider != "tart" ||
		facts.drainTimeout == nil || *facts.drainTimeout != 3*time.Hour ||
		facts.logFile == nil || *facts.logFile != "/var/log/custom/runner.log" {
		t.Fatalf("valid config facts = %+v, %v", facts, err)
	}
	if _, err := readServiceConfig(broken, false); err == nil {
		t.Fatal("a config with a syntax error was accepted")
	}
	// drain-timeout = "soon" parses as TOML but config.Load rejects it (not a
	// valid duration), exercising the decode-failure branch distinct from the
	// syntax-error branch above.
	if _, err := readServiceConfig(invalid, false); err == nil || !strings.Contains(err.Error(), "load config") {
		t.Fatalf("readServiceConfig(invalid, false) = %v, want an error from the load step", err)
	}
	if _, err := readServiceConfig(invalid, true); err == nil || !strings.Contains(err.Error(), "load config") {
		t.Fatalf("readServiceConfig(invalid, true) = %v, want an error from the load step", err)
	}
	if facts, err := readServiceConfig(missing, false); err != nil || facts.found {
		t.Fatalf("missing optional config = %+v, %v", facts, err)
	}
	if _, err := readServiceConfig(missing, true); err == nil {
		t.Fatal("a missing required config was accepted")
	}
}

func TestBuildInstallOptsRecordsConfigFactsAndXDG(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(valid, []byte("provider = \"tart\"\nlog-file = \"/var/log/custom/runner.log\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := envFrom(map[string]string{"XDG_STATE_HOME": "/xdg/state", "XDG_CONFIG_HOME": "/xdg/config"})

	opts, warnings, err := buildInstallOpts(true, valid, "/usr/local/bin/runner", true, env)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("buildInstallOpts() warnings=%q err=%v", warnings, err)
	}
	if !opts.user || opts.configPath != valid || opts.binaryPath != "/usr/local/bin/runner" || opts.provider != "tart" ||
		opts.logFile == nil || *opts.logFile != "/var/log/custom/runner.log" ||
		opts.xdgStateHome != "/xdg/state" || opts.xdgConfigHome != "/xdg/config" {
		t.Fatalf("installOpts = %+v", opts)
	}

	missing := filepath.Join(dir, "missing.toml")
	if _, warnings, err := buildInstallOpts(true, missing, "/usr/local/bin/runner", false, env); err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "Config file not found") {
		t.Fatalf("fresh install without config: warnings=%q err=%v", warnings, err)
	}
	if _, _, err := buildInstallOpts(true, missing, "/usr/local/bin/runner", true, env); err == nil {
		t.Fatal("--force without a config was accepted")
	}
}

func TestValidateServiceBinary(t *testing.T) {
	safe := map[string]fileStat{
		"/": rootDir(0o755), "/usr": rootDir(0o755), "/usr/local": rootDir(0o755),
		"/usr/local/bin": rootDir(0o755), "/usr/local/bin/runner": {UID: 0, Mode: 0o755},
	}
	unsafe := map[string]fileStat{
		"/": rootDir(0o755), "/home": rootDir(0o755), "/home/ada": {UID: 1000, Mode: fs.ModeDir | 0o755},
		"/home/ada/runner": {UID: 1000, Mode: 0o755},
	}
	resolve := func(path string) (string, error) {
		switch path {
		case "/usr/local/bin/link":
			return "/usr/local/bin/runner", nil
		case "/missing":
			return "", fs.ErrNotExist
		}
		return path, nil
	}
	tests := []struct {
		name    string
		goos    string
		user    bool
		euid    int
		path    string
		stat    map[string]fileStat
		want    string
		wantErr string
		// wantUntrusted is the resolved binary an *untrustedBinaryError must name.
		wantUntrusted string
	}{
		{name: "linux system resolves the symlink then passes", goos: "linux", euid: 0, path: "/usr/local/bin/link", stat: safe, want: "/usr/local/bin/runner"},
		{name: "linux system user-owned binary", goos: "linux", euid: 0, path: "/home/ada/runner", stat: unsafe, wantUntrusted: "/home/ada/runner"},
		{name: "linux user service skips the check", goos: "linux", user: true, euid: 1000, path: "/home/ada/runner", stat: unsafe, want: "/home/ada/runner"},
		{name: "linux user service installed by root is checked", goos: "linux", user: true, euid: 0, path: "/home/ada/runner", stat: unsafe, wantUntrusted: "/home/ada/runner"},
		{name: "darwin system scope skips the check", goos: "darwin", euid: 0, path: "/home/ada/runner", stat: unsafe, want: "/home/ada/runner"},
		{name: "unresolvable binary", goos: "linux", euid: 0, path: "/missing", stat: safe, wantErr: "resolve binary"},
		{name: "system binary that is a directory", goos: "linux", euid: 0, path: "/usr/local/bin", stat: safe, wantErr: "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateServiceBinary(tt.goos, tt.user, tt.euid, tt.path, resolve, fakeStat(tt.stat))
			if tt.wantUntrusted != "" {
				var untrusted *untrustedBinaryError
				if !errors.As(err, &untrusted) || untrusted.Path != tt.wantUntrusted {
					t.Fatalf("validateServiceBinary() error = %v, want an untrusted binary error for %s", err, tt.wantUntrusted)
				}
				if strings.Contains(err.Error(), "sudo") {
					t.Fatalf("validateServiceBinary() error = %v, want no hint text", err)
				}
				return
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateServiceBinary() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("validateServiceBinary() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestInstallServiceValidatesBeforeTouchingExistingDefinition(t *testing.T) {
	var actions []string
	installed, running := true, true
	manager := &fakeMigrationServiceManager{actions: &actions, installed: &installed, running: &running}
	opts := installOpts{binaryPath: "/home/ada/runner", configPath: "/etc/runner/config.toml", force: true}

	err := installService(manager, opts, func(bool, string) (string, error) { return "", errors.New("unsafe binary") })
	if err == nil || len(actions) != 0 {
		t.Fatalf("failed validation: err=%v actions=%v, want no manager action", err, actions)
	}

	saved := renderServiceDefinition
	t.Cleanup(func() { renderServiceDefinition = saved })
	renderServiceDefinition = func(installOpts) (string, error) { return "", errors.New("render failed") }
	err = installService(manager, opts, func(_ bool, p string) (string, error) { return p, nil })
	if err == nil || len(actions) != 0 {
		t.Fatalf("failed render: err=%v actions=%v, want no manager action", err, actions)
	}

	renderServiceDefinition = saved
	err = installService(manager, opts, func(bool, string) (string, error) { return "/usr/local/bin/runner", nil })
	if err != nil || strings.Join(actions, ",") != "install-new" || manager.installOpts.binaryPath != "/usr/local/bin/runner" {
		t.Fatalf("valid install: err=%v actions=%v opts=%+v", err, actions, manager.installOpts)
	}
}

func TestInstallRetryCommand(t *testing.T) {
	tests := []struct {
		name string
		opts installOpts
		want string
	}{
		{
			name: "system scope",
			opts: installOpts{configPath: "/etc/runner/config.toml"},
			want: "sudo /usr/local/bin/runner service install --user=false --config-path '/etc/runner/config.toml' --binary-path /usr/local/bin/runner",
		},
		{
			name: "system scope with --force",
			opts: installOpts{configPath: "/etc/runner/config.toml", force: true},
			want: "sudo /usr/local/bin/runner service install --user=false --force --config-path '/etc/runner/config.toml' --binary-path /usr/local/bin/runner",
		},
		{
			name: "user scope with --force and --no-start",
			opts: installOpts{user: true, configPath: "/root/my runner/config.toml", force: true, noStart: true},
			want: "sudo /usr/local/bin/runner service install --user --force --no-start --config-path '/root/my runner/config.toml' --binary-path /usr/local/bin/runner",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := installRetryCommand(tt.opts); got != tt.want {
				t.Errorf("installRetryCommand() = %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestUntrustedBinaryHint(t *testing.T) {
	retry := "sudo /usr/local/bin/runner service install --user=false --force --config-path '/etc/runner/config.toml' --binary-path /usr/local/bin/runner"
	tests := []struct {
		name   string
		binary string
		want   string
	}{
		{
			name:   "binary elsewhere",
			binary: "/home/ada/.local/bin/runner",
			want: "\n\n  Install a copy only root can modify, then retry:\n" +
				"    sudo install -m 0755 '/home/ada/.local/bin/runner' /usr/local/bin/runner\n" +
				"    " + retry,
		},
		{
			name:   "binary already at the root path",
			binary: "/usr/local/bin/runner",
			want:   "\n\n  Make the path reported above changeable only by root, then retry:\n    " + retry,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := untrustedBinaryHint(tt.binary, retry); got != tt.want {
				t.Errorf("untrustedBinaryHint() = %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestUninstallServicesRemovesLegacyDefinitions(t *testing.T) {
	notInstalled := func(bool) error { return fmt.Errorf("%w (no unit file)", errServiceNotInstalled) }
	removed := func(bool) error { return nil }
	tests := []struct {
		name        string
		current     func(bool) error
		legacy      bool
		removeErr   error
		wantLegacy  bool
		wantErrIs   error
		wantErrText string
	}{
		{name: "only legacy present", current: notInstalled, legacy: true, wantLegacy: true},
		{name: "both present", current: removed, legacy: true, wantLegacy: true},
		{name: "only current present", current: removed},
		{name: "nothing installed", current: notInstalled, wantErrIs: errServiceNotInstalled},
		{name: "legacy removal fails", current: removed, legacy: true, removeErr: errors.New("busy"), wantErrText: "busy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLegacy, err := uninstallServices(false, tt.current, func(bool) bool { return tt.legacy }, func(bool) error { return tt.removeErr })
			if gotLegacy != tt.wantLegacy {
				t.Errorf("removedLegacy = %v, want %v", gotLegacy, tt.wantLegacy)
			}
			switch {
			case tt.wantErrIs != nil:
				if !errors.Is(err, tt.wantErrIs) {
					t.Errorf("err = %v, want %v", err, tt.wantErrIs)
				}
			case tt.wantErrText != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Errorf("err = %v, want %q", err, tt.wantErrText)
				}
			case err != nil:
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

func TestLaunchdServiceLogFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := filepath.Join(home, "Library", "Logs", "runner", "stderr.log")
	legacy := filepath.Join(home, "Library", "Logs", "runner.log")
	only := func(paths ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(paths, p) }
	}
	if path, note := launchdServiceLogFile(true, only(current, legacy)); path != current || note != "" {
		t.Errorf("with the current log = %q, %q", path, note)
	}
	if path, note := launchdServiceLogFile(true, only(legacy)); path != legacy || !strings.Contains(note, "--force") {
		t.Errorf("with only the legacy log = %q, %q", path, note)
	}
	if path, _ := launchdServiceLogFile(true, only()); path != "" {
		t.Errorf("with no log = %q, want empty", path)
	}
}

func TestStatusPrivilegeError(t *testing.T) {
	if err := statusPrivilegeError("darwin", false, 501); err == nil || !strings.Contains(err.Error(), "sudo runner service status --user=false") {
		t.Errorf("macOS system status as a user = %v", err)
	}
	for _, tc := range []struct {
		goos string
		user bool
		euid int
	}{{"darwin", true, 501}, {"darwin", false, 0}, {"linux", false, 1000}} {
		if err := statusPrivilegeError(tc.goos, tc.user, tc.euid); err != nil {
			t.Errorf("statusPrivilegeError(%+v) = %v, want nil", tc, err)
		}
	}
}

func TestResolveBinaryPathResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "runner")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "runner-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	c := &cobra.Command{}
	c.Flags().String("binary-path", "", "")
	if err := c.Flags().Set("binary-path", link); err != nil {
		t.Fatal(err)
	}
	got, err := resolveBinaryPath(c)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if got != want {
		t.Fatalf("resolveBinaryPath() = %q, want %q", got, want)
	}
}
