package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogFileWriterRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.log")
	w, err := openLogFile(path, 10)
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
	w, err := OpenLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	logger := NewLoggerWithWriter("info", "json", w)
	logger.Info("hello-log")
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

func TestLoggerColorsStdoutButNotLogFile(t *testing.T) {
	setColorEnv(t, "1") // stands in for a color terminal on stdout
	stdout := captureStdout(t)
	var file bytes.Buffer

	NewScaleSetLoggerWithWriter("info", "text", "linux", 0, &file).Warn("disk low", "free", "2GB")

	if got := stdout(); !strings.Contains(got, "\x1b[") {
		t.Errorf("stdout lost its styling: %q", got)
	}
	if got := file.String(); strings.Contains(got, "\x1b") || !strings.Contains(got, " WARN linux: disk low free=2GB\n") {
		t.Errorf("log file = %q, want plain text", got)
	}
}

func TestLoggerPlainWhenStdoutIsNotTerminal(t *testing.T) {
	// A color-capable TERM with stdout on a pipe, as under systemd, launchd,
	// docker run without -t, or CI.
	setColorEnv(t, "")
	stdout := captureStdout(t)
	var file bytes.Buffer

	NewScaleSetLoggerWithWriter("info", "text", "linux", 0, &file).Warn("disk low", "free", "2GB")

	if got := stdout(); strings.Contains(got, "\x1b") || !strings.Contains(got, " WARN linux: disk low free=2GB\n") {
		t.Errorf("stdout = %q, want plain text", got)
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

// captureStdout points os.Stdout at a pipe, which is never a terminal, and
// returns a function that restores it and yields what was written.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })
	return func() string {
		os.Stdout = orig
		_ = w.Close()
		out, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
}
