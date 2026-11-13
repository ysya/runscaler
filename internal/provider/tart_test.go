package provider

import (
	"context"
	"errors"
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
	// sequences answers an exact key with these results in order before
	// falling back to results, for a command whose outcome changes.
	sequences map[string][]cmdResult
}

type blockingContextCommandRunner struct {
	mu           sync.Mutex
	hadDeadlines []bool
}

func (m *blockingContextCommandRunner) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	_, hadDeadline := ctx.Deadline()
	m.mu.Lock()
	m.hadDeadlines = append(m.hadDeadlines, hadDeadline)
	m.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *blockingContextCommandRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	_, err := m.Run(ctx, name, args...)
	return err
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
	if seq := m.sequences[key]; len(seq) > 0 {
		m.sequences[key] = seq[1:]
		return seq[0].output, seq[0].err
	}
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
		// Real waits are seconds to minutes; keep the start-up loop instant.
		runnerPoll:          time.Millisecond,
		runnerListenTimeout: 50 * time.Millisecond,
		runnerSettle:        time.Millisecond,
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
	// 5. pgrep: runner still alive
	// 6. grep: runner reports "Listening for Jobs"
	// 7. pgrep: runner survived its first poll for messages
	execCount := cmd.callCount("tart exec")
	if execCount != 7 {
		t.Fatalf("expected 7 tart exec calls, got %d", execCount)
	}
}

func TestTartProvider_SharedVMKeepsVMUntilLastRunner(t *testing.T) {
	cmd := &mockCommandRunner{results: map[string]cmdResult{
		"tart clone":  {},
		"tart run":    {},
		"tart exec":   {},
		"tart stop":   {},
		"tart delete": {},
	}}
	p := newTestTartProvider(cmd)
	p.runnersPerVM = 2

	first, err := p.StartInstance(context.Background(), "runner-one", "jit-one")
	if err != nil {
		t.Fatalf("first StartInstance() error: %v", err)
	}
	second, err := p.StartInstance(context.Background(), "runner-two", "jit-two")
	if err != nil {
		t.Fatalf("second StartInstance() error: %v", err)
	}
	if first != "runner-one" || second != "runner-two" {
		t.Fatalf("instance IDs = %q, %q, want runner names", first, second)
	}
	if got := cmd.callCount("tart clone"); got != 1 {
		t.Fatalf("clone count = %d, want one shared VM", got)
	}

	if err := p.RemoveInstance(context.Background(), first); err != nil {
		t.Fatalf("first RemoveInstance() error: %v", err)
	}
	if got := cmd.callCount("tart delete"); got != 0 {
		t.Fatalf("delete count after first runner = %d, want 0", got)
	}
	if err := p.RemoveInstance(context.Background(), second); err != nil {
		t.Fatalf("second RemoveInstance() error: %v", err)
	}
	if got := cmd.callCount("tart delete"); got != 1 {
		t.Fatalf("delete count after last runner = %d, want 1", got)
	}
}

// runnerDiagListing is `ls -t` of the runner's _diag directory after a job:
// newest first, with a Worker log ahead of the Runner log.
const runnerDiagListing = "Worker_20261007-230510-utc.log\nRunner_20261007-230024-utc.log\n"

// startupCmdRunner answers a cold start whose runner start-up checks are
// overridden by the given results.
func startupCmdRunner(overrides map[string]cmdResult) *mockCommandRunner {
	results := map[string]cmdResult{
		"tart clone": {},
		"tart run":   {},
		"tart exec":  {},
		"tart exec runner-abc ls -t /Users/admin/actions-runner/_diag": {output: []byte(runnerDiagListing)},
	}
	for k, v := range overrides {
		results[k] = v
	}
	return &mockCommandRunner{results: results}
}

func TestTartProvider_StartInstance_RunnerExitsBeforeListening(t *testing.T) {
	cmd := startupCmdRunner(map[string]cmdResult{
		"tart exec runner-abc pgrep -f Runner.Listener": {err: errors.New("exit status 1")},
		"tart exec runner-abc tail -20 /tmp/runner.log": {output: []byte("An error occurred: Not configured")},
	})
	p := newTestTartProvider(cmd)

	_, err := p.StartInstance(context.Background(), "runner-abc", "jit")
	if err == nil || !strings.Contains(err.Error(), "Not configured") {
		t.Fatalf("StartInstance() error = %v, want one carrying the runner log", err)
	}
	if callIndex(cmd.getCalls(), "tart", "delete", "runner-abc") < 0 {
		t.Error("VM with a dead runner was not deleted")
	}
}

