package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/diagnostics"
	"github.com/ysya/runscaler/internal/layout"
)

func TestDiagnosticsDirectoryFollowsEffectiveLogAndDisabledDefaults(t *testing.T) {
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	for _, tc := range []struct {
		name         string
		id           layout.Identity
		opened, want string
	}{
		{"custom", layout.Identity{ServiceScope: "system", GOOS: "linux"}, filepath.Join(parent, "granted", "runner.log"), filepath.Join(parent, "granted", "diagnostics")},
		{"fallback", layout.Identity{ServiceScope: "system", GOOS: "linux"}, filepath.Join(parent, "fallback", "runner.log"), filepath.Join(parent, "fallback", "diagnostics")},
		{"linux-disabled", layout.Identity{GOOS: "linux", Home: parent}, "", filepath.Join(parent, ".local/state/runner/diagnostics")},
		{"mac-disabled", layout.Identity{GOOS: "darwin", Home: parent}, "", filepath.Join(parent, "Library/Logs/runner/diagnostics")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := diagnosticDirectory(tc.id, tc.opened)
			if err != nil || got != tc.want {
				t.Fatalf("directory=%s err=%v", got, err)
			}
			if _, err := diagnostics.NewStore(got, false).Save(t.Context(), diagnostics.Record{}); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, id := range []layout.Identity{{Root: true, GOOS: "linux"}, {ServiceScope: "system", GOOS: "linux"}} {
		got, err := diagnosticDirectory(id, "")
		if err != nil || got != filepath.Join(func() string { p, _ := filepath.EvalSymlinks("/var/log"); return p }(), "runner/diagnostics") {
			t.Fatalf("system default=%s err=%v", got, err)
		}
	}
}

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

func TestNonRootSystemLogDecisionKeepsGrantedSystemDirectory(t *testing.T) {
	id := layout.Identity{GOOS: "linux", Home: "/var/lib/runner", ServiceScope: "system"}
	outside := "/home/admin/not-granted/runner.log"
	got := resolveLogFile(config.Config{LogFile: &outside}, id)
	if got.Path != "/var/log/runner/runner.log" || got.Warning == "" {
		t.Fatalf("system log decision = %+v", got)
	}
	custom := "/var/log/ci/runner.log"
	got = resolveLogFile(config.Config{LogFile: &custom}, id)
	if got.Path != custom || got.Fallback != "/var/log/runner/runner.log" {
		t.Fatalf("custom system log = %+v", got)
	}
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

func TestLogFileToReadFallsBackOnlyWhenTheDecidedLogIsMissing(t *testing.T) {
	d := logFileDecision{Path: "/var/log/custom/runner.log", Fallback: "/var/log/runner/runner.log", Enabled: true}
	missing := func(gone ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(gone, p) }
	}
	if got, fellBack := logFileToRead(d, missing("/var/log/custom/runner.log")); got != d.Fallback || !fellBack {
		t.Errorf("decided log missing = %q, %v; want the fallback runner run wrote to", got, fellBack)
	}
	if got, fellBack := logFileToRead(d, missing()); got != d.Path || fellBack {
		t.Errorf("decided log present = %q, %v", got, fellBack)
	}
	if got, fellBack := logFileToRead(d, missing(d.Path, d.Fallback)); got != d.Path || fellBack {
		t.Errorf("both missing = %q, %v; want the decided path named in the error", got, fellBack)
	}
	if got, fellBack := logFileToRead(logFileDecision{Path: d.Path, Enabled: true}, missing(d.Path)); got != d.Path || fellBack {
		t.Errorf("no fallback = %q, %v", got, fellBack)
	}
}
