package provider

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockCommandRunner records all command invocations for testing.
type mockCommandRunner struct {
	mu       sync.Mutex
	calls    []cmdCall
	results  map[string]cmdResult // key: "command arg1 arg2..." -> result
	fallback cmdResult
}

type cmdCall struct {
	name string
	args []string
}

type cmdResult struct {
	output []byte
	err    error
}

func (m *mockCommandRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	_, err := m.Run(ctx, name, args...)
	return err
}

func (m *mockCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, cmdCall{name: name, args: args})

	key := name + " " + strings.Join(args, " ")
	if r, ok := m.results[key]; ok {
		return r.output, r.err
	}

	// Check prefix matches (for flexible matching)
	for k, r := range m.results {
		if strings.HasPrefix(key, k) {
			return r.output, r.err
		}
	}

	return m.fallback.output, m.fallback.err
}

func (m *mockCommandRunner) getCalls() []cmdCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]cmdCall, len(m.calls))
	copy(cp, m.calls)
	return cp
}

func (m *mockCommandRunner) callCount(prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, c := range m.calls {
		key := c.name + " " + strings.Join(c.args, " ")
		if strings.HasPrefix(key, prefix) {
			count++
		}
	}
	return count
}

func newTestTartProvider(cmd *mockCommandRunner) *TartProvider {
	return &TartProvider{
		baseImage:   "macos-base:latest",
		runnerDir:   "/Users/admin/actions-runner",
		logger:      slog.New(slog.DiscardHandler),
		cmd:         cmd,
		coordinator: NewTartHostCoordinator(10),
	}
}

func TestTartProvider_StartInstance_Success(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart clone": {output: nil, err: nil},
			"tart run":   {output: nil, err: nil},
			"tart exec":  {output: nil, err: nil},
		},
	}
	b := newTestTartProvider(cmd)
	ctx := context.Background()

	instanceID, err := b.StartInstance(ctx, "runner-abc", "mock-jit-config")
	if err != nil {
		t.Fatalf("StartInstance() error: %v", err)
	}
	if instanceID != "runner-abc" {
		t.Errorf("instanceID = %q, want %q", instanceID, "runner-abc")
	}

	// Verify clone was called with correct args
	calls := cmd.getCalls()
	found := false
	for _, c := range calls {
		if c.name == "tart" && len(c.args) >= 3 && c.args[0] == "clone" {
			if c.args[1] != "macos-base:latest" || c.args[2] != "runner-abc" {
				t.Errorf("clone args = %v, want [clone macos-base:latest runner-abc]", c.args)
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("tart clone was not called")
	}

	// Verify tart exec was called:
	// 1. readiness check ("true")
	// 2. test -x (verify runner binary)
	// 3. write JIT config to file
	// 4. start runner
	// 5. pgrep verification
	execCount := cmd.callCount("tart exec")
	if execCount != 5 {
		t.Fatalf("expected 5 tart exec calls, got %d", execCount)
	}
}

func TestTartProvider_StartInstance_CloneFails(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart clone": {err: fmt.Errorf("tart clone: image not found")},
		},
	}
	b := newTestTartProvider(cmd)
	ctx := context.Background()

	_, err := b.StartInstance(ctx, "runner-abc", "jit")
	if err == nil {
		t.Fatal("StartInstance() should fail when clone fails")
	}
	if !strings.Contains(err.Error(), "clone") {
		t.Errorf("error should mention clone, got: %v", err)
	}
}

func TestTartProvider_RemoveInstance(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart stop":   {output: nil, err: nil},
			"tart delete": {output: nil, err: nil},
		},
	}
	b := newTestTartProvider(cmd)
	ctx := context.Background()

	err := b.RemoveInstance(ctx, "runner-abc")
	if err != nil {
		t.Fatalf("RemoveInstance() error: %v", err)
	}

	if cmd.callCount("tart stop") != 1 {
		t.Error("tart stop should be called once")
	}
	if cmd.callCount("tart delete") != 1 {
		t.Error("tart delete should be called once")
	}
}

func TestTartProvider_RemoveInstance_StopFails(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart stop":   {err: fmt.Errorf("VM already stopped")},
			"tart delete": {output: nil, err: nil},
		},
	}
	b := newTestTartProvider(cmd)
	ctx := context.Background()

	// Should succeed even if stop fails (VM may already be stopped)
	err := b.RemoveInstance(ctx, "runner-abc")
	if err != nil {
		t.Fatalf("RemoveInstance() should succeed even if stop fails, got: %v", err)
	}
}

func TestTartProvider_RemoveInstance_DeleteFails(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart stop":   {output: nil, err: nil},
			"tart delete": {err: fmt.Errorf("permission denied")},
		},
	}
	b := newTestTartProvider(cmd)
	ctx := context.Background()

	err := b.RemoveInstance(ctx, "runner-abc")
	if err == nil {
		t.Fatal("RemoveInstance() should fail when delete fails")
	}
}