func TestTartProvider_StartInstance_WaitsThroughEmptyStartupLog(t *testing.T) {
	cmd := startupCmdRunner(nil)
	cmd.sequences = map[string][]cmdResult{
		"tart exec runner-abc pgrep -f Runner.Listener":                   {{err: errors.New("exit status 1")}, {}, {}},
		"tart exec runner-abc tail -20 /tmp/runner.log":                   {{}, {}},
		"tart exec runner-abc grep -q Listening for Jobs /tmp/runner.log": {{err: errors.New("exit status 1")}, {}},
	}
	p := newTestTartProvider(cmd)
	p.runnerPoll = time.Millisecond
	p.runnerListenTimeout = 100 * time.Millisecond
	p.runnerSettle = time.Millisecond

	if _, err := p.StartInstance(context.Background(), "runner-abc", "jit"); err != nil {
		t.Fatalf("StartInstance() failed after transient empty startup log: %v", err)
	}
	if got := cmd.callCount("tart exec runner-abc pgrep -f Runner.Listener"); got != 3 {
		t.Fatalf("pgrep checks = %d, want two readiness polls and the settle check", got)
	}
	if got := cmd.callCount("tart exec runner-abc grep -q Listening for Jobs /tmp/runner.log"); got != 2 {
		t.Fatalf("readiness marker checks = %d, want a missing then present marker", got)
	}
	if callIndex(cmd.getCalls(), "tart", "delete", "runner-abc") >= 0 {
		t.Fatal("VM was deleted during transient runner startup")
	}
}

func TestTartProvider_RunnerAliveSurfacesTartTransportError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "tart")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'Error: unavailable (14): Transport became inactive\\n' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := &TartProvider{cmd: execCommandRunner{}}

	alive, err := p.runnerAlive(context.Background(), "runner-abc", p.runnerDir)
	if alive {
		t.Fatal("runnerAlive() reported a Tart transport error as alive")
	}
	if err == nil || !strings.Contains(err.Error(), "Transport became inactive") {
		t.Fatalf("runnerAlive() error = %v, want the Tart transport failure", err)
	}
}

func TestTartProvider_RunnerAliveMalformedExitStatusDoesNotPanic(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart exec runner-abc pgrep -f Runner.Listener": {
				err: errors.New("wrapped: exit status "),
			},
		},
	}
	p := newTestTartProvider(cmd)

	alive, err := p.runnerAlive(context.Background(), "runner-abc", p.runnerDir)
	if alive {
		t.Fatal("runnerAlive() reported a malformed-status process as alive")
	}
	if err == nil || !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("runnerAlive() error = %v, want the original malformed status error", err)
	}
}

func TestTartProvider_StartInstance_RunnerNeverConnects(t *testing.T) {
	cmd := startupCmdRunner(map[string]cmdResult{
		"tart exec runner-abc grep -q Listening for Jobs /tmp/runner.log": {err: errors.New("exit status 1")},
		"tart exec runner-abc tail -n 20 /Users/admin/actions-runner/_diag/Runner_20261007-230024-utc.log": {
			output: []byte("[2026-10-07 23:01:24Z WARN GitHubActionsService] GET request to https://pipelinesghubeus8.actions.githubusercontent.com timed out after 60 seconds"),
		},
	})
	p := newTestTartProvider(cmd)

	_, err := p.StartInstance(context.Background(), "runner-abc", "jit")
	if err == nil {
		t.Fatal("StartInstance() succeeded for a runner that never connected")
	}
	if !strings.Contains(err.Error(), "timed out after 60 seconds") {
		t.Errorf("error = %q, want it to carry the newest Runner diag log", err)
	}
	if callIndex(cmd.getCalls(), "tart", "delete", "runner-abc") < 0 {
		t.Error("VM whose runner never connected was not deleted")
	}
}

