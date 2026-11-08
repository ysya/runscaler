package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withPathRunner makes resolved the `runner` the invoking shell finds.
func withPathRunner(t *testing.T, resolved string) {
	t.Helper()
	saved := pathRunner
	t.Cleanup(func() { pathRunner = saved })
	pathRunner = func() string { return resolved }
}

// installedBinary creates an executable runner binary at a resolved path.
func installedBinary(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runner")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHintsNameThePathRunnerPlainly(t *testing.T) {
	binary := installedBinary(t)
	withPathRunner(t, binary)
	if got, want := (runnerCommand{binary: binary, args: []string{"validate", "--config", "/srv/config.toml"}}).String(), "runner validate --config /srv/config.toml"; got != want {
		t.Errorf("PATH runner: %q, want %q", got, want)
	}
	// sudo searches its own secure_path, not the shell's PATH.
	if got, want := (runnerCommand{sudo: true, binary: binary, args: []string{"doctor"}}).String(), "sudo "+binary+" doctor"; got != want {
		t.Errorf("sudo: %q, want %q", got, want)
	}
	withPathRunner(t, filepath.Join(filepath.Dir(binary), "another-runner"))
	if got, want := (runnerCommand{binary: binary, args: []string{"logs"}}).String(), binary+" logs"; got != want {
		t.Errorf("another runner on PATH: %q, want %q", got, want)
	}
}

func TestPathRunnerFindsTheShellsRunner(t *testing.T) {
	binary := installedBinary(t)
	saved := shellPATH
	t.Cleanup(func() { shellPATH = saved })
	shellPATH = strings.Join([]string{"relative/bin", filepath.Join(t.TempDir(), "missing"), filepath.Dir(binary)}, string(os.PathListSeparator))
	if got := pathRunner(); got != binary {
		t.Fatalf("pathRunner() = %q, want %q", got, binary)
	}
	shellPATH = filepath.Join(t.TempDir(), "empty")
	if got := pathRunner(); got != "" {
		t.Fatalf("pathRunner() = %q without a runner on PATH", got)
	}
}

func TestTargetCommandLeavesOutTheScopeWhenAutoDetectionAgrees(t *testing.T) {
	binary := installedBinary(t)
	withPathRunner(t, binary)
	user := &installationTarget{Scope: scopeUser, BinaryPath: binary, Unique: true}
	if got, want := targetCommand(user, "restart"), "runner service restart"; got != want {
		t.Errorf("only installation: %q, want %q", got, want)
	}
	user.Unique = false
	if got, want := targetCommand(user, "restart"), "runner service restart --scope user"; got != want {
		t.Errorf("one of two installations: %q, want %q", got, want)
	}
	system := &installationTarget{Scope: scopeSystem, BinaryPath: binary, Unique: true}
	if got, want := targetCommand(system, "restart"), "sudo "+binary+" service restart"; got != want {
		t.Errorf("only system installation: %q, want %q", got, want)
	}
}

func TestSelectInstallationMarksTheOnlyInstallationUnique(t *testing.T) {
	user := func() *installationTarget { return &installationTarget{Scope: scopeUser} }
	for _, tc := range []struct {
		name       string
		candidates []installationCandidate
		scope      serviceScope
		unique     bool
	}{
		{"only installation", []installationCandidate{{Scope: scopeSystem}, {Scope: scopeUser, Target: user()}}, scopeAuto, true},
		{"explicit scope of the only installation", []installationCandidate{{Scope: scopeSystem}, {Scope: scopeUser, Target: user()}}, scopeUser, true},
		{"another scope installed", []installationCandidate{{Scope: scopeSystem, Target: &installationTarget{Scope: scopeSystem}}, {Scope: scopeUser, Target: user()}}, scopeUser, false},
		{"another scope unverifiable", []installationCandidate{{Scope: scopeSystem, Err: errors.New("bus unavailable")}, {Scope: scopeUser, Target: user()}}, scopeUser, false},
	} {
		got, err := selectInstallation(tc.candidates, tc.scope)
		if err != nil || got == nil || got.Unique != tc.unique {
			t.Errorf("%s: target=%+v err=%v; want Unique=%v", tc.name, got, err, tc.unique)
		}
	}
}

func TestUpdateAndStatusHintsUseTheShortFormForTheOnlyInstallation(t *testing.T) {
	binary := installedBinary(t)
	withPathRunner(t, binary)
	target := &installationTarget{Scope: scopeUser, BinaryPath: binary, ConfigPath: "/srv/config.toml", Unique: true}

	notice := updateRestartNotice("0.16.0", "0.15.0", target, validateCommand(context.Background(), target), nil)
	for _, want := range []string{"runner validate --config /srv/config.toml", "runner service restart\n"} {
		if !strings.Contains(notice, want) {
			t.Errorf("update notice lacks %q:\n%s", want, notice)
		}
	}
	if strings.Contains(notice, "--scope") || strings.Contains(notice, binary) {
		t.Errorf("update notice keeps what auto detection makes redundant:\n%s", notice)
	}

	err := statusUnreachable(statusCommand(t, "/srv/config.toml", target), "127.0.0.1", 8080)
	for _, want := range []string{"runner service status\n", "runner service logs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("status hint lacks %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), "--scope") {
		t.Errorf("status hint keeps --scope:\n%s", err)
	}
}