func TestPruneTartCache_DisabledWhenNoCriteria(t *testing.T) {
	cmd := &mockCommandRunner{}
	ctx := context.Background()

	if err := pruneTartCacheWith(ctx, cmd, "/some/home", 0, 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(cmd.getCalls()); got != 0 {
		t.Errorf("expected 0 calls when no criteria set, got %d", got)
	}
}

func TestPruneTartCache_AgeOnly(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{"tart prune": {}},
	}
	ctx := context.Background()

	if err := pruneTartCacheWith(ctx, cmd, "/some/home", 7*24*time.Hour, 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := cmd.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	want := []string{"prune", "--entries", "caches", "--older-than", "7"}
	assertArgs(t, calls[0].args, want)
}

func TestPruneTartCache_AgeAndBudget(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{"tart prune": {}},
	}
	ctx := context.Background()

	if err := pruneTartCacheWith(ctx, cmd, "/some/home", 7*24*time.Hour, 50, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := cmd.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	want := []string{"prune", "--entries", "caches", "--older-than", "7", "--space-budget", "50"}
	assertArgs(t, calls[0].args, want)
}

// Sub-day windows must floor to 1 day; --older-than=0 would wipe the whole cache.
func TestPruneTartCache_SubDayAgeFloorsToOneDay(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{"tart prune": {}},
	}
	ctx := context.Background()

	if err := pruneTartCacheWith(ctx, cmd, "/some/home", 12*time.Hour, 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := cmd.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	want := []string{"prune", "--entries", "caches", "--older-than", "1"}
	assertArgs(t, calls[0].args, want)
}

func TestPruneTartCache_BudgetOnly(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{"tart prune": {}},
	}
	ctx := context.Background()

	if err := pruneTartCacheWith(ctx, cmd, "/some/home", 0, 50, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := cmd.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	want := []string{"prune", "--entries", "caches", "--space-budget", "50"}
	assertArgs(t, calls[0].args, want)
}

func TestPruneTartCache_PropagatesError(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart prune": {err: fmt.Errorf("disk full")},
		},
	}
	ctx := context.Background()

	err := pruneTartCacheWith(ctx, cmd, "/some/home", 7*24*time.Hour, 0, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected error from failed prune, got nil")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error does not wrap underlying: %v", err)
	}
}

func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTartProvider_Shutdown_IsNoop(t *testing.T) {
	cmd := &mockCommandRunner{}
	b := newTestTartProvider(cmd)
	ctx := context.Background()

	// Should not panic or call any commands
	b.Shutdown(ctx)

	if len(cmd.getCalls()) != 0 {
		t.Errorf("Shutdown should not call any commands, got %d calls", len(cmd.getCalls()))
	}
}

// TestRunnerStartCmd_PassesJITConfigBeforeDeletingIt executes the generated
// command with a real shell and asserts the runner actually receives the JIT
// config. A previous version put `rm` after the `&`, so the command
// substitution reading the file raced the delete: the runner intermittently
// started with an empty ACTIONS_RUNNER_INPUT_JITCONFIG and died with "Not
// configured", leaving jobs queued forever. The loop makes that race show up
// reliably rather than once in a while.
func TestRunnerStartCmd_PassesJITConfigBeforeDeletingIt(t *testing.T) {
	const want = "eyJfam10Y29uZmlnIjoidGVzdC1qaXQtY29uZmlnIn0="

	for i := range 20 {
		dir := t.TempDir()
		jitPath := filepath.Join(dir, "jitconfig")
		gotPath := filepath.Join(dir, "got")

		if err := os.WriteFile(jitPath, []byte(want), 0o600); err != nil {
			t.Fatalf("write jit config: %v", err)
		}

		// Stand-in for run.sh: records the env var the runner would see.
		runScript := filepath.Join(dir, "run.sh")
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$ACTIONS_RUNNER_INPUT_JITCONFIG\" > %s\n", gotPath)
		if err := os.WriteFile(runScript, []byte(script), 0o700); err != nil {
			t.Fatalf("write run script: %v", err)
		}

		// `wait` so the test observes the backgrounded runner's result.
		cmd := exec.Command("sh", "-c", runnerStartCmd(runScript, jitPath)+"\nwait")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("iteration %d: shell failed: %v (%s)", i, err, out)
		}

		got, err := os.ReadFile(gotPath)
		if err != nil {
			t.Fatalf("iteration %d: runner never recorded a config: %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("iteration %d: runner saw JIT config %q, want %q — the delete raced the read",
				i, got, want)
		}
		if _, err := os.Stat(jitPath); !os.IsNotExist(err) {
			t.Errorf("iteration %d: JIT config still on disk after startup (stat err = %v)", i, err)
		}
	}
}

func TestWriteJITCmd_QuotesPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jit config")
	want := "payload\nJITEOF\nprintf injected\n'quoted'"
	cmd := exec.Command("sh", "-c", writeJITCmd(path, want))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write command failed: %v (%s)", err, out)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("written JIT config = %q, want %q", got, want)
	}
}
