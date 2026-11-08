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
		{name: "retired launchd label", env: map[string]string{"XPC_SERVICE_NAME": "com.runscaler.agent"}},
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
		{name: "previous template", env: map[string]string{"INVOCATION_ID": "abc", serviceVersionEnv: "2", serviceStopTimeoutEnv: "7260"}, want: []string{"predates"}},
		{name: "current and long enough", env: map[string]string{"INVOCATION_ID": "abc", serviceVersionEnv: "3", serviceStopTimeoutEnv: "7260"}},
		{name: "current but stop timeout too short", env: map[string]string{"XPC_SERVICE_NAME": "io.github.ysya.runner", serviceVersionEnv: "3", serviceStopTimeoutEnv: "3660"}, want: []string{"shorter than drain-timeout"}},
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
	groupWritableBin := map[string]fileStat{
		"/": rootDir(0o755), "/usr": rootDir(0o755), "/usr/local": rootDir(0o755),
		"/usr/local/bin": rootDir(0o775), "/usr/local/bin/runner": {UID: 0, Mode: 0o755},
	}
	tests := []struct {
		name       string
		goos       string
		scope      serviceScope
		configPath string
		binaryPath string
		stat       map[string]fileStat
		want       string
	}{
		{
			name: "trusted root binary", goos: "linux", scope: scopeSystem,
			configPath: "/etc/runner/config.toml", binaryPath: "/usr/local/bin/runner", stat: trusted,
			want: "sudo /usr/local/bin/runner service install --scope system --force --config-path /etc/runner/config.toml --binary-path /usr/local/bin/runner",
		},
		{
			name: "untrusted root binary on linux", goos: "linux", scope: scopeSystem,
			configPath: "/etc/runner/config.toml", binaryPath: "/home/ada/.local/bin/runner", stat: untrusted,
			want: "sudo install -m 0755 /home/ada/.local/bin/runner /usr/local/bin/runner && sudo /usr/local/bin/runner service install --scope system --force --config-path /etc/runner/config.toml --binary-path /usr/local/bin/runner",
		},
		{
			name: "untrusted binary already at the root path", goos: "linux", scope: scopeSystem,
			configPath: "/etc/runner/config.toml", binaryPath: "/usr/local/bin/runner", stat: groupWritableBin,
			want: "sudo /usr/local/bin/runner service install --scope system --force --config-path /etc/runner/config.toml --binary-path /usr/local/bin/runner",
		},
		{
			name: "untrusted binary for a user", goos: "linux", scope: scopeUser,
			configPath: "/home/ada/.config/runner/config.toml", binaryPath: "/home/ada/.local/bin/runner", stat: untrusted,
			want: "/home/ada/.local/bin/runner service install --scope user --force --config-path /home/ada/.config/runner/config.toml --binary-path /home/ada/.local/bin/runner",
		},
		{
			name: "darwin root", goos: "darwin", scope: scopeSystem,
			configPath: "/etc/runner/config.toml", binaryPath: "/home/ada/.local/bin/runner", stat: untrusted,
			want: "sudo /home/ada/.local/bin/runner service install --scope system --force --config-path /etc/runner/config.toml --binary-path /home/ada/.local/bin/runner",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serviceFixCommand(tt.goos, tt.scope, tt.configPath, tt.binaryPath, fakeStat(tt.stat))
			if got != tt.want {
				t.Errorf("serviceFixCommand() = %q\nwant %q", got, tt.want)
			}
		})
	}
}
