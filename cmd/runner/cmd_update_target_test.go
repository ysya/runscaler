package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/ysya/runscaler/internal/versioncheck"
)

func TestUpdateUsesInstalledBinaryAndItsVersion(t *testing.T) {
	savedDiscover, savedLatest, savedInstall, savedExecutable, savedVersion := discoverInstallations, latestUpdateRelease, installUpdateRelease, updateExecutablePath, installedBinaryVersion
	savedCurrent := version
	t.Cleanup(func() {
		discoverInstallations = savedDiscover
		latestUpdateRelease = savedLatest
		installUpdateRelease = savedInstall
		updateExecutablePath = savedExecutable
		installedBinaryVersion = savedVersion
		version = savedCurrent
	})
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	invoking := filepath.Join(dir, "invoking-runner")
	installed := filepath.Join(dir, "service-runner")
	configPath := filepath.Join(dir, "config.toml")
	for _, path := range []string{invoking, installed} {
		if err := os.WriteFile(path, []byte("old"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(configPath, []byte("health-port=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	discoverInstallations = func(context.Context) []installationCandidate {
		return []installationCandidate{{Scope: scopeUser, Target: &installationTarget{Scope: scopeUser, BinaryPath: installed, ConfigPath: configPath}}}
	}
	updateExecutablePath = func() (string, error) { return invoking, nil }
	version = "2.0.0"
	installedBinaryVersion = func(_ context.Context, path string) (string, error) {
		if path != installed {
			t.Fatalf("version read from wrong executable: %s", path)
		}
		return "1.0.0", nil
	}
	latestUpdateRelease = func(context.Context) (*versioncheck.ReleaseInfo, error) {
		return &versioncheck.ReleaseInfo{TagName: "v2.0.0"}, nil
	}
	var out bytes.Buffer
	installedCalls := 0
	installUpdateRelease = func(_ context.Context, tag, path string) error {
		installedCalls++
		if !strings.Contains(out.String(), "does not check the config") || !strings.Contains(out.String(), "#upgrade-requirements") {
			t.Fatal("compatibility notice was not printed before replacement")
		}
		if tag != "v2.0.0" || path != installed {
			t.Fatalf("update target=%s %s", tag, path)
		}
		return os.WriteFile(path, []byte("new"), 0755)
	}
	c := &cobra.Command{Use: "update"}
	c.Flags().Bool("check", false, "")
	c.Flags().String("binary-path", "", "")
	c.Flags().String("config", "", "")
	addInstallationFlags(c)
	c.SetOut(&out)
	c.SetErr(&bytes.Buffer{})
	_ = c.Flags().Set("check", "true")
	if err := runUpdate(c, nil); err != nil {
		t.Fatal(err)
	}
	if installedCalls != 0 || !strings.Contains(out.String(), "Check only") || !strings.Contains(out.String(), "releases/tag/v2.0.0") {
		t.Fatalf("check-only notice/install count=%d: %s", installedCalls, out.String())
	}
	out.Reset()
	_ = c.Flags().Set("check", "false")
	if err := runUpdate(c, nil); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(invoking)
	b, _ := os.ReadFile(installed)
	if string(a) != "old" || string(b) != "new" {
		t.Fatalf("wrong binary updated: invoking=%s installed=%s", a, b)
	}
	validate := runnerCommand{binary: installed, args: []string{"validate", "--config", configPath}}.String()
	for _, want := range []string{"Installed 2.0.0 at " + installed, "checksum verified", "1. Validate", validate, "2. Restart", "--scope user"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing target-aware guidance %q: %s", want, out.String())
		}
	}
}

func TestUpdateBinaryOnlyModeRejectsScopeOverrideBeforeNetwork(t *testing.T) {
	c := &cobra.Command{Use: "update"}
	c.Flags().Bool("check", false, "")
	c.Flags().String("binary-path", "", "")
	addInstallationFlags(c)
	_ = c.Flags().Set("binary-path", "/usr/local/bin/runner")
	_ = c.Flags().Set("scope", "system")
	if _, _, err := resolveUpdateDestination(c, "/other/runner"); err == nil {
		t.Fatal("binary-only update accepted a conflicting scope override")
	}
}

func TestUpdateBinaryOnlyDoesNotProbeAnotherManagersHealth(t *testing.T) {
	savedDiscover, savedLatest, savedInstall, savedExecutable, savedCurrent := discoverInstallations, latestUpdateRelease, installUpdateRelease, updateExecutablePath, version
	t.Cleanup(func() {
		discoverInstallations = savedDiscover
		latestUpdateRelease = savedLatest
		installUpdateRelease = savedInstall
		updateExecutablePath = savedExecutable
		version = savedCurrent
	})
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"version":"v1.0.0"}`))
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "runner")
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(binary, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("health-address='127.0.0.1'\nhealth-port="+strconv.Itoa(number)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	discoverInstallations = func(context.Context) []installationCandidate {
		t.Fatal("explicit binary-only update discovered a service")
		return nil
	}
	updateExecutablePath = func() (string, error) { return binary, nil }
	version = "1.0.0"
	latestUpdateRelease = func(context.Context) (*versioncheck.ReleaseInfo, error) {
		return &versioncheck.ReleaseInfo{TagName: "v2.0.0"}, nil
	}
	installUpdateRelease = func(context.Context, string, string) error { return nil }
	c := &cobra.Command{Use: "update"}
	c.SetContext(t.Context())
	c.Flags().Bool("check", false, "")
	c.Flags().String("binary-path", binary, "")
	c.Flags().String("config", path, "")
	addInstallationFlags(c)
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&bytes.Buffer{})
	if err := runUpdate(c, nil); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 || strings.Contains(out.String(), "service restart") {
		t.Fatalf("binary-only update observed/controlled another manager: requests=%d output=%s", requests.Load(), out.String())
	}
	if !strings.Contains(out.String(), "No service was selected") {
		t.Fatalf("binary-only scope was not explained: %s", out.String())
	}
}

func TestUpdatePermissionHintUsesTheCurrentUpdaterNotTheOlderTarget(t *testing.T) {
	trusted := func(string) (fileStat, error) { return fileStat{UID: 0, Mode: 0755}, nil }
	got := updatePermissionHint("/opt/new runner", "/usr/local/bin/old-runner", trusted)
	if !strings.Contains(got, "sudo '/opt/new runner' update --binary-path /usr/local/bin/old-runner") || strings.Contains(got, "sudo /usr/local/bin/old-runner update") {
		t.Fatalf("older target used as updater: %s", got)
	}
	untrusted := func(string) (fileStat, error) { return fileStat{UID: 1000, Mode: 0755}, nil }
	got = updatePermissionHint("/home/ci/new-runner", "/usr/local/bin/old-runner", untrusted)
	if !strings.Contains(got, "root-protected") || strings.Contains(got, "sudo /home/ci/new-runner") {
		t.Fatalf("unprotected updater recommended for root: %s", got)
	}
}

func TestUpdatePermissionFailureHappensBeforeNetwork(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged caller")
	}
	savedDiscover, savedLatest, savedExecutable := discoverInstallations, latestUpdateRelease, updateExecutablePath
	t.Cleanup(func() {
		discoverInstallations = savedDiscover
		latestUpdateRelease = savedLatest
		updateExecutablePath = savedExecutable
	})
	dir := t.TempDir()
	updater := filepath.Join(dir, "new-runner")
	protected := filepath.Join(dir, "protected")
	if err := os.Mkdir(protected, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(protected, "old-runner")
	for _, path := range []string{updater, target} {
		if err := os.WriteFile(path, []byte("unchanged"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(protected, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(protected, 0755) })
	discoverInstallations = func(context.Context) []installationCandidate {
		return []installationCandidate{{Scope: scopeSystem, Target: &installationTarget{Scope: scopeSystem, BinaryPath: target}}}
	}
	updateExecutablePath = func() (string, error) { return updater, nil }
	calls := 0
	latestUpdateRelease = func(context.Context) (*versioncheck.ReleaseInfo, error) {
		calls++
		return nil, errors.New("unexpected network")
	}
	c := &cobra.Command{Use: "update"}
	c.Flags().Bool("check", false, "")
	c.Flags().String("binary-path", "", "")
	addInstallationFlags(c)
	err := runUpdate(c, nil)
	if !errors.Is(err, fs.ErrPermission) || calls != 0 || !strings.Contains(err.Error(), updater) {
		t.Fatalf("preflight err=%v, requests=%d", err, calls)
	}
}
