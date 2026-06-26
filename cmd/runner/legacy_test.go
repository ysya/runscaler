package main

import (
	"os"
	"path/filepath"
	"runtime"
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

	var p string
	switch runtime.GOOS {
	case "linux":
		p = filepath.Join(home, ".config", "systemd", "user", legacySystemdUnit)
	case "darwin":
		p = filepath.Join(home, "Library", "LaunchAgents", legacyLaunchdPlist)
	default:
		t.Skip("unsupported OS")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !legacyServiceInstalled(true) {
		t.Error("should detect legacy service for current OS")
	}
}
