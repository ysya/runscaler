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
		BinaryPath:  "/home/test/runner/runscaler",
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
		BinaryPath:     "/usr/local/bin/runscaler",
		ConfigPath:     "/etc/runscaler/config.toml",
		AfterDocker:    true,
		User:           false,
		ReadWritePaths: "/etc/runscaler /var/run/docker.sock",
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
}
