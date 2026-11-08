package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runnerlock "github.com/ysya/runscaler/internal/lock"
)

// withRunnerLock points the machine-wide lock at a temporary file for one
// test and returns its path.
func withRunnerLock(t *testing.T) string {
	t.Helper()
	saved := runnerLockPath
	t.Cleanup(func() { runnerLockPath = saved })
	runnerLockPath = filepath.Join(t.TempDir(), "runner.lock")
	return runnerLockPath
}

// captureStdout returns what fn prints to standard output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	func() {
		defer func() { os.Stdout = saved }()
		fn()
	}()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func doctorCheck(t *testing.T, fix bool) (resourceCheck, func(), string, error) {
	t.Helper()
	var (
		mode    resourceCheck
		release func()
		err     error
	)
	out := captureStdout(t, func() { mode, release, err = doctorResourceCheck(fix) })
	return mode, release, out, err
}

func TestDoctorDiagnosisTreatsRunnerResourcesAsInUseWhileARunnerRuns(t *testing.T) {
	lockPath := withRunnerLock(t)
	holdRunnerLock(t, lockPath, runnerlock.Info{PID: 4242, StartedAt: time.Now(), ConfigPath: "/etc/runner/config.toml"})

	mode, release, out, err := doctorCheck(t, false)
	if err != nil || mode != reportInUse || release != nil {
		t.Fatalf("mode=%v holds lock=%v err=%v; want in-use reporting without holding the lock", mode, release != nil, err)
	}
	for _, want := range []string{"runner is running (PID 4242", "config /etc/runner/config.toml", "not reported as orphans"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}
}

func TestDoctorDiagnosisReportsOrphansAndFreesTheLockWhenNoRunnerRuns(t *testing.T) {
	lockPath := withRunnerLock(t)

	mode, release, _, err := doctorCheck(t, false)
	if err != nil || mode != reportOrphans || release != nil {
		t.Fatalf("mode=%v holds lock=%v err=%v; want orphan reporting", mode, release != nil, err)
	}
	// A runner may start right after the diagnosis.
	free, err := runnerlock.Acquire(lockPath, runnerlock.Info{PID: os.Getpid(), StartedAt: time.Now()})
	if err != nil {
		t.Fatalf("diagnosis kept the runner lock: %v", err)
	}
	free()
}

func TestDoctorDiagnosisWarnsWhenItCannotTellWhetherARunnerRuns(t *testing.T) {
	lockPath := withRunnerLock(t)
	// The lock refuses to follow a symlink, so it cannot be probed.
	if err := os.Symlink(filepath.Join(filepath.Dir(lockPath), "elsewhere"), lockPath); err != nil {
		t.Fatal(err)
	}

	mode, release, out, err := doctorCheck(t, false)
	if err != nil || mode != reportOrphans || release != nil {
		t.Fatalf("mode=%v holds lock=%v err=%v; want orphan reporting after a warning", mode, release != nil, err)
	}
	if !strings.Contains(out, "Cannot tell whether runner is running") {
		t.Fatalf("output %q lacks the warning", out)
	}
}

func TestDoctorFixHoldsTheLockAndRefusesWhileARunnerRuns(t *testing.T) {
	lockPath := withRunnerLock(t)

	mode, release, _, err := doctorCheck(t, true)
	if err != nil || mode != removeOrphans || release == nil {
		t.Fatalf("mode=%v holds lock=%v err=%v; want removal under the lock", mode, release != nil, err)
	}
	if _, err := runnerlock.Acquire(lockPath, runnerlock.Info{PID: os.Getpid(), StartedAt: time.Now()}); !errors.Is(err, runnerlock.ErrAlreadyRunning) {
		t.Fatalf("a runner could start during cleanup: %v", err)
	}
	release()

	holdRunnerLock(t, lockPath, runnerlock.Info{PID: 4242, StartedAt: time.Now()})
	_, release, _, err = doctorCheck(t, true)
	if err == nil || release != nil || !strings.Contains(err.Error(), "cannot fix while runner is running") {
		t.Fatalf("holds lock=%v err=%v; want --fix refused while a runner runs", release != nil, err)
	}
}

func runnerOwnedContainers() *orphanFake {
	return &orphanFake{containers: []fakeContainer{
		{id: "a1", names: []string{"/runner-deadbeef"}, labels: map[string]string{"managed-by": "runner"}},
		{id: "b2", names: []string{"/runner-0011aabb"}, labels: map[string]string{"managed-by": "runner"}},
	}}
}

func TestCheckDockerContainersFollowsTheResourceCheck(t *testing.T) {
	ctx := context.Background()

	inUse := runnerOwnedContainers()
	var issues int
	var err error
	out := captureStdout(t, func() { issues, err = checkDockerContainers(ctx, inUse, reportInUse) })
	if err != nil || issues != 0 || len(inUse.removed) != 0 {
		t.Fatalf("in use: issues=%d removed=%v err=%v; want no issues and nothing removed", issues, inUse.removed, err)
	}
	if !strings.Contains(out, "2 Docker container(s) belong to the running runner") {
		t.Fatalf("in-use output %q", out)
	}

	orphans := runnerOwnedContainers()
	captureStdout(t, func() { issues, err = checkDockerContainers(ctx, orphans, reportOrphans) })
	if err != nil || issues != 2 || len(orphans.removed) != 0 {
		t.Fatalf("report: issues=%d removed=%v err=%v; want 2 orphans reported, nothing removed", issues, orphans.removed, err)
	}

	removal := runnerOwnedContainers()
	captureStdout(t, func() { issues, err = checkDockerContainers(ctx, removal, removeOrphans) })
	if err != nil || issues != 0 || len(removal.removed) != 2 {
		t.Fatalf("fix: issues=%d removed=%v err=%v; want both removed", issues, removal.removed, err)
	}
}

func TestCheckTartVMsLeavesARunningRunnersVMsAlone(t *testing.T) {
	mc := &mockCommandRunner{}
	mc.setResult("tart list --format json", `[{"Name":"runner-deadbeef","Source":"local","State":"running"}]`)

	var issues int
	var err error
	out := captureStdout(t, func() { issues, err = checkTartVMs(context.Background(), mc, "/Users/ci/.tart", reportInUse) })
	if err != nil || issues != 0 || len(mc.calls) != 1 {
		t.Fatalf("issues=%d calls=%v err=%v; want only the listing and no issues", issues, mc.calls, err)
	}
	if !strings.Contains(out, "1 Tart VM(s) belong to the running runner") {
		t.Fatalf("in-use output %q", out)
	}
}
