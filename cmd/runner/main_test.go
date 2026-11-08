package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	runnerlock "github.com/ysya/runscaler/internal/lock"
)

// declaredSilenceUsage is each command's SilenceUsage before any test runs
// the root. An execution leaves SilenceUsage set on the package-level
// commands, while a real process executes once.
var declaredSilenceUsage = map[*cobra.Command]bool{}

func TestMain(m *testing.M) {
	var record func(*cobra.Command)
	record = func(c *cobra.Command) {
		declaredSilenceUsage[c] = c.SilenceUsage
		for _, sub := range c.Commands() {
			record(sub)
		}
	}
	record(cmd)
	os.Exit(m.Run())
}

func TestExplainLockError(t *testing.T) {
	const fix = "sudo /usr/local/bin/runner service install --scope system --force"
	readOnly := explainLockError(fmt.Errorf("open runner lock /tmp/runner.lock: %w", syscall.EROFS), fix)
	if !errors.Is(readOnly, syscall.EROFS) || !strings.Contains(readOnly.Error(), fix) {
		t.Errorf("read-only lock error should keep its cause and include the fix:\n%v", readOnly)
	}

	held := explainLockError(&runnerlock.AlreadyRunningError{Path: "/tmp/runner.lock"}, fix)
	if !errors.Is(held, runnerlock.ErrAlreadyRunning) || !strings.Contains(held.Error(), "Only one runner may run per machine") {
		t.Errorf("held lock error should keep its cause and explain the single-instance rule:\n%v", held)
	}

	other := errors.New("permission denied")
	if got := explainLockError(other, fix); got != other {
		t.Errorf("unrelated lock error = %v, want it unchanged", got)
	}
}

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
	orig := startManager
	startManager = func(c *cobra.Command) error { called = true; return nil }
	defer func() { startManager = orig }()
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

func TestRootConfigInvocationDoesNotStart(t *testing.T) {
	called := false
	orig := startManager
	startManager = func(c *cobra.Command) error { called = true; return nil }
	defer func() { startManager = orig }()
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
	if called {
		t.Error("`runner --config X` must not start without the run subcommand")
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
	for _, name := range []string{"url", "name", "token", "max-runners", "concurrent", "provider", "health-port", "dry-run"} {
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

// executeRoot runs the real root command with args as main does and returns
// what the user sees on stderr, with the exit status. With no writers set, as
// in production, Cobra writes both its error and its usage to stderr, so both
// writers share the buffer main prints to.
func executeRoot(t *testing.T, args ...string) (string, int) {
	t.Helper()
	reset := func() {
		for c, silenced := range declaredSilenceUsage {
			c.SilenceUsage = silenced
		}
		if f := cmd.PersistentFlags().Lookup("config"); f != nil {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
		cmd.SetArgs(nil)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
	}
	reset()
	t.Cleanup(reset)

	var stderr bytes.Buffer
	cmd.SetArgs(args)
	cmd.SetOut(&stderr)
	cmd.SetErr(&stderr)
	code := execute(cmd, &stderr)
	return stderr.String(), code
}

// assertFailedOnce checks that an execution exited with status 1 and showed
// msg exactly once.
func assertFailedOnce(t *testing.T, stderr string, code int, msg string) {
	t.Helper()
	if code != 1 {
		t.Errorf("exit status = %d, want 1", code)
	}
	if n := strings.Count(stderr, msg); n != 1 {
		t.Errorf("stderr shows %q %d times, want once:\n%s", msg, n, stderr)
	}
}

func TestRuntimeErrorPrintsOnceWithoutUsage(t *testing.T) {
	failing := &cobra.Command{
		Use: "fail-at-runtime",
		RunE: func(*cobra.Command, []string) error {
			return errors.New("synthetic runtime failure")
		},
	}
	cmd.AddCommand(failing)
	t.Cleanup(func() { cmd.RemoveCommand(failing) })

	stderr, code := executeRoot(t, "fail-at-runtime")
	assertFailedOnce(t, stderr, code, "synthetic runtime failure")
	if strings.Contains(stderr, "Usage:") {
		t.Errorf("a runtime error printed usage:\n%s", stderr)
	}
}

// Real subcommands, nested ones included, get the same handling from root.
func TestSubcommandRuntimeErrorsPrintOnceWithoutUsage(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.toml")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"validate", []string{"validate", "--config", missing}},
		{"run", []string{"run", "--config", missing}},
		{"nested", []string{"scaleset", "delete", "builders", "--config", missing}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			stderr, code := executeRoot(t, tc.args...)
			assertFailedOnce(t, stderr, code, "failed to read config file")
			if strings.Contains(stderr, "Usage:") {
				t.Errorf("a runtime error printed usage:\n%s", stderr)
			}
		})
	}
}

func TestUnknownFlagPrintsUsageAndErrorOnce(t *testing.T) {
	stderr, code := executeRoot(t, "validate", "--no-such-flag")
	assertFailedOnce(t, stderr, code, "unknown flag: --no-such-flag")
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("an unknown flag did not print usage:\n%s", stderr)
	}
}

func TestWrongArgCountPrintsUsageAndErrorOnce(t *testing.T) {
	stderr, code := executeRoot(t, "scaleset", "delete")
	assertFailedOnce(t, stderr, code, "accepts 1 arg(s), received 0")
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("a wrong argument count did not print usage:\n%s", stderr)
	}
}
