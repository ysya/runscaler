package main

import (
	"strings"
	"testing"
)

// renderSystemdUnit renders the systemd unit template with the given data.
func renderSystemdUnit(t *testing.T, data systemdData) string {
	t.Helper()
	var sb strings.Builder
	if err := systemdTmpl.Execute(&sb, data); err != nil {
		t.Fatalf("render systemd template: %v", err)
	}
	return sb.String()
}

func TestSystemdUnitUserModeOmitsDockerDependency(t *testing.T) {
	// User-level units cannot reference system units: systemd fails with
	// "Unit docker.service not found" and the service never starts.
	unit := renderSystemdUnit(t, systemdData{
		Description: serviceDescription,
		BinaryPath:  "/home/test/runner/runner",
		ConfigPath:  "/home/test/runner/config.toml",
		AfterDocker: true,
		User:        true,
	})

	if strings.Contains(unit, "docker.service") {
		t.Errorf("user-level unit must not reference system unit docker.service:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("user-level unit should be wanted by default.target:\n%s", unit)
	}
}

func TestSystemdUnitSystemModeKeepsDockerDependency(t *testing.T) {
	unit := renderSystemdUnit(t, systemdData{
		Description:    serviceDescription,
		BinaryPath:     "/usr/local/bin/runner",
		ConfigPath:     "/etc/runner/config.toml",
		AfterDocker:    true,
		User:           false,
		ReadWritePaths: "/etc/runner /var/run/docker.sock",
	})

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

func renderLaunchdPlist(t *testing.T, data launchdData) string {
	t.Helper()
	var sb strings.Builder
	if err := launchdTmpl.Execute(&sb, data); err != nil {
		t.Fatalf("render launchd template: %v", err)
	}
	return sb.String()
}

func TestLaunchdPlistInvokesRunSubcommand(t *testing.T) {
	plist := renderLaunchdPlist(t, launchdData{
		Label:      launchdLabel,
		BinaryPath: "/usr/local/bin/runner",
		ConfigPath: "/etc/runner/config.toml",
		LogPath:    "/var/log/runner.log",
	})
	if !strings.Contains(plist, "<string>run</string>") {
		t.Errorf("ProgramArguments must include the `run` subcommand:\n%s", plist)
	}
	if !strings.Contains(plist, "io.github.ysya.runner") {
		t.Errorf("launchd label should be io.github.ysya.runner:\n%s", plist)
	}
}
