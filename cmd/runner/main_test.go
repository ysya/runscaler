package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestHandleShutdownSignals_DrainsThenCancelsThenForces(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	signals := make(chan os.Signal, 3)
	drained := make(chan struct{})
	canceled := make(chan struct{})
	forced := make(chan struct{})
	var cancelOnce sync.Once

	go handleShutdownSignals(ctx, signals, func() { close(drained) }, func() {
		cancelOnce.Do(func() { close(canceled) })
	}, func() { close(forced) })

	signals <- syscall.SIGTERM
	awaitClosed(t, drained, "SIGTERM did not request drain")
	assertOpen(t, canceled, "first SIGTERM canceled the run instead of draining")

	signals <- syscall.SIGQUIT
	awaitClosed(t, canceled, "second signal did not request immediate shutdown")
	assertOpen(t, forced, "second signal forced process exit")

	signals <- os.Interrupt
	awaitClosed(t, forced, "third signal did not force process exit")
}

func TestHandleShutdownSignals_SIGINTStopsImmediatelyWithoutDrain(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	signals := make(chan os.Signal, 1)
	drained := make(chan struct{})
	canceled := make(chan struct{})

	go handleShutdownSignals(ctx, signals, func() { close(drained) }, func() { close(canceled) }, func() {})
	signals <- os.Interrupt
	awaitClosed(t, canceled, "SIGINT did not stop immediately")
	assertOpen(t, drained, "SIGINT must never request drain")
}

type blockingDrainer struct {
	started chan struct{}
	release chan struct{}
}

func (d *blockingDrainer) Drain(ctx context.Context) error {
	close(d.started)
	select {
	case <-d.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestWatchScaleSetDrain_KeepsListenerAliveUntilDrainReturns(t *testing.T) {
	listenCtx, cancelListen := context.WithCancel(context.Background())
	defer cancelListen()
	drain := make(chan struct{})
	d := &blockingDrainer{started: make(chan struct{}), release: make(chan struct{})}

	go watchScaleSetDrain(listenCtx, drain, time.Minute, d, cancelListen, slog.New(slog.DiscardHandler))
	close(drain)
	awaitClosed(t, d.started, "Drain was not started")
	if listenCtx.Err() != nil {
		t.Fatal("listener was canceled before Drain returned; job completions would be lost")
	}
	close(d.release)
	awaitDone(t, listenCtx, "listener was not canceled after Drain returned")
}

func TestWatchScaleSetDrain_ZeroTimeoutStopsImmediately(t *testing.T) {
	listenCtx, cancelListen := context.WithCancel(context.Background())
	defer cancelListen()
	drain := make(chan struct{})
	d := &blockingDrainer{started: make(chan struct{}), release: make(chan struct{})}

	go watchScaleSetDrain(listenCtx, drain, 0, d, cancelListen, slog.New(slog.DiscardHandler))
	close(drain)
	awaitDone(t, listenCtx, "disabled drain did not stop listener immediately")
	assertOpen(t, d.started, "disabled drain still called Drain")
}

func awaitClosed(t *testing.T, ch <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(failure)
	}
}

func awaitDone(t *testing.T, ctx context.Context, failure string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal(failure)
	}
}

func assertOpen(t *testing.T, ch <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(failure)
	default:
	}
}

func TestRootBareInvocationDoesNotStart(t *testing.T) {
	called := false
	orig := startScaling
	startScaling = func(c *cobra.Command) error { called = true; return nil }
	defer func() { startScaling = orig }()
	defer func() {
		if f := cmd.PersistentFlags().Lookup("config"); f != nil {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	}()

	cmd.SetArgs([]string{})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	defer cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if called {
		t.Error("bare `runner` must print help, not start scaling")
	}
}

func TestRootConfigInvocationStartsViaDropIn(t *testing.T) {
	called := false
	orig := startScaling
	startScaling = func(c *cobra.Command) error { called = true; return nil }
	defer func() { startScaling = orig }()
	defer func() {
		if f := cmd.PersistentFlags().Lookup("config"); f != nil {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	}()

	cmd.SetArgs([]string{"--config", "/nonexistent/x.toml"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	defer cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !called {
		t.Error("`runner --config X` must start via drop-in compat")
	}
}

func TestRunSubcommandRegistered(t *testing.T) {
	found := false
	for _, c := range cmd.Commands() {
		if c.Name() == "run" {
			found = true
			break
		}
	}
	if !found {
		t.Error("`run` subcommand must be registered on root")
	}
}

func TestRunOwnsStartFlags(t *testing.T) {
	for _, name := range []string{"url", "name", "token", "max-runners", "backend", "health-port", "dry-run"} {
		if runCommand.Flags().Lookup(name) == nil {
			t.Errorf("`run` must own the --%s start flag", name)
		}
	}
}

func TestRootNoLongerOwnsStartFlags(t *testing.T) {
	if cmd.Flags().Lookup("url") != nil {
		t.Error("root must not own --url anymore; it moved to `run`")
	}
}

func TestConfigStaysPersistentOnRoot(t *testing.T) {
	if cmd.PersistentFlags().Lookup("config") == nil {
		t.Error("--config must remain a persistent flag on root")
	}
}
