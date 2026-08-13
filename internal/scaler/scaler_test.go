package scaler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// --- Mocks ---

type mockBackend struct {
	mu       sync.Mutex
	started  []string // runner names
	removed  []string // resource IDs
	shutdown bool
}

type watcherBackend struct {
	mu      sync.Mutex
	started []string
	removed []string
	waits   map[string]chan struct{}
}

func (m *watcherBackend) StartRunner(_ context.Context, name, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	resource := "resource-" + name
	m.started = append(m.started, resource)
	m.waits[resource] = make(chan struct{})
	return resource, nil
}

func (m *watcherBackend) RemoveRunner(_ context.Context, resource string) error {
	m.mu.Lock()
	m.removed = append(m.removed, resource)
	m.mu.Unlock()
	return nil
}

func (m *watcherBackend) Shutdown(context.Context) {}

func (m *watcherBackend) WaitRunner(ctx context.Context, resource string) error {
	m.mu.Lock()
	w := m.waits[resource]
	m.mu.Unlock()
	select {
	case <-w:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *watcherBackend) crashFirst() {
	m.mu.Lock()
	defer m.mu.Unlock()
	close(m.waits[m.started[0]])
}

func (m *watcherBackend) startedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.started)
}

func (m *mockBackend) StartRunner(_ context.Context, name string, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, name)
	return "resource-" + name, nil
}

func (m *mockBackend) RemoveRunner(_ context.Context, resourceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, resourceID)
	return nil
}

func (m *mockBackend) Shutdown(_ context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdown = true
}

type mockScaleset struct {
	generated int
}

func (m *mockScaleset) GenerateJitRunnerConfig(_ context.Context, _ *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	m.generated++
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		EncodedJITConfig: "mock-jit-config",
	}, nil
}

// fakeChecker is a DiskChecker double for the pre-job-start disk-pressure
// check: it reports whatever NeedsReclaim/Sweep verdict a test configures
// and counts Sweep calls so a test can assert a healthy disk never pays for
// one.
type fakeChecker struct {
	needs    bool
	needsErr error
	sweepErr error
	sweeps   int
}

func (f *fakeChecker) NeedsReclaim() (bool, error) {
	return f.needs, f.needsErr
}

func (f *fakeChecker) Sweep(_ context.Context) error {
	f.sweeps++
	return f.sweepErr
}

func newTestScaler(minRunners, maxRunners int) (*Scaler, *mockBackend, *mockScaleset) {
	mb := &mockBackend{}
	ms := &mockScaleset{}
	s := NewScaler(1, minRunners, maxRunners, mb, ms, slog.New(slog.DiscardHandler))
	return s, mb, ms
}

// --- runnerState tests ---

func TestRunnerStateLifecycle(t *testing.T) {
	rs := runnerState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	if rs.count() != 0 {
		t.Fatalf("initial count = %d, want 0", rs.count())
	}

	rs.addIdle("runner-1", "resource-1")
	rs.addIdle("runner-2", "resource-2")
	if rs.count() != 2 {
		t.Fatalf("count after addIdle = %d, want 2", rs.count())
	}

	rs.markBusy("runner-1")
	if rs.count() != 2 {
		t.Fatalf("count after markBusy = %d, want 2", rs.count())
	}
	if _, ok := rs.idle["runner-1"]; ok {
		t.Error("runner-1 should not be in idle after markBusy")
	}
	if _, ok := rs.busy["runner-1"]; !ok {
		t.Error("runner-1 should be in busy after markBusy")
	}

	resourceID, ok := rs.markDone("runner-1")
	if !ok {
		t.Fatal("markDone should return ok=true for busy runner")
	}
	if resourceID != "resource-1" {
		t.Errorf("markDone returned %q, want %q", resourceID, "resource-1")
	}
	if rs.count() != 1 {
		t.Fatalf("count after markDone = %d, want 1", rs.count())
	}

	// markDone on idle runner (no job started)
	resourceID, ok = rs.markDone("runner-2")
	if !ok {
		t.Fatal("markDone should return ok=true for idle runner")
	}
	if resourceID != "resource-2" {
		t.Errorf("markDone(idle) returned %q, want %q", resourceID, "resource-2")
	}
	if rs.count() != 0 {
		t.Fatalf("count after all done = %d, want 0", rs.count())
	}
}

func TestRunnerStateMarkBusyReturnsFalse(t *testing.T) {
	rs := runnerState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	if rs.markBusy("nonexistent") {
		t.Error("markBusy on non-existent runner should return false")
	}
}

