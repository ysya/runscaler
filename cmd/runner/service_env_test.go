package main

import (
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
