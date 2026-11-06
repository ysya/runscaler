package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

func TestLoadConfigKeepsLegacyBackendWhenProviderFlagIsUnchanged(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
url = "https://github.com/org"
name = "runners"
token = "ghp_x"
backend = "tart"
runner-image = "macos:latest"
max-runners = 2
`), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &cobra.Command{}
	c.Flags().String("config", path, "")
	c.Flags().String("provider", config.DefaultProvider, "")
	c.Flags().String("backend", "", "")
	c.Flags().Lookup("config").Changed = true

	cfg, err := loadConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ResolveScaleSets()[0].Provider; got != "tart" {
		t.Errorf("Provider = %q, want tart from legacy config", got)
	}
}

func TestLoadConfigSurfacesParseError(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	legacyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacyDir, "config.toml"),
		[]byte("this is := not valid toml ===\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origDir := legacyConfigDir
	legacyConfigDir = legacyDir
	defer func() { legacyConfigDir = origDir }()

	emptyCwd := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(emptyCwd); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	c := &cobra.Command{}
	c.PersistentFlags().String("config", "", "")

	if _, err := loadConfig(c); err == nil {
		t.Error("expected a parse error to surface, got nil")
	}
}

func TestLoadConfigFallsBackToLegacyDir(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	legacyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacyDir, "config.toml"),
		[]byte("url = \"https://github.com/org\"\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origDir := legacyConfigDir
	legacyConfigDir = legacyDir
	defer func() { legacyConfigDir = origDir }()

	emptyCwd := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(emptyCwd); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	c := &cobra.Command{}
	c.PersistentFlags().String("config", "", "")

	cfg, err := loadConfig(c)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Defaults.RegistrationURL != "https://github.com/org" {
		t.Errorf("expected config loaded from legacy dir, got url=%q", cfg.Defaults.RegistrationURL)
	}
}

// TestLoadConfigWarnsOnUnknownKeys pins that unknown config keys are
// reported via cfg.Warnings but never as an error: a self-updated
// deployment with a config written for another version must keep starting.
func TestLoadConfigWarnsOnUnknownKeys(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path,
		[]byte("url = \"https://github.com/org\"\nname = \"x\"\ntypo-key = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &cobra.Command{}
	// Register on Flags() directly: persistent flags only merge into
	// Flags() during Execute, which this test bypasses.
	c.Flags().String("config", path, "")

	cfg, err := loadConfig(c)
	if err != nil {
		t.Fatalf("loadConfig must not error on unknown keys, got: %v", err)
	}
	if len(cfg.Warnings) != 1 {
		t.Fatalf("Warnings = %q, want exactly 1", cfg.Warnings)
	}
	if !strings.Contains(cfg.Warnings[0], `unknown config key "typo-key"`) {
		t.Errorf("warning = %q, want it to name typo-key", cfg.Warnings[0])
	}
}
