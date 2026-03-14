package backend

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
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

func newTestTartBackend(cmd *mockCommandRunner) *TartBackend {
	slots := make(chan int, 10)
	for i := 0; i < 10; i++ {
		slots <- i
	}
	return &TartBackend{
		baseImage: "macos-base:latest",
		runnerDir: "/Users/admin/actions-runner",
		logger:    slog.New(slog.DiscardHandler),
		cmd:       cmd,
		vmSlots:   slots,
	}
}

func TestTartBackend_StartRunner_Success(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart clone": {output: nil, err: nil},
			"tart run":   {output: nil, err: nil},
			"tart exec":  {output: nil, err: nil},
		},
	}
	b := newTestTartBackend(cmd)
	ctx := context.Background()

	resourceID, err := b.StartRunner(ctx, "runner-abc", "mock-jit-config")
	if err != nil {
		t.Fatalf("StartRunner() error: %v", err)
	}
	if resourceID != "runner-abc" {
		t.Errorf("resourceID = %q, want %q", resourceID, "runner-abc")
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

func TestTartBackend_StartRunner_CloneFails(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart clone": {err: fmt.Errorf("tart clone: image not found")},
		},
	}
	b := newTestTartBackend(cmd)
	ctx := context.Background()

	_, err := b.StartRunner(ctx, "runner-abc", "jit")
	if err == nil {
		t.Fatal("StartRunner() should fail when clone fails")
	}
	if !strings.Contains(err.Error(), "clone") {
		t.Errorf("error should mention clone, got: %v", err)
	}
}

func TestTartBackend_RemoveRunner(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart stop":   {output: nil, err: nil},
			"tart delete": {output: nil, err: nil},
		},
	}
	b := newTestTartBackend(cmd)
	ctx := context.Background()

	err := b.RemoveRunner(ctx, "runner-abc")
	if err != nil {
		t.Fatalf("RemoveRunner() error: %v", err)
	}

	if cmd.callCount("tart stop") != 1 {
		t.Error("tart stop should be called once")
	}
	if cmd.callCount("tart delete") != 1 {
		t.Error("tart delete should be called once")
	}
}

func TestTartBackend_RemoveRunner_StopFails(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart stop":   {err: fmt.Errorf("VM already stopped")},
			"tart delete": {output: nil, err: nil},
		},
	}
	b := newTestTartBackend(cmd)
	ctx := context.Background()

	// Should succeed even if stop fails (VM may already be stopped)
	err := b.RemoveRunner(ctx, "runner-abc")
	if err != nil {
		t.Fatalf("RemoveRunner() should succeed even if stop fails, got: %v", err)
	}
}

func TestTartBackend_RemoveRunner_DeleteFails(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart stop":   {output: nil, err: nil},
			"tart delete": {err: fmt.Errorf("permission denied")},
		},
	}
	b := newTestTartBackend(cmd)
	ctx := context.Background()

	err := b.RemoveRunner(ctx, "runner-abc")
	if err == nil {
		t.Fatal("RemoveRunner() should fail when delete fails")
	}
}

func TestTartBackend_Shutdown_IsNoop(t *testing.T) {
	cmd := &mockCommandRunner{}
	b := newTestTartBackend(cmd)
	ctx := context.Background()

	// Should not panic or call any commands
	b.Shutdown(ctx)

	if len(cmd.getCalls()) != 0 {
		t.Errorf("Shutdown should not call any commands, got %d calls", len(cmd.getCalls()))
	}
}
