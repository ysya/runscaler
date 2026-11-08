package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRetiredCommandsAndFlagsAreUnavailable(t *testing.T) {
	for _, c := range cmd.Commands() {
		if c.Name() == "migrate" {
			t.Fatal("retired migrate command is registered")
		}
	}
	for _, c := range []*cobra.Command{runCommand, initCmd} {
		if c.Flags().Lookup("backend") != nil {
			t.Errorf("%s still accepts --backend", c.Name())
		}
	}
}

func TestConfigSearchUsesOnlyCurrentLocations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	paths, err := configSearchPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".", filepath.Join(os.Getenv("HOME"), ".config", "runner"), "/etc/runner"}
	if !slices.Equal(paths, want) {
		t.Fatalf("config search=%q; want %q", paths, want)
	}
}

func TestInstalledServiceRequiresTheRunSubcommand(t *testing.T) {
	if _, err := targetFromArgs(scopeSystem, "/usr/local/bin/runner", []string{"/usr/local/bin/runner", "--config", "/etc/runner/config.toml"}); err == nil {
		t.Fatal("retired root invocation accepted as installed service")
	}
}

func TestReleaseBuildsOnlyTheCurrentBinary(t *testing.T) {
	data, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"id: runscaler-compat", "binary: runscaler", "runscaler-{{"} {
		if strings.Contains(string(data), old) {
			t.Errorf("retired compatibility asset remains: %s", old)
		}
	}
	if !strings.Contains(string(data), "CGO_ENABLED=0") {
		t.Fatal("release lost cgo-free build contract")
	}
}

func TestDoctorDoesNotInspectRetiredVolumes(t *testing.T) {
	got := doctorSharedVolumes(map[string]bool{"/var/run/docker.sock": true}, nil)
	if !slices.Equal(got["/var/run/docker.sock"], []string{"runner-shared"}) {
		t.Fatalf("doctor inspects %q, want only the current default volume", got)
	}
}

func TestDoctorHasNoPreLockHealthProbe(t *testing.T) {
	for _, name := range []string{"health-port", "health-address"} {
		if doctorCmd.Flags().Lookup(name) != nil {
			t.Errorf("doctor still registers --%s for the retired health probe", name)
		}
	}
}

func TestMakefileBuildsWithoutCgo(t *testing.T) {
	data, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	builds := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "go build") {
			builds++
			if !strings.Contains(line, "CGO_ENABLED=0") {
				t.Errorf("Makefile build without CGO_ENABLED=0: %s", strings.TrimSpace(line))
			}
		}
	}
	if builds == 0 {
		t.Fatal("no go build lines found in the Makefile")
	}
}
