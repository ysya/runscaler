package main

import (
	"strings"
	"testing"
)

func TestSystemdEnvironmentFileUsesLiteralSinglePath(t *testing.T) {
	unit, err := renderSystemdUnit(installOpts{binaryPath: "/usr/local/bin/runner", configPath: "/etc/runner/config.toml", environmentFiles: []string{"/etc/runner/service $literal %env.conf"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "\nEnvironmentFile=/etc/runner/service $literal %%env.conf\n") {
		t.Fatalf("EnvironmentFile is not a literal path (the systemd parser does not remove quotes):\n%s", unit)
	}
}

func TestSystemdEnvironmentFileRejectsAmbiguousPaths(t *testing.T) {
	for _, path := range []string{"/etc/runner/credentials*", "/etc/runner/key?", "/etc/runner/[key]", "/etc/runner/trailing ", "/etc/runner/back\\slash", "/etc/runner/new\nline"} {
		if _, err := renderSystemdUnit(installOpts{binaryPath: "/usr/local/bin/runner", configPath: "/etc/runner/config.toml", environmentFiles: []string{path}}); err == nil {
			t.Errorf("accepted ambiguous environment path %q", path)
		}
	}
}
