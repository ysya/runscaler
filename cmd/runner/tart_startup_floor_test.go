package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/provider"
)

func TestStartupCleansOrphansBeforeCloneFloor(t *testing.T) {
	testStartupCloneFloorCleanup(t, false)
}

func TestDryRunDoesNotCleanOrphansBeforeCloneFloor(t *testing.T) {
	testStartupCloneFloorCleanup(t, true)
}

func testStartupCloneFloorCleanup(t *testing.T, dryRun bool) {
	t.Helper()
	fixture := t.TempDir()
	bin := filepath.Join(fixture, "bin")
	home := filepath.Join(fixture, "tart-home")
	for _, dir := range []string{bin, home} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	callsPath := filepath.Join(fixture, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$PR70_TART_CALLS"
case "$1" in
  list) printf '%s\n' '[{"Name":"fixture-image","Source":"local","State":"stopped"},{"Name":"runner-deadbeef","Source":"local","State":"running"}]' ;;
  delete|stop) exit 0 ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "tart"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PR70_TART_CALLS", callsPath)
	v := viper.New()
	for key, value := range map[string]any{
		"url": "https://github.com/example-org", "name": "fixture", "provider": "tart",
		"token": "review-fixture", "runner-image": "fixture-image", "max-runners": 1,
		"log-file": "", "log-level": "error", "log-format": "text", "diagnostics.enabled": false,
		"dry-run":   dryRun,
		"tart.home": home, "tart.clone-min-free-gb": 1000000,
	} {
		v.Set(key, value)
	}
	cfg, err := config.Load(v)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = runManager(ctx, cfg, nil)
	if !errors.Is(err, provider.ErrBelowCloneFloor) {
		t.Fatalf("unexpected startup error: %v", err)
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned := strings.Contains(string(calls), "delete runner-deadbeef"); cleaned == dryRun {
		t.Fatalf("orphan cleanup=%v, dry-run=%v; Tart calls: %q", cleaned, dryRun, calls)
	}
}
