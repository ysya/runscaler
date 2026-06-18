package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateConfigCopiesAndIsIdempotent(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	oldCfg := filepath.Join(oldDir, "config.toml")
	newCfg := filepath.Join(newDir, "config.toml")
	if err := os.WriteFile(oldCfg, []byte("url=\"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origOld, origNew := legacyConfigPath, newConfigPath
	legacyConfigPath, newConfigPath = oldCfg, newCfg
	defer func() { legacyConfigPath, newConfigPath = origOld, origNew }()

	copied, err := migrateConfig()
	if err != nil {
		t.Fatalf("migrateConfig: %v", err)
	}
	if !copied {
		t.Error("expected config to be copied")
	}
	if _, err := os.Stat(newCfg); err != nil {
		t.Errorf("new config missing: %v", err)
	}
	if _, err := os.Stat(oldCfg); err != nil {
		t.Errorf("legacy config should REMAIN after copy (atomicity): %v", err)
	}

	copied2, err := migrateConfig()
	if err != nil {
		t.Fatalf("second migrateConfig: %v", err)
	}
	if copied2 {
		t.Error("second run should be a no-op (target exists)")
	}
}

func TestNewServiceInstalledUserLevel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if newServiceInstalled(true) {
		t.Error("should be false with no new unit/plist present")
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, systemdUnitFile), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !newServiceInstalled(true) {
		t.Error("should detect new user systemd unit")
	}
}

func TestMigrateConfigSkipsWhenTargetExists(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	oldCfg := filepath.Join(oldDir, "config.toml")
	newCfg := filepath.Join(newDir, "config.toml")
	if err := os.WriteFile(oldCfg, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newCfg, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origOld, origNew := legacyConfigPath, newConfigPath
	legacyConfigPath, newConfigPath = oldCfg, newCfg
	defer func() { legacyConfigPath, newConfigPath = origOld, origNew }()

	moved, err := migrateConfig()
	if err != nil {
		t.Fatalf("migrateConfig: %v", err)
	}
	if moved {
		t.Error("must not move when target exists")
	}
	data, _ := os.ReadFile(newCfg)
	if string(data) != "new\n" {
		t.Error("existing target must not be overwritten")
	}
}
