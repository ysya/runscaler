package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLogFileWriterRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.log")
	w, err := openLogFile(path, 0o700, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "new\n" || string(backup) != "12345678" {
		t.Fatalf("current=%q backup=%q", current, backup)
	}
}

func TestLoggerWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.log")
	w, err := OpenLogFile(path, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	NewLoggerWithWriter("info", "json", os.Stdout, w).Info("hello-log")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello-log") {
		t.Fatalf("log contents = %q", data)
	}
}

func TestLoggerColorsConsoleButNotLogFile(t *testing.T) {
	setColorEnv(t, "1") // stands in for a color terminal
	console, read := pipeConsole(t)
	var file bytes.Buffer

	NewScaleSetLoggerWithWriter("info", "text", "linux", 0, console, &file).Warn("disk low", "free", "2GB")

	if got := read(); !strings.Contains(got, "\x1b[") {
		t.Errorf("console lost its styling: %q", got)
	}
	if got := file.String(); strings.Contains(got, "\x1b") || !strings.Contains(got, " WARN linux: disk low free=2GB\n") {
		t.Errorf("log file = %q, want plain text", got)
	}
}

func TestLoggerPlainWhenConsoleIsNotTerminal(t *testing.T) {
	setColorEnv(t, "")
	console, read := pipeConsole(t)
	var file bytes.Buffer

	NewScaleSetLoggerWithWriter("info", "text", "linux", 0, console, &file).Warn("disk low", "free", "2GB")

	if got := read(); strings.Contains(got, "\x1b") || !strings.Contains(got, " WARN linux: disk low free=2GB\n") {
		t.Errorf("console = %q, want plain text", got)
	}
}

func TestOpenLogFileRefusesSymlinkedLogFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runner.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if w, err := OpenLogFile(path, 0o700); err == nil {
		_ = w.Close()
		t.Fatal("OpenLogFile followed a symlink")
	}
	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Fatalf("symlink target changed to %q", data)
	}
}

func TestOpenLogFileCreatesDirectoryWithGivenMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "runner")
	w, err := OpenLogFile(filepath.Join(dir, "runner.log"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("log directory mode = %o, want 700", info.Mode().Perm())
	}
}

// userOnlyProblem stands in for the root check so these tests run
// unprivileged: a directory must belong to this process and be writable by
// nobody else.
func userOnlyProblem(st *unix.Stat_t) string {
	if int(st.Uid) != os.Geteuid() {
		return "is owned by someone else"
	}
	if st.Mode&0o022 != 0 {
		return "is writable by group or others"
	}
	return ""
}

// trustedTempRoot resolves symlinks in the temp path (macOS /var is one),
// since the trusted walk refuses every symlinked component.
func trustedTempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestOpenTrustedLogFileCreatesMissingDirectories(t *testing.T) {
	root := trustedTempRoot(t)
	dir := filepath.Join(root, "runner", "nested")
	w, err := openTrustedLogFile(filepath.Join(dir, "runner.log"), root, 0o755, DefaultLogMaxSize, userOnlyProblem)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "runner.log")); err != nil || string(data) != "hello\n" {
		t.Fatalf("log = %q, %v", data, err)
	}
}

func TestOpenTrustedLogFileRejectsSymlinkedDirectory(t *testing.T) {
	root := trustedTempRoot(t)
	elsewhere := trustedTempRoot(t)
	if err := os.Symlink(elsewhere, filepath.Join(root, "runner")); err != nil {
		t.Fatal(err)
	}
	if w, err := openTrustedLogFile(filepath.Join(root, "runner", "runner.log"), root, 0o755, DefaultLogMaxSize, userOnlyProblem); err == nil {
		_ = w.Close()
		t.Fatal("trusted open followed a symlinked directory")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("symlink target received files: %v", entries)
	}
}

func TestOpenTrustedLogFileRejectsUnsafeDirectory(t *testing.T) {
	root := trustedTempRoot(t)
	dir := filepath.Join(root, "runner")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	w, err := openTrustedLogFile(filepath.Join(dir, "runner.log"), root, 0o755, DefaultLogMaxSize, userOnlyProblem)
	if err == nil {
		_ = w.Close()
		t.Fatal("trusted open accepted a world-writable directory")
	}
	if !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenTrustedLogFileRequiresADirectoryBelowTheRoot(t *testing.T) {
	root := trustedTempRoot(t)
	if w, err := openTrustedLogFile(filepath.Join(root, "runner.log"), root, 0o755, DefaultLogMaxSize, userOnlyProblem); err == nil {
		_ = w.Close()
		t.Fatal("log directly in the trusted root was accepted")
	}
}

func TestTrustedLogWriterRotatesInsideTheOriginalDirectory(t *testing.T) {
	root := trustedTempRoot(t)
	dir := filepath.Join(root, "runner")
	w, err := openTrustedLogFile(filepath.Join(dir, "runner.log"), root, 0o755, 10, userOnlyProblem)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	// Replace the directory's path with a symlink to somewhere else.
	moved := filepath.Join(root, "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	decoy := trustedTempRoot(t)
	if err := os.Symlink(decoy, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("rotated\n")); err != nil { // 8+8 bytes exceeds 10 and rotates
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(filepath.Join(moved, "runner.log"))
	backup, _ := os.ReadFile(filepath.Join(moved, "runner.log.1"))
	if string(current) != "rotated\n" || string(backup) != "12345678" {
		t.Fatalf("original directory current=%q backup=%q", current, backup)
	}
	if entries, _ := os.ReadDir(decoy); len(entries) != 0 {
		t.Fatalf("rotation followed the swapped path into %s: %v", decoy, entries)
	}
}

// pipeConsole returns a pipe to use as a logger console, which is never a
// terminal, and a function that closes it and yields what was written.
func pipeConsole(t *testing.T) (*os.File, func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	return w, func() string {
		_ = w.Close()
		out, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
}

// setColorEnv pins the variables that decide whether colorprofile styles
// output, so the developer's terminal cannot leak into a test.
func setColorEnv(t *testing.T, cliColorForce string) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR", "")
	t.Setenv("CLICOLOR_FORCE", cliColorForce)
	t.Setenv("TTY_FORCE", "")
}
