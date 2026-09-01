package main

import (
	"fmt"
	"os"
	"path/filepath"
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
