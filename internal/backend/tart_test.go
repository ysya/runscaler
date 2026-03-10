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

// mockSSHDialer records SSH calls for testing.
type mockSSHDialer struct {
	mu    sync.Mutex
	calls []sshCall
	err   error
}

type sshCall struct {
	addr, user, password, command string
}

func (m *mockSSHDialer) DialAndRun(_ context.Context, addr, user, password, command string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, sshCall{addr: addr, user: user, password: password, command: command})
	return m.err
}

func (m *mockSSHDialer) getCalls() []sshCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]sshCall, len(m.calls))
	copy(cp, m.calls)
	return cp
}

func newTestTartBackend(cmd *mockCommandRunner, sshMock *mockSSHDialer) *TartBackend {
	return &TartBackend{
		baseImage:   "macos-base:latest",
		sshUser:     "admin",
		sshPassword: "admin",
		runnerDir:   "/Users/admin/actions-runner",
		logger:      slog.New(slog.DiscardHandler),
		cmd:         cmd,
		ssh:         sshMock,
		vmSlots:     make(chan struct{}, 10), // generous limit for tests
	}
}

func TestTartBackend_StartRunner_Success(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart clone": {output: nil, err: nil},
			"tart run":   {output: nil, err: nil},
			"tart ip":    {output: []byte("192.168.64.5\n"), err: nil},
		},
	}
	sshMock := &mockSSHDialer{}
	b := newTestTartBackend(cmd, sshMock)
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

	// Verify SSH was called: first waitForSSH ("true"), then runRunnerViaSSH (JIT config)
	sshCalls := sshMock.getCalls()
	if len(sshCalls) != 2 {
		t.Fatalf("expected 2 SSH calls, got %d", len(sshCalls))
	}
	// First call: SSH readiness check
	if sshCalls[0].command != "true" {
		t.Errorf("first SSH call should be readiness check, got: %s", sshCalls[0].command)
	}
	// Second call: runner start
	sc := sshCalls[1]
	if sc.addr != "192.168.64.5" {
		t.Errorf("SSH addr = %q, want %q", sc.addr, "192.168.64.5")
	}
	if sc.user != "admin" {
		t.Errorf("SSH user = %q, want %q", sc.user, "admin")
	}
	if !strings.Contains(sc.command, "ACTIONS_RUNNER_INPUT_JITCONFIG=mock-jit-config") {
		t.Errorf("SSH command should contain JIT config, got: %s", sc.command)
	}
}

func TestTartBackend_StartRunner_CloneFails(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart clone": {err: fmt.Errorf("tart clone: image not found")},
		},
	}
	b := newTestTartBackend(cmd, &mockSSHDialer{})
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
	b := newTestTartBackend(cmd, &mockSSHDialer{})
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
	b := newTestTartBackend(cmd, &mockSSHDialer{})
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
	b := newTestTartBackend(cmd, &mockSSHDialer{})
	ctx := context.Background()

	err := b.RemoveRunner(ctx, "runner-abc")
	if err == nil {
		t.Fatal("RemoveRunner() should fail when delete fails")
	}
}

func TestTartBackend_Shutdown_IsNoop(t *testing.T) {
	cmd := &mockCommandRunner{}
	b := newTestTartBackend(cmd, &mockSSHDialer{})
	ctx := context.Background()

	// Should not panic or call any commands
	b.Shutdown(ctx)

	if len(cmd.getCalls()) != 0 {
		t.Errorf("Shutdown should not call any commands, got %d calls", len(cmd.getCalls()))
	}
}