func TestRunnerStateMarkDoneReturnsFalse(t *testing.T) {
	rs := runnerState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	if _, ok := rs.markDone("nonexistent"); ok {
		t.Error("markDone on non-existent runner should return ok=false")
	}
}

func TestRunnerStateConcurrency(t *testing.T) {
	rs := runnerState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("runner-%d", i)
			rs.addIdle(name, fmt.Sprintf("resource-%d", i))
		}(i)
	}
	wg.Wait()

	if rs.count() != 100 {
		t.Errorf("concurrent addIdle count = %d, want 100", rs.count())
	}
}

// --- Scaler tests ---

func TestHandleDesiredRunnerCount_ScaleUp(t *testing.T) {
	s, mb, ms := newTestScaler(0, 10)
	ctx := context.Background()

	got, err := s.HandleDesiredRunnerCount(ctx, 3)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}
	if got != 3 {
		t.Errorf("returned count = %d, want 3", got)
	}
	if len(mb.started) != 3 {
		t.Errorf("runners started = %d, want 3", len(mb.started))
	}
	if ms.generated != 3 {
		t.Errorf("JIT configs generated = %d, want 3", ms.generated)
	}
}

func TestExitedRunnerIsRemovedAndReplaced(t *testing.T) {
	b := &watcherBackend{waits: make(map[string]chan struct{})}
	s := NewScaler(1, 0, 2, b, &mockScaleset{}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := s.HandleDesiredRunnerCount(ctx, 1); err != nil {
		t.Fatal(err)
	}
	b.crashFirst()

	deadline := time.Now().Add(2 * time.Second)
	for b.startedCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := b.startedCount(); got != 2 {
		t.Fatalf("started runners = %d, want crashed runner plus replacement", got)
	}
	if idle, busy := s.RunnerCounts(); idle != 1 || busy != 0 {
		t.Fatalf("counts = idle %d busy %d, want 1/0", idle, busy)
	}
}

func TestHandleDesiredRunnerCount_RespectsMax(t *testing.T) {
	s, mb, _ := newTestScaler(0, 5)
	ctx := context.Background()

	got, err := s.HandleDesiredRunnerCount(ctx, 100)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}
	if got != 5 {
		t.Errorf("returned count = %d, want 5 (maxRunners)", got)
	}
	if len(mb.started) != 5 {
		t.Errorf("runners started = %d, want 5", len(mb.started))
	}
}

func TestHandleDesiredRunnerCount_WithMinRunners(t *testing.T) {
	s, mb, _ := newTestScaler(2, 10)
	ctx := context.Background()

	// With 0 assigned jobs, target = min(10, 2+0) = 2
	got, err := s.HandleDesiredRunnerCount(ctx, 0)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount(0) error: %v", err)
	}
	if got != 2 {
		t.Errorf("returned count = %d, want 2 (minRunners)", got)
	}
	if len(mb.started) != 2 {
		t.Errorf("runners started = %d, want 2", len(mb.started))
	}
}

func TestHandleDesiredRunnerCount_NoScaleWhenEqual(t *testing.T) {
	s, mb, _ := newTestScaler(0, 10)
	ctx := context.Background()

	// Pre-populate 3 idle runners
	s.runners.addIdle("runner-1", "r1")
	s.runners.addIdle("runner-2", "r2")
	s.runners.addIdle("runner-3", "r3")

	got, err := s.HandleDesiredRunnerCount(ctx, 3)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}
	if got != 3 {
		t.Errorf("returned count = %d, want 3", got)
	}
	if len(mb.started) != 0 {
		t.Errorf("should not start runners when count matches, started = %d", len(mb.started))
	}
}

func TestHandleDesiredRunnerCount_NoScaleDown(t *testing.T) {
	s, mb, _ := newTestScaler(0, 10)
	ctx := context.Background()

	// Pre-populate 5 runners
	for i := range 5 {
		s.runners.addIdle(fmt.Sprintf("runner-%d", i), fmt.Sprintf("r%d", i))
	}

	// Desired is 2, but we don't scale down (ephemeral runners removed via HandleJobCompleted)
	got, err := s.HandleDesiredRunnerCount(ctx, 2)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}
	if got != 5 {
		t.Errorf("returned count = %d, want 5 (no scale down)", got)
	}
	if len(mb.started) != 0 {
		t.Errorf("should not start runners, started = %d", len(mb.started))
	}
	if len(mb.removed) != 0 {
		t.Errorf("should not remove runners, removed = %d", len(mb.removed))
	}
}

