package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// This is a parser/dependency check, not an actual service or UID test.
func TestGeneratedSystemdDefinitionsVerify(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd verification requires Linux")
	}
	tool, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is not installed")
	}
	binary, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"system-root", "system-nonroot", "user"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			opts := installOpts{binaryPath: binary, configPath: "/etc/runner/config $literal file.toml", provider: "tart", user: mode == "user"}
			if mode == "system-nonroot" {
				opts.runtimeUser, opts.runtimeGroup = "nobody", "65534"
			}
			unit, err := renderSystemdUnit(opts)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "runner.service")
			if err := os.WriteFile(path, []byte(unit), 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(tool, "verify", path)
			command.Env = append(os.Environ(), "SYSTEMD_LOG_LEVEL=warning")
			if data, err := command.CombinedOutput(); err != nil {
				t.Fatalf("unit verification failed: %v\n%s", err, data)
			}
		})
	}
}
