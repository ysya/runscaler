package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/provider"
)

// inspectionRaceRunner publishes a real fixture config before clone returns.
// Cleanup uses the same directory, so deleting an active probe breaks detection.
type inspectionRaceRunner struct {
	home      string
	published chan string
	resume    chan struct{}
}

func (r *inspectionRaceRunner) Run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch args[0] {
	case "clone":
		name := args[2]
		dir := filepath.Join(r.home, "vms", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"os":"linux"}`), 0o600); err != nil {
			return nil, err
		}
		r.published <- name
		select {
		case <-r.resume:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case "list":
		entries, err := os.ReadDir(filepath.Join(r.home, "vms"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		items := []provider.TartVM{}
		for _, entry := range entries {
			items = append(items, provider.TartVM{Name: entry.Name(), Source: "local", State: "stopped"})
		}
		return json.Marshal(items)
	case "stop":
		return nil, nil
	case "delete":
		return nil, os.RemoveAll(filepath.Join(r.home, "vms", args[1]))
	default:
		return nil, fmt.Errorf("unexpected fixture command: %v", args)
	}
}

func (r *inspectionRaceRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	_, err := r.Run(ctx, name, args...)
	return err
}

func TestDoctorCleanupCannotDeleteActiveTartInspection(t *testing.T) {
	testCleanupCannotDeleteActiveTartInspection(t, func(ctx context.Context, runner provider.CommandRunner, home string) error {
		_, err := checkTartVMs(ctx, runner, home, removeOrphans)
		return err
	})
}

func TestStartupCleanupCannotDeleteActiveTartInspection(t *testing.T) {
	testCleanupCannotDeleteActiveTartInspection(t, func(ctx context.Context, runner provider.CommandRunner, home string) error {
		return reconcileTartHome(ctx, runner, home, slog.New(slog.DiscardHandler))
	})
}

func TestTartCleanupLockFailureDoesNotRunCommands(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(home, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, cleanup := range map[string]func(context.Context, provider.CommandRunner, string) error{
		"doctor": func(ctx context.Context, runner provider.CommandRunner, home string) error {
			_, err := checkTartVMs(ctx, runner, home, removeOrphans)
			return err
		},
		"startup": func(ctx context.Context, runner provider.CommandRunner, home string) error {
			return reconcileTartHome(ctx, runner, home, slog.New(slog.DiscardHandler))
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &mockCommandRunner{}
			runner.setResult("tart list --format json", `[{"Name":"runner-deadbeef","Source":"local"}]`)
			if err := cleanup(context.Background(), runner, home); err == nil || len(runner.calls) != 0 {
				t.Fatalf("cleanup bypassed its failed lock: error=%v commands=%v", err, runner.calls)
			}
		})
	}
}

func testCleanupCannotDeleteActiveTartInspection(t *testing.T, cleanup func(context.Context, provider.CommandRunner, string) error) {
	t.Helper()
	home := t.TempDir()
	runner := &inspectionRaceRunner{home: home, published: make(chan string, 1), resume: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var resume sync.Once
	unblock := func() { resume.Do(func() { close(runner.resume) }) }
	defer unblock()
	detected := make(chan error, 1)
	go func() {
		guest, err := provider.DetectTartGuest(ctx, runner, home, "fixture-image")
		if err == nil && guest != provider.TartGuestLinux {
			err = fmt.Errorf("detected %s, want linux", guest)
		}
		detected <- err
	}()
	var name string
	select {
	case name = <-runner.published:
	case <-ctx.Done():
		t.Fatal("inspection did not publish its fixture clone")
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cleanupCancel()
	err := cleanup(cleanupCtx, runner, home)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cleanup error = %v, want to wait for the active inspection", err)
	}
	if _, err := os.Stat(filepath.Join(home, "vms", name, "config.json")); err != nil {
		t.Errorf("cleanup deleted an active inspection clone: %v", err)
	}
	unblock()
	if err := <-detected; err != nil {
		t.Fatalf("concurrent cleanup broke guest detection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "vms", name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection did not clean up its clone: %v", err)
	}
	stale := filepath.Join(home, "vms", "runner-keep-deadbeef")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(ctx, runner, home); err != nil {
		t.Fatalf("cleanup after inspection = %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup left a stale owned clone: %v", err)
	}
}