func TestTartProvider_StartInstance_RunnerExitsRightAfterConnecting(t *testing.T) {
	cmd := startupCmdRunner(map[string]cmdResult{
		"tart exec runner-abc tail -20 /tmp/runner.log": {
			output: []byte("Listening for Jobs\nAn error occured: Runner version v2.334.0 is deprecated and cannot receive messages."),
		},
	})
	// Alive while connecting, gone once GitHub refuses its first poll.
	cmd.sequences = map[string][]cmdResult{
		"tart exec runner-abc pgrep -f Runner.Listener": {{}, {err: errors.New("exit status 1")}},
	}
	p := newTestTartProvider(cmd)

	_, err := p.StartInstance(context.Background(), "runner-abc", "jit")
	if err == nil || !strings.Contains(err.Error(), "is deprecated") {
		t.Fatalf("StartInstance() error = %v, want one carrying the refusal from the runner log", err)
	}
	if callIndex(cmd.getCalls(), "tart", "delete", "runner-abc") < 0 {
		t.Error("VM whose runner was refused was not deleted")
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

func TestTartProvider_RemoveInstanceAlreadyAbsent(t *testing.T) {
	cmd := &mockCommandRunner{
		results: map[string]cmdResult{
			"tart stop":   {err: fmt.Errorf("virtual machine not found")},
			"tart delete": {err: fmt.Errorf("virtual machine does not exist")},
		},
	}
	b := newTestTartProvider(cmd)
	if err := b.RemoveInstance(context.Background(), "missing"); err != nil {
		t.Fatalf("RemoveInstance() error for absent VM: %v", err)
	}
}

func TestTartProvider_RemoveInstanceHonorsDeadline(t *testing.T) {
	cmd := &blockingContextCommandRunner{}
	b := &TartProvider{logger: slog.New(slog.DiscardHandler), cmd: cmd}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := b.RemoveInstance(ctx, "runner-abc")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RemoveInstance() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("RemoveInstance() ignored deadline; elapsed = %s", elapsed)
	}
	cmd.mu.Lock()
	defer cmd.mu.Unlock()
	if len(cmd.hadDeadlines) != 2 || !cmd.hadDeadlines[0] || !cmd.hadDeadlines[1] {
		t.Fatalf("command deadline observations = %v, want [true true]", cmd.hadDeadlines)
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

// tartListWithLocalVMs is `tart list --format json` with a golden VM, a
// similarly named old one, and the OCI tag runner clones from. tart escapes
// "/" in JSON, so OCI names never appear verbatim in the raw output.
const tartListWithLocalVMs = `[
  {"State":"stopped","Disk":140,"Accessed":"2026-10-07T20:47:57Z","Name":"ghcr.io\/cirruslabs\/macos-golden-gate-xcode:27","Source":"OCI","Running":false,"Size":81},
  {"State":"stopped","Disk":140,"Accessed":"2026-10-06T08:00:00Z","Name":"ios-golden","Source":"local","Running":false,"Size":82},
  {"State":"stopped","Disk":120,"Accessed":"2026-09-01T08:00:00Z","Name":"macos-base-old","Source":"local","Running":false,"Size":60}
]`

func TestTartProvider_EnsureImage(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		wantPull bool
	}{
		{name: "OCI image already cached", image: "ghcr.io/cirruslabs/macos-golden-gate-xcode:27", wantPull: false},
		{name: "local golden VM", image: "ios-golden", wantPull: false},
		{name: "image not present", image: "ghcr.io/cirruslabs/macos-tahoe-xcode:26.5", wantPull: true},
		{name: "only a similarly named VM", image: "macos-base", wantPull: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &mockCommandRunner{results: map[string]cmdResult{
				"tart list --format json": {output: []byte(tartListWithLocalVMs)},
				"tart pull":               {},
			}}
			p := newTestTartProvider(cmd)
			p.baseImage = tt.image

			if err := p.EnsureImage(context.Background()); err != nil {
				t.Fatalf("EnsureImage() error: %v", err)
			}
			pulled := callIndex(cmd.getCalls(), "tart", "pull", tt.image) >= 0
			if pulled != tt.wantPull {
				t.Errorf("pulled = %v, want %v; calls: %v", pulled, tt.wantPull, cmd.getCalls())
			}
		})
	}
}
