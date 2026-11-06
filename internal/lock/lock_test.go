package lock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func withSys(t *testing.T, replace func(*sysOps)) {
	t.Helper()
	saved := sys
	replace(&sys)
	t.Cleanup(func() { sys = saved })
}

func TestAcquireRejectsHardLinkedLockFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "precious")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runner.lock")
	if err := os.Link(target, path); err != nil {
		t.Fatal(err)
	}

	if _, err := Acquire(path, Info{PID: 1}); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("Acquire() on hard link = %v, want hard link error", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" || info.Mode().Perm() != 0o600 {
		t.Fatalf("hard link target changed: %q mode %o", data, info.Mode().Perm())
	}
}

func TestAcquireLeavesForeignOwnedLockModeAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.lock")
	if err := os.WriteFile(path, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	chmods := 0
	withSys(t, func(s *sysOps) {
		realFstat := s.fstat
		s.fstat = func(fd int, st *unix.Stat_t) error {
			err := realFstat(fd, st)
			st.Uid = uint32(os.Geteuid() + 1)
			return err
		}
		s.fchmod = func(int, uint32) error {
			chmods++
			return nil
		}
	})

	release, err := Acquire(path, Info{PID: 1})
	if err != nil {
		t.Fatalf("Acquire() on a lock owned by someone else = %v", err)
	}
	release()
	if chmods != 0 {
		t.Fatalf("fchmod called %d times on a lock this process does not own", chmods)
	}
}

func TestAcquireCreatesMissingLockExclusively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.lock")
	var modes []int
	withSys(t, func(s *sysOps) {
		realOpen := s.open
		s.open = func(p string, mode int, perm uint32) (int, error) {
			modes = append(modes, mode)
			return realOpen(p, mode, perm)
		}
	})

	release, err := Acquire(path, Info{PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(modes) != 2 || modes[0]&unix.O_CREAT != 0 || modes[1]&(unix.O_CREAT|unix.O_EXCL) != unix.O_CREAT|unix.O_EXCL {
		t.Fatalf("open modes = %#v, want plain open then O_CREAT|O_EXCL", modes)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o666 {
		t.Fatalf("created lock mode = %o, want 666", info.Mode().Perm())
	}
}

func TestAcquireRetriesWhenAnotherProcessCreatesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.lock")
	var modes []int
	withSys(t, func(s *sysOps) {
		realOpen := s.open
		s.open = func(p string, mode int, perm uint32) (int, error) {
			modes = append(modes, mode)
			switch len(modes) {
			case 1:
				return -1, unix.ENOENT
			case 2:
				// Another runner wins the create race.
				if err := os.WriteFile(p, nil, 0o666); err != nil {
					return -1, err
				}
				return -1, unix.EEXIST
			}
			return realOpen(p, mode, perm)
		}
	})

	release, err := Acquire(path, Info{PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(modes) != 3 || modes[2]&unix.O_CREAT != 0 {
		t.Fatalf("open modes = %#v, want a plain reopen after EEXIST", modes)
	}
}

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
