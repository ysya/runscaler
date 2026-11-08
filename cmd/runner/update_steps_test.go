package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/versioncheck"
)

func TestUpdateRestartNoticeListsTheStepsInOrder(t *testing.T) {
	target := &installationTarget{Scope: scopeUser, BinaryPath: "/home/ci/.local/bin/runner", ConfigPath: "/home/ci/config.toml"}
	drain := 2 * time.Hour
	notice := updateRestartNotice("v0.15.0", "0.14.0", target, "VALIDATE-COMMAND", &drain)

	last := -1
	for _, want := range []string{"still running 0.14.0", "upgrading to 0.15.0", "1. Validate", "VALIDATE-COMMAND", "2. Restart", "/home/ci/.local/bin/runner service restart --scope user", "up to 2h for running jobs"} {
		at := strings.Index(notice, want)
		if at <= last {
			t.Fatalf("%q missing or out of order in:\n%s", want, notice)
		}
		last = at
	}
	if strings.Contains(notice, "v0.15.0") {
		t.Fatalf("versions are shown inconsistently:\n%s", notice)
	}
}

func TestUpdateRestartNoticeStatesTheDrainTimeout(t *testing.T) {
	target := &installationTarget{Scope: scopeUser, BinaryPath: "/opt/runner"}
	ninety, disabled := 90*time.Minute, time.Duration(0)
	for _, tc := range []struct {
		name  string
		drain *time.Duration
		want  string
	}{
		{"configured", &ninety, "Restarting waits up to 1h30m for running jobs to finish (drain-timeout)"},
		{"disabled", &disabled, "drain-timeout is disabled, so restarting stops running jobs at once"},
		{"unknown", nil, "Restarting waits for running jobs up to drain-timeout"},
	} {
		if notice := updateRestartNotice("0.5.0", "0.4.0", target, "validate", tc.drain); !strings.Contains(notice, tc.want) {
			t.Errorf("%s: notice lacks %q:\n%s", tc.name, tc.want, notice)
		}
	}
}

func TestValidateCommandRunsAsTheService(t *testing.T) {
	saved := installationOutput
	t.Cleanup(func() { installationOutput = saved })
	ctx := context.Background()

	user := &installationTarget{Scope: scopeUser, BinaryPath: "/home/ci/.local/bin/runner", ConfigPath: "/home/ci/config.toml"}
	if got := validateCommand(ctx, user); got != "/home/ci/.local/bin/runner validate --config /home/ci/config.toml" {
		t.Errorf("user service: %q", got)
	}

	system := &installationTarget{Scope: scopeSystem, RuntimeUser: "runscaler", RuntimeGroup: "runscaler", BinaryPath: "/usr/local/bin/runner", ConfigPath: "/etc/runner/config.toml"}
	installationOutput = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("--wait --pipe --collect --expand-environment"), nil
	}
	if got := validateCommand(ctx, system); !strings.HasPrefix(got, "sudo 'systemd-run'") || !strings.Contains(got, "'validate' '--config' '/etc/runner/config.toml'") {
		t.Errorf("system service with systemd-run: %q, want its identity reproduced", got)
	}

	installationOutput = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("systemd-run unavailable")
	}
	plain := "sudo /usr/local/bin/runner validate --config /etc/runner/config.toml"
	if got := validateCommand(ctx, system); got != plain {
		t.Errorf("system service without systemd-run: %q, want %q", got, plain)
	}
	root := &installationTarget{Scope: scopeSystem, RuntimeUser: "root", BinaryPath: "/usr/local/bin/runner", ConfigPath: "/etc/runner/config.toml"}
	if got := validateCommand(ctx, root); got != plain {
		t.Errorf("root system service: %q, want %q", got, plain)
	}
}

// upToDateUpdate runs `runner update` with the installed binary already at
// the latest release while the selected service reports running.
func upToDateUpdate(t *testing.T, running string) string {
	t.Helper()
	savedDiscover, savedLatest, savedInstall, savedExecutable, savedVersion, savedCurrent := discoverInstallations, latestUpdateRelease, installUpdateRelease, updateExecutablePath, installedBinaryVersion, version
	t.Cleanup(func() {
		discoverInstallations, latestUpdateRelease, installUpdateRelease = savedDiscover, savedLatest, savedInstall
		updateExecutablePath, installedBinaryVersion, version = savedExecutable, savedVersion, savedCurrent
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"` + running + `"}`))
	}))
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary, configPath := filepath.Join(dir, "runner"), filepath.Join(dir, "config.toml")
	if err := os.WriteFile(binary, []byte("current"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("health-address='127.0.0.1'\nhealth-port="+port+"\ndrain-timeout='45m'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatal(err)
	}
	discoverInstallations = func(context.Context) []installationCandidate {
		return []installationCandidate{{Scope: scopeUser, Target: &installationTarget{Scope: scopeUser, BinaryPath: binary, ConfigPath: configPath}}}
	}
	updateExecutablePath = func() (string, error) { return binary, nil }
	version = "2.0.0"
	installedBinaryVersion = func(context.Context, string) (string, error) { return "2.0.0", nil }
	latestUpdateRelease = func(context.Context) (*versioncheck.ReleaseInfo, error) {
		return &versioncheck.ReleaseInfo{TagName: "v2.0.0"}, nil
	}
	installUpdateRelease = func(context.Context, string, string) error {
		t.Fatal("an up-to-date binary was downloaded again")
		return nil
	}
	c := &cobra.Command{Use: "update"}
	c.SetContext(t.Context())
	c.Flags().Bool("check", false, "")
	c.Flags().String("binary-path", "", "")
	c.Flags().String("config", "", "")
	addInstallationFlags(c)
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&bytes.Buffer{})
	if err := runUpdate(c, nil); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestUpdateWhenUpToDateRemindsAboutAPendingRestart(t *testing.T) {
	out := upToDateUpdate(t, "1.0.0")
	for _, want := range []string{"up to date (2.0.0)", "still running 1.0.0", " service restart\n", "up to 45m for running jobs"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestUpdateWhenUpToDateStaysQuietOnceTheServiceRunsIt(t *testing.T) {
	out := upToDateUpdate(t, "v2.0.0")
	if !strings.Contains(out, "up to date (2.0.0)") || strings.Contains(out, "service restart") {
		t.Fatalf("output:\n%s", out)
	}
}
