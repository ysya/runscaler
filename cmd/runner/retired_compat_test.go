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
	if !slices.Equal(sharedVolumeNames, []string{"runner-shared"}) {
		t.Fatalf("doctor still inspects retired volumes: %q", sharedVolumeNames)
	}
}
