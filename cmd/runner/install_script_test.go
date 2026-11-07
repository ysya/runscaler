package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The download stub always exits before extraction/install. These cases never
// download a release, create an account, or touch a real installation.
func TestInstallerScopePreflight(t *testing.T) {
	script, err := filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, osName, uid string
		args              []string
		want              string
	}{
		{"linux default needs root", "Linux", "1000", nil, "requires root"},
		{"linux user", "Linux", "1000", []string{"--user"}, "Downloading runner"},
		{"linux system", "Linux", "0", nil, "Downloading runner"},
		{"root user denied", "Linux", "0", []string{"--scope", "user"}, "actual user without sudo"},
		{"mac user", "Darwin", "1000", nil, "Downloading runner"},
		{"mac system denied", "Darwin", "1000", []string{"--scope=system"}, "user installation only"},
		{"invalid scope", "Linux", "1000", []string{"--scope=other"}, "Invalid scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stubs := map[string]string{
				"id":    "#!/bin/sh\necho " + tc.uid + "\n",
				"uname": "#!/bin/sh\nif [ \"$1\" = -s ]; then echo " + tc.osName + "; else echo arm64; fi\n",
				"curl":  "#!/bin/sh\nexit 33\n",
			}
			for name, content := range stubs {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("/bin/sh", append([]string{script}, tc.args...)...)
			command.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + dir, "RUNNER_VERSION=v-test", "TMPDIR=" + dir}
			out, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("installer output %q, err=%v; want %q", out, err, tc.want)
			}
		})
	}
}
