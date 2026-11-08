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
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/versioncheck"
)

// restartCalls records what update --restart asked to validate and restart.
type restartCalls struct {
	validated   []*installationTarget
	restarted   []serviceScope
	validateErr error
	restartErr  error
}

func withRestartSeams(t *testing.T) *restartCalls {
	t.Helper()
	calls := &restartCalls{}
	savedValidate, savedRestart := validateAsService, restartService
	t.Cleanup(func() { validateAsService, restartService = savedValidate, savedRestart })
	validateAsService = func(_ *cobra.Command, target *installationTarget) error {
		calls.validated = append(calls.validated, target)
		return calls.validateErr
	}
	restartService = func(scope serviceScope) error {
		calls.restarted = append(calls.restarted, scope)
		return calls.restartErr
	}
	return calls
}

// restartTarget returns a user installation whose health endpoint reports
// running, or none when running is empty.
func restartTarget(t *testing.T, running string) *installationTarget {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := "drain-timeout='30m'\nhealth-port=0\n"
	if running != "" {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"version":"` + running + `"}`))
		}))
		t.Cleanup(server.Close)
		host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		body = "drain-timeout='30m'\nhealth-address='" + host + "'\nhealth-port=" + port + "\n"
	}
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &installationTarget{Scope: scopeUser, BinaryPath: filepath.Join(dir, "runner"), ConfigPath: configPath}
}

func finishWith(t *testing.T, installed string, target *installationTarget, changed bool) (string, error) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	c := &cobra.Command{Use: "update"}
	c.SetContext(context.Background())
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&bytes.Buffer{})
	err := finishUpgrade(c, installed, target, changed)
	return out.String(), err
}

func TestUpdateRestartValidatesBeforeRestarting(t *testing.T) {
	calls := withRestartSeams(t)
	target := restartTarget(t, "1.0.0")

	out, err := finishWith(t, "v2.0.0", target, true)
	if err != nil || len(calls.validated) != 1 || calls.validated[0] != target || !slices.Equal(calls.restarted, []serviceScope{scopeUser}) {
		t.Fatalf("err=%v validated=%d restarted=%v; want one validation, then one restart", err, len(calls.validated), calls.restarted)
	}
	last := -1
	for _, want := range []string{"Validating the config with 2.0.0", "Restarting the user service", "up to 30m for running jobs", "Restarted the user service"} {
		at := strings.Index(out, want)
		if at <= last {
			t.Fatalf("%q missing or out of order in:\n%s", want, out)
		}
		last = at
	}
}

func TestUpdateRestartKeepsTheServiceWhenValidationFails(t *testing.T) {
	calls := withRestartSeams(t)
	calls.validateErr = errors.New("exit status 1")
	_, err := finishWith(t, "2.0.0", restartTarget(t, "1.0.0"), true)
	if err == nil || len(calls.restarted) != 0 || !strings.Contains(err.Error(), "was not restarted") {
		t.Fatalf("err=%v restarted=%v; want no restart after a failed validation", err, calls.restarted)
	}
}

func TestUpdateRestartReportsARestartFailure(t *testing.T) {
	calls := withRestartSeams(t)
	calls.restartErr = errors.New("systemctl failed")
	if _, err := finishWith(t, "2.0.0", restartTarget(t, "1.0.0"), true); err == nil || !strings.Contains(err.Error(), "systemctl failed") {
		t.Fatalf("err=%v; want the restart failure", err)
	}
}

func TestUpdateRestartLeavesAServiceThatRunsTheRelease(t *testing.T) {
	calls := withRestartSeams(t)
	out, err := finishWith(t, "v2.0.0", restartTarget(t, "2.0.0"), true)
	if err != nil || len(calls.validated)+len(calls.restarted) != 0 || !strings.Contains(out, "already runs 2.0.0") {
		t.Fatalf("err=%v calls=%+v out=%s; want nothing restarted", err, calls, out)
	}
}

func TestUpdateRestartWithUnknownHealthRestartsOnlyANewBinary(t *testing.T) {
	calls := withRestartSeams(t)
	out, err := finishWith(t, "2.0.0", restartTarget(t, ""), false)
	if err != nil || len(calls.validated)+len(calls.restarted) != 0 || !strings.Contains(out, "was not restarted") || !strings.Contains(out, "service restart") {
		t.Fatalf("unchanged binary: err=%v calls=%+v out=%s; want a hint, not a restart", err, calls, out)
	}
	if _, err := finishWith(t, "2.0.0", restartTarget(t, ""), true); err != nil || len(calls.restarted) != 1 {
		t.Fatalf("new binary: err=%v restarted=%v; want a restart", err, calls.restarted)
	}
}

func TestUpdateRestartRefusesWhatItCannotRestart(t *testing.T) {
	newUpdate := func() *cobra.Command {
		c := &cobra.Command{Use: "update"}
		c.Flags().String("binary-path", "", "")
		return c
	}
	user := &installationTarget{Scope: scopeUser, BinaryPath: "/home/ci/.local/bin/runner"}
	if err := updateRestartAllowed(newUpdate(), true, user); err == nil || !strings.Contains(err.Error(), "--check") {
		t.Errorf("--check: %v", err)
	}
	binaryOnly := newUpdate()
	if err := binaryOnly.Flags().Set("binary-path", "/usr/local/bin/runner"); err != nil {
		t.Fatal(err)
	}
	if err := updateRestartAllowed(binaryOnly, false, nil); err == nil || !strings.Contains(err.Error(), "--binary-path") {
		t.Errorf("--binary-path: %v", err)
	}
	if err := updateRestartAllowed(newUpdate(), false, nil); err == nil || !strings.Contains(err.Error(), "installed service") {
		t.Errorf("no installation: %v", err)
	}
	if os.Geteuid() != 0 {
		if err := updateRestartAllowed(newUpdate(), false, user); err != nil {
			t.Errorf("user service: %v", err)
		}
	}
}

func TestUpdateRestartRunsTheWholeUpgrade(t *testing.T) {
	calls := withRestartSeams(t)
	target := restartTarget(t, "1.0.0")
	if err := os.WriteFile(target.BinaryPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	savedDiscover, savedLatest, savedInstall, savedExecutable, savedVersion := discoverInstallations, latestUpdateRelease, installUpdateRelease, updateExecutablePath, installedBinaryVersion
	t.Cleanup(func() {
		discoverInstallations, latestUpdateRelease, installUpdateRelease = savedDiscover, savedLatest, savedInstall
		updateExecutablePath, installedBinaryVersion = savedExecutable, savedVersion
	})
	discoverInstallations = func(context.Context) []installationCandidate {
		return []installationCandidate{{Scope: scopeUser, Target: target}}
	}
	updateExecutablePath = func() (string, error) { return target.BinaryPath, nil }
	installedBinaryVersion = func(context.Context, string) (string, error) { return "1.0.0", nil }
	releases := 0
	latestUpdateRelease = func(context.Context) (*versioncheck.ReleaseInfo, error) {
		releases++
		return &versioncheck.ReleaseInfo{TagName: "v2.0.0"}, nil
	}
	installUpdateRelease = func(_ context.Context, _ string, path string) error {
		return os.WriteFile(path, []byte("new"), 0o755)
	}
	run := func(check bool) (string, error) {
		viper.Reset()
		t.Cleanup(viper.Reset)
		c := &cobra.Command{Use: "update"}
		c.SetContext(context.Background())
		c.Flags().Bool("check", check, "")
		c.Flags().Bool("restart", true, "")
		c.Flags().String("binary-path", "", "")
		c.Flags().String("config", "", "")
		addInstallationFlags(c)
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&bytes.Buffer{})
		err := runUpdate(c, nil)
		return out.String(), err
	}

	if _, err := run(true); err == nil || releases != 0 {
		t.Fatalf("--restart --check: err=%v releases checked=%d; want a refusal before the network", err, releases)
	}
	out, err := run(false)
	if err != nil || len(calls.validated) != 1 || !slices.Equal(calls.restarted, []serviceScope{scopeUser}) {
		t.Fatalf("err=%v calls=%+v out=%s; want install, validation and restart", err, calls, out)
	}
	if !strings.Contains(out, "Installed 2.0.0") || !strings.Contains(out, "Restarted the user service") || strings.Contains(out, "1. Validate") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestValidateAsServiceRunsTheBinaryWithTheServiceEnvironment(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the binary must be root-protected")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "runner")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho \"args=$*\"\necho \"scope=$RUNNER_SERVICE_SCOPE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := &installationTarget{Scope: scopeUser, BinaryPath: binary, ConfigPath: "/srv/runner/config.toml", Environment: map[string]string{"RUNNER_SERVICE_SCOPE": "user"}}
	c := &cobra.Command{Use: "update"}
	c.SetContext(context.Background())
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	if err := validateAsService(c, target); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	for _, want := range []string{"args=validate --config /srv/runner/config.toml", "scope=user"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateAsService(c, target); err == nil {
		t.Fatal("a failing validation was reported as passing")
	}
}
