package main

import (
	"io/fs"
	"strings"
	"testing"
	"time"
)

func envFrom(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestStartedByServiceManager(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "systemd", env: map[string]string{"INVOCATION_ID": "abc"}, want: true},
		{name: "current launchd agent", env: map[string]string{"XPC_SERVICE_NAME": "io.github.ysya.runner"}, want: true},
		{name: "legacy launchd agent", env: map[string]string{"XPC_SERVICE_NAME": "com.runscaler.agent"}, want: true},
		{name: "macOS terminal", env: map[string]string{"XPC_SERVICE_NAME": "0"}},
		{name: "plain shell"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := startedByServiceManager(envFrom(tt.env)); got != tt.want {
				t.Errorf("startedByServiceManager() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSystemdUnitFromCgroup(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "v2 system service", content: "0::/system.slice/runscaler.service", want: "runscaler.service"},
		{name: "v2 user service", content: "0::/user.slice/user-1000.slice/user@1000.service/app.slice/runner.service", want: "runner.service"},
		{name: "docker prefix", content: "0::/docker/abc123/system.slice/runner.service", want: "runner.service"},
		{name: "v1 name=systemd line", content: "12:pids:/system.slice/other.service\n1:name=systemd:/system.slice/runscaler.service", want: "runscaler.service"},
		{name: "file content with trailing newline", content: "0::/system.slice/runscaler.service\n", want: "runscaler.service"},
		{name: "session scope", content: "0::/user.slice/user-1000.slice/session-3.scope"},
		{name: "empty content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := systemdUnitFromCgroup(tt.content); got != tt.want {
				t.Errorf("systemdUnitFromCgroup(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestRunningUnderLegacyDefinition(t *testing.T) {
	cgroup := func(content string) func() (string, error) {
		return func() (string, error) { return content, nil }
	}
	unreadable := func() (string, error) {
		return "", &fs.PathError{Op: "open", Path: "/proc/self/cgroup", Err: fs.ErrNotExist}
	}
	systemd := map[string]string{"INVOCATION_ID": "abc"}
	tests := []struct {
		name       string
		goos       string
		env        map[string]string
		readCgroup func() (string, error)
		want       bool
	}{
		{name: "legacy launchd label", goos: "darwin", env: map[string]string{"XPC_SERVICE_NAME": "com.runscaler.agent"}, readCgroup: unreadable, want: true},
		{name: "current launchd label", goos: "darwin", env: map[string]string{"XPC_SERVICE_NAME": "io.github.ysya.runner"}, readCgroup: unreadable},
		{name: "legacy systemd unit", goos: "linux", env: systemd, readCgroup: cgroup("0::/system.slice/runscaler.service\n"), want: true},
		{name: "runner unit beside a leftover legacy file", goos: "linux", env: systemd, readCgroup: cgroup("0::/system.slice/runner.service\n")},
		{name: "unreadable cgroup", goos: "linux", env: systemd, readCgroup: unreadable},
		{name: "not started by systemd", goos: "linux", readCgroup: cgroup("0::/system.slice/runscaler.service\n")},
		{name: "systemd variables on darwin", goos: "darwin", env: systemd, readCgroup: cgroup("0::/system.slice/runscaler.service\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runningUnderLegacyDefinition(tt.goos, envFrom(tt.env), tt.readCgroup); got != tt.want {
				t.Errorf("runningUnderLegacyDefinition() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOutdatedServiceWarnings(t *testing.T) {
	drain := 2 * time.Hour
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{name: "not a service", env: map[string]string{serviceVersionEnv: "1"}},
		{name: "template without version", env: map[string]string{"INVOCATION_ID": "abc"}, want: []string{"predates"}},
		{name: "unparseable version", env: map[string]string{"INVOCATION_ID": "abc", serviceVersionEnv: "two"}, want: []string{"predates"}},
		{name: "current and long enough", env: map[string]string{"INVOCATION_ID": "abc", serviceVersionEnv: "2", serviceStopTimeoutEnv: "7260"}},
		{name: "current but stop timeout too short", env: map[string]string{"XPC_SERVICE_NAME": "io.github.ysya.runner", serviceVersionEnv: "2", serviceStopTimeoutEnv: "3660"}, want: []string{"shorter than drain-timeout"}},
		{name: "legacy label without version", env: map[string]string{"XPC_SERVICE_NAME": "com.runscaler.agent"}, want: []string{"predates"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := outdatedServiceWarnings(envFrom(tt.env), drain)
			if len(got) != len(tt.want) {
				t.Fatalf("warnings = %q, want %d matching %q", got, len(tt.want), tt.want)
			}
			for i, want := range tt.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("warning %d = %q, want it to contain %q", i, got[i], want)
				}
			}
		})
	}
}

func TestServiceReinstallCommand(t *testing.T) {
	got := serviceReinstallCommand(true, "/home/ada/runner/config.toml", "/opt/my runner/runner")
	want := "sudo '/opt/my runner/runner' service install --user=false --force --config-path '/home/ada/runner/config.toml' --binary-path '/opt/my runner/runner'"
	if got != want {
		t.Errorf("root command = %q\nwant %q", got, want)
	}
	got = serviceReinstallCommand(false, "", "/Users/ada/.local/bin/runner")
	want = "'/Users/ada/.local/bin/runner' service install --user --force --binary-path '/Users/ada/.local/bin/runner'"
	if got != want {
		t.Errorf("user command = %q\nwant %q", got, want)
	}
}

func TestServiceFixCommand(t *testing.T) {
	trusted := map[string]fileStat{
		"/": rootDir(0o755), "/usr": rootDir(0o755), "/usr/local": rootDir(0o755),
		"/usr/local/bin": rootDir(0o755), "/usr/local/bin/runner": {UID: 0, Mode: 0o755},
	}
	userDir := fileStat{UID: 1000, Mode: fs.ModeDir | 0o755}
	untrusted := map[string]fileStat{
		"/": rootDir(0o755), "/home": rootDir(0o755), "/home/ada": userDir,
		"/home/ada/.local": userDir, "/home/ada/.local/bin": userDir,
		"/home/ada/.local/bin/runner": {UID: 1000, Mode: 0o755},
	}
	tests := []struct {
		name        string
		goos        string
		root        bool
		configPath  string
		binaryPath  string
		underLegacy bool
		stat        map[string]fileStat
		want        string
	}{
		{
			name: "trusted root binary", goos: "linux", root: true,
			configPath: "/etc/runner/config.toml", binaryPath: "/usr/local/bin/runner", stat: trusted,
			want: "sudo '/usr/local/bin/runner' service install --user=false --force --config-path '/etc/runner/config.toml' --binary-path '/usr/local/bin/runner'",
		},
		{
			name: "untrusted root binary on linux", goos: "linux", root: true,
			configPath: "/etc/runner/config.toml", binaryPath: "/home/ada/.local/bin/runner", stat: untrusted,
			want: "sudo install -m 0755 '/home/ada/.local/bin/runner' /usr/local/bin/runner && sudo '/usr/local/bin/runner' service install --user=false --force --config-path '/etc/runner/config.toml' --binary-path '/usr/local/bin/runner'",
		},
		{
			name: "untrusted binary for a user", goos: "linux",
			configPath: "/home/ada/.config/runner/config.toml", binaryPath: "/home/ada/.local/bin/runner", stat: untrusted,
			want: "'/home/ada/.local/bin/runner' service install --user --force --config-path '/home/ada/.config/runner/config.toml' --binary-path '/home/ada/.local/bin/runner'",
		},
		{
			name: "darwin root", goos: "darwin", root: true,
			configPath: "/etc/runner/config.toml", binaryPath: "/home/ada/.local/bin/runner", stat: untrusted,
			want: "sudo '/home/ada/.local/bin/runner' service install --user=false --force --config-path '/etc/runner/config.toml' --binary-path '/home/ada/.local/bin/runner'",
		},
		{
			name: "legacy definition for root", goos: "linux", root: true, underLegacy: true,
			configPath: "/etc/runner/config.toml", binaryPath: "/home/ada/.local/bin/runner", stat: untrusted,
			want: "sudo '/home/ada/.local/bin/runner' migrate --user=false",
		},
		{
			name: "legacy definition for a user", goos: "linux", underLegacy: true,
			configPath: "/home/ada/.config/runner/config.toml", binaryPath: "/home/ada/.local/bin/runner", stat: untrusted,
			want: "'/home/ada/.local/bin/runner' migrate --user",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serviceFixCommand(tt.goos, tt.root, tt.configPath, tt.binaryPath, tt.underLegacy, fakeStat(tt.stat))
			if got != tt.want {
				t.Errorf("serviceFixCommand() = %q\nwant %q", got, tt.want)
			}
		})
	}
}
