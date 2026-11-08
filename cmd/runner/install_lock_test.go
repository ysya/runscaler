package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	runnerlock "github.com/ysya/runscaler/internal/lock"
)

// withInstallations replaces installation discovery and the machine-wide lock
// for one test, so it never reads the host's services or /tmp/runner.lock.
// It returns the temporary lock path.
func withInstallations(t *testing.T, candidates []installationCandidate) string {
	t.Helper()
	savedDiscover, savedLock := discoverInstallations, runnerLockPath
	t.Cleanup(func() { discoverInstallations, runnerLockPath = savedDiscover, savedLock })
	discoverInstallations = func(context.Context) []installationCandidate { return candidates }
	runnerLockPath = filepath.Join(t.TempDir(), "runner.lock")
	return runnerLockPath
}

func installationCommand(t *testing.T, name string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: name}
	c.SetErr(&bytes.Buffer{})
	addInstallationFlags(c)
	return c
}

func holdRunnerLock(t *testing.T, path string, info runnerlock.Info) {
	t.Helper()
	release, err := runnerlock.Acquire(path, info)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
}

func TestFreshInstallRefusesWhileAnotherRunnerHoldsTheLock(t *testing.T) {
	// Discovery sees no installation, as when another account's user service
	// runs the runner; that runner still holds the machine-wide lock.
	lockPath := withInstallations(t, []installationCandidate{{Scope: scopeSystem}, {Scope: scopeUser}})
	holdRunnerLock(t, lockPath, runnerlock.Info{PID: 4242, StartedAt: time.Now(), ConfigPath: "/home/other/runner/config.toml"})

	for _, scope := range []string{"auto", "user", "system"} {
		c := installationCommand(t, "install")
		if err := c.Flags().Set("scope", scope); err != nil {
			t.Fatal(err)
		}
		_, target, err := serviceCommandInstallation(c, true)
		if !errors.Is(err, runnerlock.ErrAlreadyRunning) || target != nil {
			t.Fatalf("--scope %s: target=%+v err=%v; want a refusal while another runner holds the lock", scope, target, err)
		}
		for _, want := range []string{"PID 4242", "/home/other/runner/config.toml", "no service was installed"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("--scope %s: error %q lacks %q", scope, err, want)
			}
		}
	}
}

func TestFreshInstallReleasesTheLockItProbed(t *testing.T) {
	lockPath := withInstallations(t, []installationCandidate{{Scope: scopeSystem}, {Scope: scopeUser}})

	scope, target, err := serviceCommandInstallation(installationCommand(t, "install"), true)
	if err != nil || target != nil || scope != defaultScope() {
		t.Fatalf("scope=%v target=%+v err=%v; want a fresh install in the default scope", scope, target, err)
	}
	// The service the installation starts must be able to take the lock.
	release, err := runnerlock.Acquire(lockPath, runnerlock.Info{PID: os.Getpid(), StartedAt: time.Now()})
	if err != nil {
		t.Fatalf("installation kept the runner lock: %v", err)
	}
	release()
}

func TestInstalledServiceHoldingTheLockStaysManageable(t *testing.T) {
	installed := &installationTarget{Scope: scopeUser, BinaryPath: "/home/runscaler/.local/bin/runner", ConfigPath: "/home/runscaler/config.toml"}
	lockPath := withInstallations(t, []installationCandidate{{Scope: scopeSystem}, {Scope: scopeUser, Target: installed}})
	holdRunnerLock(t, lockPath, runnerlock.Info{PID: 4242, StartedAt: time.Now(), ConfigPath: installed.ConfigPath})

	// Regenerating the installed service (install --force) and managing it
	// must not trip over the lock its own runner holds.
	for _, tc := range []struct {
		name    string
		install bool
	}{{"install", true}, {"restart", false}} {
		scope, target, err := serviceCommandInstallation(installationCommand(t, tc.name), tc.install)
		if err != nil || target != installed || scope != scopeUser {
			t.Fatalf("%s: scope=%v target=%+v err=%v; want the installed service despite its own lock", tc.name, scope, target, err)
		}
	}
}

func TestFreshInstallRefusesWhenTheLockCannotBeChecked(t *testing.T) {
	lockPath := withInstallations(t, []installationCandidate{{Scope: scopeSystem}, {Scope: scopeUser}})
	// The lock refuses to follow a symlink, so it cannot be probed.
	if err := os.Symlink(filepath.Join(filepath.Dir(lockPath), "elsewhere"), lockPath); err != nil {
		t.Fatal(err)
	}

	_, target, err := serviceCommandInstallation(installationCommand(t, "install"), true)
	if err == nil || target != nil || errors.Is(err, runnerlock.ErrAlreadyRunning) || !strings.Contains(err.Error(), "cannot check for a running runner") {
		t.Fatalf("target=%+v err=%v; want installation to stop when the runner lock cannot be checked", target, err)
	}
}
