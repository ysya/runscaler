// Package lock provides the machine-wide process lock shared by runner run
// and destructive maintenance commands.
package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const DefaultPath = "/tmp/runner.lock"

var ErrAlreadyRunning = errors.New("another runner is already running")

// Info is diagnostic metadata stored in the lock file. The kernel flock is
// the only source of truth; this content may be stale or malformed.
type Info struct {
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	ConfigPath string    `json:"config_path,omitempty"`
}

// AlreadyRunningError includes best-effort information about the process that
// currently holds the lock.
type AlreadyRunningError struct {
	Path string
	Info *Info
}

func (e *AlreadyRunningError) Error() string {
	if e.Info == nil {
		return fmt.Sprintf("%s (lock %s is held)", ErrAlreadyRunning, e.Path)
	}
	detail := fmt.Sprintf("PID %d, started %s", e.Info.PID, e.Info.StartedAt.Format(time.DateTime))
	if e.Info.ConfigPath != "" {
		detail += ", config " + e.Info.ConfigPath
	}
	return fmt.Sprintf("%s on this host (%s)", ErrAlreadyRunning, detail)
}

func (e *AlreadyRunningError) Unwrap() error { return ErrAlreadyRunning }

// Acquire obtains a non-blocking exclusive flock. The file is opened with
// O_NOFOLLOW and verified as regular so a privileged service cannot be tricked
// into following a /tmp symlink. The file deliberately remains in place after
// release: unlinking a flock file can split contenders across two inodes and
// break mutual exclusion.
func Acquire(path string, info Info) (release func(), err error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o666)
	if err != nil {
		return nil, fmt.Errorf("open runner lock %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open runner lock %s: invalid file descriptor", path)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = f.Close()
		}
	}()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, fmt.Errorf("inspect runner lock %s: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("runner lock %s is not a regular file", path)
	}
	// Root-run and user-run instances must be able to contend on the same file.
	// Contents are diagnostics only and are written only after holding the lock.
	if err := unix.Fchmod(fd, 0o666); err != nil {
		return nil, fmt.Errorf("set runner lock permissions %s: %w", path, err)
	}

	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, &AlreadyRunningError{Path: path, Info: readInfo(f)}
		}
		return nil, fmt.Errorf("acquire runner lock %s: %w", path, err)
	}

	if err := writeInfo(f, info); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fmt.Errorf("write runner lock %s: %w", path, err)
	}

	closeOnError = false
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unix.Flock(fd, unix.LOCK_UN)
			_ = f.Close()
		})
	}, nil
}

func readInfo(f *os.File) *Info {
	if _, err := f.Seek(0, 0); err != nil {
		return nil
	}
	var info Info
	if err := json.NewDecoder(f).Decode(&info); err != nil {
		return nil
	}
	return &info
}

func writeInfo(f *os.File, info Info) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(info); err != nil {
		return err
	}
	return f.Sync()
}
