package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/colorprofile"
	"golang.org/x/sys/unix"

	"github.com/ysya/runscaler/internal/pathtrust"
)

const (
	DefaultLogFileName = "runner.log"
	DefaultLogMaxSize  = int64(10 * 1024 * 1024)
)

// LogFileWriter is a concurrency-safe rotating writer shared by every logger
// in a process. Rotation keeps one backup at name + ".1". Every open, rename
// and unlink is relative to the directory opened at start, so replacing that
// directory's path afterwards cannot redirect the log.
type LogFileWriter struct {
	mu      sync.Mutex
	dir     *os.File
	name    string
	maxSize int64
	file    *os.File
	size    int64
}

// OpenLogFile opens path for appending, creating its directory with dirPerm.
func OpenLogFile(path string, dirPerm fs.FileMode) (*LogFileWriter, error) {
	return openLogFile(path, dirPerm, DefaultLogMaxSize)
}

func openLogFile(path string, dirPerm fs.FileMode, maxSize int64) (*LogFileWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	return newLogFileWriter(dir, filepath.Base(path), maxSize)
}

// OpenRootLogFile opens path for a root writer. Every component from / is
// opened without following symlinks; directories below trustedRoot are
// created with dirPerm when missing and must be changeable only by root.
// trustedRoot itself may be group-writable (/var/log is on some
// distributions): once the writer holds its directory, renaming anything
// above it cannot redirect writes or rotation.
func OpenRootLogFile(path, trustedRoot string, dirPerm fs.FileMode) (*LogFileWriter, error) {
	return openTrustedLogFile(path, trustedRoot, dirPerm, DefaultLogMaxSize, rootOnlyProblem)
}

func rootOnlyProblem(st *unix.Stat_t) string {
	return pathtrust.Problem(st.Uid, fs.FileMode(st.Mode&0o777))
}

func openTrustedLogFile(path, trustedRoot string, dirPerm fs.FileMode, maxSize int64, problem func(*unix.Stat_t) string) (*LogFileWriter, error) {
	dir, err := openTrustedDir(filepath.Dir(path), trustedRoot, dirPerm, problem)
	if err != nil {
		return nil, err
	}
	return newLogFileWriter(dir, filepath.Base(path), maxSize)
}

// openTrustedDir walks from / to dirPath opening each component with
// O_NOFOLLOW, creating and vetting the components below trustedRoot.
func openTrustedDir(dirPath, trustedRoot string, dirPerm fs.FileMode, problem func(*unix.Stat_t) string) (*os.File, error) {
	dirPath = filepath.Clean(dirPath)
	trustedRoot = filepath.Clean(trustedRoot)
	rel, err := filepath.Rel(trustedRoot, dirPath)
	if !filepath.IsAbs(dirPath) || err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("log directory %s is not below %s", dirPath, trustedRoot)
	}
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /: %w", err)
	}
	current := "/"
	for _, name := range strings.Split(strings.TrimPrefix(dirPath, "/"), "/") {
		next := filepath.Join(current, name)
		below := strings.HasPrefix(next, trustedRoot+"/")
		child, err := unix.Openat(fd, name, flags, 0)
		if errors.Is(err, unix.ENOENT) && below {
			if mkErr := unix.Mkdirat(fd, name, uint32(dirPerm.Perm())); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				_ = unix.Close(fd)
				return nil, fmt.Errorf("create %s: %w", next, mkErr)
			}
			child, err = unix.Openat(fd, name, flags, 0)
		}
		_ = unix.Close(fd)
		if err != nil {
			return nil, fmt.Errorf("open %s without following symlinks: %w", next, err)
		}
		fd = child
		if below {
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				_ = unix.Close(fd)
				return nil, fmt.Errorf("inspect %s: %w", next, err)
			}
			if reason := problem(&st); reason != "" {
				_ = unix.Close(fd)
				return nil, fmt.Errorf("log directory %s %s", next, reason)
			}
		}
		current = next
	}
	return os.NewFile(uintptr(fd), dirPath), nil
}

func newLogFileWriter(dir *os.File, name string, maxSize int64) (*LogFileWriter, error) {
	w := &LogFileWriter{dir: dir, name: name, maxSize: maxSize}
	f, err := w.openAt(unix.O_APPEND)
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = dir.Close()
		return nil, err
	}
	w.file, w.size = f, info.Size()
	return w, nil
}

// openAt opens the log inside the writer's directory. O_NOFOLLOW refuses a
// symlink planted at the log's own name.
func (w *LogFileWriter) openAt(mode int) (*os.File, error) {
	path := filepath.Join(w.dir.Name(), w.name)
	fd, err := unix.Openat(int(w.dir.Fd()), w.name, unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|mode, 0o640)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func (w *LogFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.maxSize > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, fmt.Errorf("rotate log: %w", err)
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *LogFileWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	dirfd := int(w.dir.Fd())
	backup := w.name + ".1"
	_ = unix.Unlinkat(dirfd, backup, 0)
	if err := unix.Renameat(dirfd, w.name, dirfd, backup); err != nil && !errors.Is(err, unix.ENOENT) {
		w.file, _ = w.openAt(unix.O_APPEND)
		return err
	}
	f, err := w.openAt(unix.O_TRUNC)
	if err != nil {
		_ = unix.Renameat(dirfd, backup, dirfd, w.name)
		w.file, _ = w.openAt(unix.O_APPEND)
		return err
	}
	w.file, w.size = f, 0
	return nil
}

func (w *LogFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var errs []error
	if w.file != nil {
		errs = append(errs, w.file.Close())
		w.file = nil
	}
	if w.dir != nil {
		errs = append(errs, w.dir.Close())
		w.dir = nil
	}
	return errors.Join(errs...)
}

// outputWriter tees log output to the console and, when enabled, the log
// file. The file copy is stripped of styling so runner.log stays plain text.
func outputWriter(console *os.File, file io.Writer) io.Writer {
	if file == nil {
		return console
	}
	return io.MultiWriter(console, &colorprofile.Writer{Forward: file, Profile: colorprofile.NoTTY})
}

// consoleColorProfile picks log styling from the console itself: color on an
// interactive terminal, plain text on the pipes, sockets and files that
// systemd, launchd, docker run without -t and CI provide. charmlog would
// otherwise probe its writer, and the tee outputWriter builds for a log file
// is never a terminal. NO_COLOR, CLICOLOR_FORCE and TERM=dumb are honored.
func consoleColorProfile(console *os.File) colorprofile.Profile {
	return colorprofile.Detect(console, os.Environ())
}
