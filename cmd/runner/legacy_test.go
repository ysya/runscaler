package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyConfigExists(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")

	orig := legacyConfigPath
	legacyConfigPath = cfg
	defer func() { legacyConfigPath = orig }()

	if legacyConfigExists() {
		t.Error("should be false before file exists")
	}
	if err := os.WriteFile(cfg, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !legacyConfigExists() {
		t.Error("should be true after file exists")
	}
}

func TestLegacyServiceInstalledUserLevel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if legacyServiceInstalled(true) {
		t.Error("should be false with no legacy unit/plist present")
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, legacySystemdUnit), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !legacyServiceInstalled(true) {
		t.Error("should detect legacy user systemd unit")
	}
}