func TestHandleJobStarted(t *testing.T) {
	s, _, _ := newTestScaler(0, 10)
	ctx := context.Background()

	s.runners.addIdle("runner-abc", "resource-abc")

	err := s.HandleJobStarted(ctx, &scaleset.JobStarted{
		JobMessageBase: scaleset.JobMessageBase{
			RunnerRequestID: 1,
			JobID:           "job-1",
		},
		RunnerName: "runner-abc",
	})
	if err != nil {
		t.Fatalf("HandleJobStarted() error: %v", err)
	}

	if _, ok := s.runners.idle["runner-abc"]; ok {
		t.Error("runner should not be idle after job started")
	}
	if _, ok := s.runners.busy["runner-abc"]; !ok {
		t.Error("runner should be busy after job started")
	}
}

func TestHandleJobCompleted(t *testing.T) {
	s, mb, _ := newTestScaler(0, 10)
	ctx := context.Background()

	s.runners.addIdle("runner-abc", "resource-abc")
	s.runners.markBusy("runner-abc")

	err := s.HandleJobCompleted(ctx, &scaleset.JobCompleted{
		JobMessageBase: scaleset.JobMessageBase{
			RunnerRequestID: 1,
			JobID:           "job-1",
		},
		RunnerName: "runner-abc",
	})
	if err != nil {
		t.Fatalf("HandleJobCompleted() error: %v", err)
	}

	if s.runners.count() != 0 {
		t.Errorf("runner count = %d, want 0 after job completed", s.runners.count())
	}
	if len(mb.removed) != 1 {
		t.Errorf("runners removed = %d, want 1", len(mb.removed))
	}
	if mb.removed[0] != "resource-abc" {
		t.Errorf("removed resource = %q, want %q", mb.removed[0], "resource-abc")
	}
}

func TestShutdown(t *testing.T) {
	s, mb, _ := newTestScaler(0, 10)
	ctx := context.Background()

	s.runners.addIdle("idle-1", "r-idle-1")
	s.runners.addIdle("busy-1", "r-busy-1")
	s.runners.markBusy("busy-1")

	s.Shutdown(ctx)

	if s.runners.count() != 0 {
		t.Errorf("runner count = %d after shutdown, want 0", s.runners.count())
	}
	if len(mb.removed) != 2 {
		t.Errorf("runners removed = %d, want 2", len(mb.removed))
	}
	if !mb.shutdown {
		t.Error("backend.Shutdown() was not called")
	}
}

// --- startRunner disk-check tests ---

func TestStartRunner_ReclaimsWhenDiskLow(t *testing.T) {
	chk := &fakeChecker{needs: true}
	s := NewScaler(1, 0, 1, &mockBackend{}, &mockScaleset{}, slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	if _, err := s.startRunner(context.Background()); err != nil {
		t.Fatalf("startRunner error: %v", err)
	}
	if chk.sweeps != 1 {
		t.Errorf("expected one reclaim before starting the runner, got %d", chk.sweeps)
	}
}

func TestStartRunner_SkipsReclaimWhenDiskFine(t *testing.T) {
	chk := &fakeChecker{needs: false}
	s := NewScaler(1, 0, 1, &mockBackend{}, &mockScaleset{}, slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	if _, err := s.startRunner(context.Background()); err != nil {
		t.Fatalf("startRunner error: %v", err)
	}
	if chk.sweeps != 0 {
		t.Errorf("healthy disk must not pay for a sweep, got %d", chk.sweeps)
	}
}

func TestStartRunner_ProceedsWhenReclaimFails(t *testing.T) {
	chk := &fakeChecker{needs: true, sweepErr: errors.New("daemon down")}
	s := NewScaler(1, 0, 1, &mockBackend{}, &mockScaleset{}, slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	if _, err := s.startRunner(context.Background()); err != nil {
		t.Fatalf("a failed reclaim must not block the job: %v", err)
	}
}

// TestStartRunner_ProceedsWhenNeedsReclaimErrors pins the subtlety in
// diskguard.Guard.NeedsReclaim's own doc comment: on total statfs failure
// it returns (true, err) — "cannot tell, assume we should reclaim". The
// call site must not chase that bool once err is non-nil; it logs and
// starts the runner without even attempting a sweep.
func TestStartRunner_ProceedsWhenNeedsReclaimErrors(t *testing.T) {
	chk := &fakeChecker{needs: true, needsErr: errors.New("statfs failed for all stores")}
	s := NewScaler(1, 0, 1, &mockBackend{}, &mockScaleset{}, slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	if _, err := s.startRunner(context.Background()); err != nil {
		t.Fatalf("a failed disk check must not block the job: %v", err)
	}
	if chk.sweeps != 0 {
		t.Errorf("NeedsReclaim error must skip the sweep entirely, got %d", chk.sweeps)
	}
}
