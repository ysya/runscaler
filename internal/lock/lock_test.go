package lock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireExclusiveAndReusable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.lock")
	info := Info{PID: 123, StartedAt: time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC), ConfigPath: "/tmp/config.toml"}

	release, err := Acquire(path, info)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	_, err = Acquire(path, Info{PID: 456})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Acquire() error = %v, want ErrAlreadyRunning", err)
	}
	var held *AlreadyRunningError
	if !errors.As(err, &held) || held.Info == nil || held.Info.PID != 123 {
		t.Fatalf("second Acquire() details = %#v, want PID 123", held)
	}

	release()
	release() // idempotent

	releaseAgain, err := Acquire(path, Info{PID: 789})
	if err != nil {
		t.Fatalf("Acquire() after release error = %v", err)
	}
	releaseAgain()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file should remain in place: %v", err)
	}
}

func TestAcquireRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runner.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if _, err := Acquire(path, Info{PID: 1}); err == nil {
		t.Fatal("Acquire() on symlink succeeded, want error")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatalf("symlink target changed to %q", data)
	}
}

func TestAcquireIgnoresCorruptDiagnosticContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.lock")
	if err := os.WriteFile(path, []byte("not-json"), 0o666); err != nil {
		t.Fatal(err)
	}

	release, err := Acquire(path, Info{PID: 42})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	release()
}
