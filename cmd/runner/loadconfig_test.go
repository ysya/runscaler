package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

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
