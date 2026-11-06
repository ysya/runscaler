package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/layout"
)

func TestLogWriterKeepsDisabledFileLoggingNil(t *testing.T) {
	var disabled *config.LogFileWriter
	if logWriter(disabled) != nil {
		t.Fatal("a nil *LogFileWriter became a non-nil io.Writer")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	// Before the fix every log line panicked on the typed-nil file writer.
	config.NewLoggerWithWriter("info", "text", w, logWriter(disabled)).Info("still logs")
}

func TestResolveLogFile(t *testing.T) {
	str := func(s string) *string { return &s }
	user := layout.Identity{GOOS: "linux", Home: "/home/ada"}
	homeless := layout.Identity{GOOS: "linux"}
	root := layout.Identity{GOOS: "linux", Root: true}
	tests := []struct {
		name        string
		id          layout.Identity
		logFile     *string
		want        logFileDecision
		wantWarning string
	}{
		{name: "user default", id: user, want: logFileDecision{Path: "/home/ada/.local/state/runner/runner.log", Enabled: true}},
		{name: "user explicit is kept anywhere", id: user, logFile: str("/home/ada/runner/runner.log"), want: logFileDecision{Path: "/home/ada/runner/runner.log", Enabled: true}},
		{name: "disabled", id: user, logFile: str("")},
		{name: "user without a default location", id: homeless, wantWarning: "no default log location"},
		{name: "user without home keeps an explicit path", id: homeless, logFile: str("/srv/runner.log"), want: logFileDecision{Path: "/srv/runner.log", Enabled: true}},
		{name: "root default", id: root, want: logFileDecision{Path: "/var/log/runner/runner.log", Enabled: true}},
		{name: "root explicit under a var log directory keeps a fallback", id: root, logFile: str("/var/log/custom/runner.log"), want: logFileDecision{Path: "/var/log/custom/runner.log", Fallback: "/var/log/runner/runner.log", Enabled: true}},
		{name: "root explicit outside var log", id: root, logFile: str("/home/ada/runner/runner.log"), want: logFileDecision{Path: "/var/log/runner/runner.log", Enabled: true}, wantWarning: "directory under /var/log"},
		{name: "root explicit directly in var log", id: root, logFile: str("/var/log/runner.log"), want: logFileDecision{Path: "/var/log/runner/runner.log", Enabled: true}, wantWarning: "directory under /var/log"},
		{name: "root explicit with characters systemd would need escaped", id: root, logFile: str("/var/log/has space/runner.log"), want: logFileDecision{Path: "/var/log/runner/runner.log", Enabled: true}, wantWarning: "directory under /var/log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveLogFile(config.Config{LogFile: tt.logFile}, tt.id)
			warning := got.Warning
			got.Warning = ""
			if got != tt.want {
				t.Errorf("resolveLogFile() = %+v, want %+v", got, tt.want)
			}
			if (tt.wantWarning == "") != (warning == "") || !strings.Contains(warning, tt.wantWarning) {
				t.Errorf("warning = %q, want it to contain %q", warning, tt.wantWarning)
			}
		})
	}
}

func TestOpenRunLogFallsBackOnce(t *testing.T) {
	good := filepath.Join(t.TempDir(), "runner.log")
	saved := openLogFileFor
	t.Cleanup(func() { openLogFileFor = saved })
	var tried []string
	openLogFileFor = func(_ layout.Identity, path string) (*config.LogFileWriter, error) {
		tried = append(tried, path)
		if path == good {
			return config.OpenLogFile(path, 0o700)
		}
		return nil, errors.New("unsafe directory")
	}

	w, path, warnings := openRunLog(layout.Identity{Root: true}, logFileDecision{Path: "/var/log/custom/runner.log", Fallback: good, Enabled: true})
	if w == nil || path != good {
		t.Fatalf("openRunLog() = %v, %q; want the fallback", w, path)
	}
	_ = w.Close()
	if strings.Join(tried, ",") != "/var/log/custom/runner.log,"+good || len(warnings) != 2 {
		t.Fatalf("tried %v, warnings %q", tried, warnings)
	}

	tried = nil
	w, path, warnings = openRunLog(layout.Identity{}, logFileDecision{Path: "/nowhere/runner.log", Enabled: true})
	if w != nil || path != "" || len(tried) != 1 || len(warnings) != 1 {
		t.Fatalf("without fallback: w=%v path=%q tried=%v warnings=%q", w, path, tried, warnings)
	}
}

func TestOldDefaultLogFile(t *testing.T) {
	dir := t.TempDir()
	usedConfig := filepath.Join(dir, "config.toml")
	old := filepath.Join(dir, "runner.log")
	current := filepath.Join(t.TempDir(), "runner.log")
	explicit := old

	if got := oldDefaultLogFile(config.Config{}, usedConfig, current); got != "" {
		t.Fatalf("missing old log reported as %q", got)
	}
	if err := os.WriteFile(old, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := oldDefaultLogFile(config.Config{}, usedConfig, current); got != old {
		t.Fatalf("oldDefaultLogFile() = %q, want %q", got, old)
	}
	if got := oldDefaultLogFile(config.Config{}, usedConfig, old); got != "" {
		t.Fatalf("current log reported as old: %q", got)
	}
	if got := oldDefaultLogFile(config.Config{LogFile: &explicit}, usedConfig, current); got != "" {
		t.Fatalf("explicit log-file reported as old default: %q", got)
	}
	if got := oldDefaultLogFile(config.Config{}, "", current); got != "" {
		t.Fatalf("flags-only run reported an old log: %q", got)
	}
	if got := oldDefaultLogFile(config.Config{}, usedConfig, ""); got != "" {
		t.Fatalf("run without a log file reported an old log: %q", got)
	}
}

func TestLogConsole(t *testing.T) {
	tests := []struct {
		discarded, fileOpen bool
		want                *os.File
	}{
		{false, false, os.Stdout},
		{false, true, os.Stdout},
		{true, true, os.Stdout},
		{true, false, os.Stderr},
	}
	for _, tt := range tests {
		if got := logConsole(tt.discarded, tt.fileOpen); got != tt.want {
			t.Errorf("logConsole(%v, %v) = %s, want %s", tt.discarded, tt.fileOpen, got.Name(), tt.want.Name())
		}
	}
}

func TestStdoutIsDevNull(t *testing.T) {
	saved := os.Stdout
	t.Cleanup(func() { os.Stdout = saved })

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	os.Stdout = null
	if !stdoutIsDevNull() {
		t.Error("stdout on /dev/null not detected")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	os.Stdout = w
	if stdoutIsDevNull() {
		t.Error("a pipe was reported as /dev/null")
	}
}
