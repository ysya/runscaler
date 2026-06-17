package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateConfigMovesAndIsIdempotent(t *testing.T) {
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

	moved, err := migrateConfig()
	if err != nil {
		t.Fatalf("migrateConfig: %v", err)
	}
	if !moved {
		t.Error("expected config to be moved")
	}
	if _, err := os.Stat(newCfg); err != nil {
		t.Errorf("new config missing: %v", err)
	}
	if _, err := os.Stat(oldCfg); !os.IsNotExist(err) {
		t.Errorf("old config should be gone after move")
	}

	moved2, err := migrateConfig()
	if err != nil {
		t.Fatalf("second migrateConfig: %v", err)
	}
	if moved2 {
		t.Error("second run should be a no-op")
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
