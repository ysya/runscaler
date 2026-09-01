package controller

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

type mockProvider struct {
	mu       sync.Mutex
	started  []string // runner names
	removed  []string // instance IDs
	shutdown bool
}

type watcherProvider struct {
	mu      sync.Mutex
	started []string
	removed []string
	waits   map[string]chan struct{}
}

func (m *watcherProvider) StartInstance(_ context.Context, name, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	resource := "instance-" + name
	m.started = append(m.started, resource)
	m.waits[resource] = make(chan struct{})
	return resource, nil
}

func (m *watcherProvider) RemoveInstance(_ context.Context, resource string) error {
	m.mu.Lock()
	m.removed = append(m.removed, resource)
	m.mu.Unlock()
	return nil
}

func (m *watcherProvider) Shutdown(context.Context) {}

func (m *watcherProvider) WaitInstance(ctx context.Context, resource string) error {
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

func (m *watcherProvider) crashFirst() {
	m.mu.Lock()
	defer m.mu.Unlock()
	close(m.waits[m.started[0]])
}

func (m *watcherProvider) startedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.started)
}

func (m *mockProvider) StartInstance(_ context.Context, name string, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, name)
	return "instance-" + name, nil
}

func (m *mockProvider) RemoveInstance(_ context.Context, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, instanceID)
	return nil
}

func (m *mockProvider) Shutdown(_ context.Context) {
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
// one. It also records the deadline Sweep was handed and can block until
// that deadline expires, so the timeout wrapped around the sweep is
// testable.
type fakeChecker struct {
	needs       bool
	needsErr    error
	sweepErr    error
	sweepBlocks bool // Sweep waits for its context to end, simulating a slow store
	sweeps      int

	sweepDeadline    time.Time
	sweepHadDeadline bool
}

func (f *fakeChecker) NeedsReclaim() (bool, error) {
	return f.needs, f.needsErr
}

func (f *fakeChecker) Sweep(ctx context.Context) error {
	f.sweeps++
	f.sweepDeadline, f.sweepHadDeadline = ctx.Deadline()
	if f.sweepBlocks {
		<-ctx.Done()
	}
	return f.sweepErr
}

func newTestController(minRunners, maxRunners int) (*ScaleSetController, *mockProvider, *mockScaleset) {
	mb := &mockProvider{}
	ms := &mockScaleset{}
	s := NewScaleSetController(1, minRunners, maxRunners, mb, ms, slog.New(slog.DiscardHandler))
	return s, mb, ms
}

// --- instanceState tests ---

func TestRunnerStateLifecycle(t *testing.T) {
	rs := instanceState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	if rs.count() != 0 {
		t.Fatalf("initial count = %d, want 0", rs.count())
	}

	rs.addIdle("runner-1", "instance-1")
	rs.addIdle("runner-2", "instance-2")
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

	instanceID, ok := rs.markDone("runner-1")
	if !ok {
		t.Fatal("markDone should return ok=true for busy runner")
	}
	if instanceID != "instance-1" {
		t.Errorf("markDone returned %q, want %q", instanceID, "instance-1")
	}
	if rs.count() != 1 {
		t.Fatalf("count after markDone = %d, want 1", rs.count())
	}

	// markDone on idle runner (no job started)
	instanceID, ok = rs.markDone("runner-2")
	if !ok {
		t.Fatal("markDone should return ok=true for idle runner")
	}
	if instanceID != "instance-2" {
		t.Errorf("markDone(idle) returned %q, want %q", instanceID, "instance-2")
	}
	if rs.count() != 0 {
		t.Fatalf("count after all done = %d, want 0", rs.count())
	}
}

func TestRunnerStateMarkBusyReturnsFalse(t *testing.T) {
	rs := instanceState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	if rs.markBusy("nonexistent") {
		t.Error("markBusy on non-existent runner should return false")
	}
}

func TestRunnerStateMarkDoneReturnsFalse(t *testing.T) {
	rs := instanceState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	if _, ok := rs.markDone("nonexistent"); ok {
		t.Error("markDone on non-existent runner should return ok=false")
	}
}

func TestRunnerStateConcurrency(t *testing.T) {
	rs := instanceState{
		idle: make(map[string]string),
		busy: make(map[string]string),
	}

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("runner-%d", i)
			rs.addIdle(name, fmt.Sprintf("instance-%d", i))
		}(i)
	}
	wg.Wait()

	if rs.count() != 100 {
		t.Errorf("concurrent addIdle count = %d, want 100", rs.count())
	}
}

// --- ScaleSetController tests ---

func TestHandleDesiredRunnerCount_ScaleUp(t *testing.T) {
	s, mb, ms := newTestController(0, 10)
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
	b := &watcherProvider{waits: make(map[string]chan struct{})}
	s := NewScaleSetController(1, 0, 2, b, &mockScaleset{}, slog.New(slog.DiscardHandler))
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
	if idle, busy := s.InstanceCounts(); idle != 1 || busy != 0 {
		t.Fatalf("counts = idle %d busy %d, want 1/0", idle, busy)
	}
}

func TestHandleDesiredRunnerCount_RespectsMax(t *testing.T) {
	s, mb, _ := newTestController(0, 5)
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
	s, mb, _ := newTestController(2, 10)
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
	s, mb, _ := newTestController(0, 10)
	ctx := context.Background()

	// Pre-populate 3 idle runners
	s.instances.addIdle("runner-1", "r1")
	s.instances.addIdle("runner-2", "r2")
	s.instances.addIdle("runner-3", "r3")

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
	s, mb, _ := newTestController(0, 10)
	ctx := context.Background()

	// Pre-populate 5 runners
	for i := range 5 {
		s.instances.addIdle(fmt.Sprintf("runner-%d", i), fmt.Sprintf("r%d", i))
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
	s, _, _ := newTestController(0, 10)
	ctx := context.Background()

	s.instances.addIdle("runner-abc", "instance-abc")

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

	if _, ok := s.instances.idle["runner-abc"]; ok {
		t.Error("runner should not be idle after job started")
	}
	if _, ok := s.instances.busy["runner-abc"]; !ok {
		t.Error("runner should be busy after job started")
	}
}

func TestHandleJobCompleted(t *testing.T) {
	s, mb, _ := newTestController(0, 10)
	ctx := context.Background()

	s.instances.addIdle("runner-abc", "instance-abc")
	s.instances.markBusy("runner-abc")

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

	if s.instances.count() != 0 {
		t.Errorf("runner count = %d, want 0 after job completed", s.instances.count())
	}
	if len(mb.removed) != 1 {
		t.Errorf("runners removed = %d, want 1", len(mb.removed))
	}
	if mb.removed[0] != "instance-abc" {
		t.Errorf("removed resource = %q, want %q", mb.removed[0], "instance-abc")
	}
}

func TestShutdown(t *testing.T) {
	s, mb, _ := newTestController(0, 10)
	ctx := context.Background()

	s.instances.addIdle("idle-1", "r-idle-1")
	s.instances.addIdle("busy-1", "r-busy-1")
	s.instances.markBusy("busy-1")

	s.Shutdown(ctx)

	if s.instances.count() != 0 {
		t.Errorf("runner count = %d after shutdown, want 0", s.instances.count())
	}
	if len(mb.removed) != 2 {
		t.Errorf("runners removed = %d, want 2", len(mb.removed))
	}
	if !mb.shutdown {
		t.Error("provider.Shutdown() was not called")
	}
}

func TestDrain_RemovesIdleKeepsBusy(t *testing.T) {
	mb := &mockProvider{}
	s := NewScaleSetController(1, 0, 5, mb, &mockScaleset{}, slog.New(slog.DiscardHandler))
	s.instances.addIdle("runner-idle", "res-idle")
	s.instances.addIdle("runner-busy", "res-busy")
	s.instances.markBusy("runner-busy")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = s.Drain(ctx)

	if !containsString(mb.removed, "res-idle") {
		t.Error("idle runner should be removed immediately — it holds no work")
	}
	if containsString(mb.removed, "res-busy") {
		t.Error("busy runner was removed during drain; its job is still running")
	}
	if !s.IsDraining() {
		t.Error("controller should remain in draining state")
	}
}

func TestDrain_ReturnsWhenBusyReachesZero(t *testing.T) {
	mb := &mockProvider{}
	s := NewScaleSetController(1, 0, 5, mb, &mockScaleset{}, slog.New(slog.DiscardHandler))
	s.instances.addIdle("runner-busy", "res-busy")
	s.instances.markBusy("runner-busy")

	go func() {
		time.Sleep(50 * time.Millisecond)
		s.instances.markDone("runner-busy")
	}()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("Drain should return as soon as the last busy runner finishes")
	}
}

func TestDrain_StopsScalingUp(t *testing.T) {
	mb := &mockProvider{}
	s := NewScaleSetController(1, 0, 5, mb, &mockScaleset{}, slog.New(slog.DiscardHandler))

	if err := s.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	got, err := s.HandleDesiredRunnerCount(context.Background(), 3)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	if got != 0 || len(mb.started) != 0 {
		t.Errorf("draining controller started %d runners (count=%d); it must take no new work",
			len(mb.started), got)
	}
}

func TestExitedRunnerIsNotReplacedWhileDraining(t *testing.T) {
	b := &watcherProvider{waits: make(map[string]chan struct{})}
	s := NewScaleSetController(1, 0, 2, b, &mockScaleset{}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := s.HandleDesiredRunnerCount(ctx, 1); err != nil {
		t.Fatal(err)
	}
	s.draining.Store(true)
	b.crashFirst()

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if idle, busy := s.InstanceCounts(); idle == 0 && busy == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("crashed runner was not removed from state")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := b.startedCount(); got != 1 {
		t.Fatalf("started runners = %d, want no replacement while draining", got)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// --- startInstance disk-check tests ---

func TestStartInstance_ReclaimsWhenDiskLow(t *testing.T) {
	chk := &fakeChecker{needs: true}
	s := NewScaleSetController(1, 0, 1, &mockProvider{}, &mockScaleset{}, slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	if _, err := s.startInstance(context.Background()); err != nil {
		t.Fatalf("startInstance error: %v", err)
	}
	if chk.sweeps != 1 {
		t.Errorf("expected one reclaim before starting the runner, got %d", chk.sweeps)
	}
}

func TestStartInstance_SkipsReclaimWhenDiskFine(t *testing.T) {
	chk := &fakeChecker{needs: false}
	s := NewScaleSetController(1, 0, 1, &mockProvider{}, &mockScaleset{}, slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	if _, err := s.startInstance(context.Background()); err != nil {
		t.Fatalf("startInstance error: %v", err)
	}
	if chk.sweeps != 0 {
		t.Errorf("healthy disk must not pay for a sweep, got %d", chk.sweeps)
	}
}

func TestStartInstance_ProceedsWhenReclaimFails(t *testing.T) {
	chk := &fakeChecker{needs: true, sweepErr: errors.New("daemon down")}
	s := NewScaleSetController(1, 0, 1, &mockProvider{}, &mockScaleset{}, slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	if _, err := s.startInstance(context.Background()); err != nil {
		t.Fatalf("a failed reclaim must not block the job: %v", err)
	}
}

// TestStartInstance_ProceedsWhenNeedsReclaimErrors pins the subtlety in
// diskguard.Guard.NeedsReclaim's own doc comment: on total statfs failure
// it returns (true, err) — "cannot tell, assume we should reclaim". The
// call site must not chase that bool once err is non-nil; it logs and
// starts the runner without even attempting a sweep.
func TestStartInstance_ProceedsWhenNeedsReclaimErrors(t *testing.T) {
	chk := &fakeChecker{needs: true, needsErr: errors.New("statfs failed for all stores")}
	s := NewScaleSetController(1, 0, 1, &mockProvider{}, &mockScaleset{}, slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	if _, err := s.startInstance(context.Background()); err != nil {
		t.Fatalf("a failed disk check must not block the job: %v", err)
	}
	if chk.sweeps != 0 {
		t.Errorf("NeedsReclaim error must skip the sweep entirely, got %d", chk.sweeps)
	}
}

// TestStartInstance_ReclaimIsBounded pins the timeout the spec's
// error-handling table requires around the pre-job sweep. Each store
// carries its own 10-minute bound, but a sweep walks all of them, so an
// unbounded aggregate lets one job start wait for their sum — and
// startInstance is called once per runner inside HandleDesiredRunnerCount's
// scale-up loop, with reconcileMu held throughout.
func TestStartInstance_ReclaimIsBounded(t *testing.T) {
	chk := &fakeChecker{needs: true}
	s := NewScaleSetController(1, 0, 1, &mockProvider{}, &mockScaleset{}, slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	// A parent with no deadline of its own: any deadline Sweep sees must
	// have come from startInstance.
	if _, err := s.startInstance(context.Background()); err != nil {
		t.Fatalf("startInstance error: %v", err)
	}
	if !chk.sweepHadDeadline {
		t.Fatal("Sweep was handed a context with no deadline — the pre-job reclaim is unbounded")
	}
	if budget := time.Until(chk.sweepDeadline); budget > preJobReclaimTimeout {
		t.Errorf("Sweep deadline is %s away, want <= preJobReclaimTimeout (%s)", budget, preJobReclaimTimeout)
	}
}

// TestReclaimBeforeStart_ReturnsWhenTheSweepOverruns pins the behaviour on
// timeout: the reclaim is abandoned and the caller carries on to start the
// job. A reclaim that cannot finish must never turn into a refusal to serve
// jobs — the timeout is driven through the testable core so the assertion
// costs milliseconds rather than preJobReclaimTimeout.
func TestReclaimBeforeStart_ReturnsWhenTheSweepOverruns(t *testing.T) {
	chk := &fakeChecker{needs: true, sweepBlocks: true}
	s := NewScaleSetController(1, 0, 1, &mockProvider{}, &mockScaleset{}, slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.reclaimBeforeStartWith(context.Background(), 50*time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a sweep that never finishes still blocks the job start — the timeout is not in effect")
	}
	if chk.sweeps != 1 {
		t.Errorf("sweeps = %d, want 1", chk.sweeps)
	}
}

// TestStartInstance_ProceedsAfterAnOverrunningReclaim is the end-to-end
// counterpart: a sweep abandoned by its bound must still leave startInstance
// returning a live runner, not an error. It drives the whole path — check,
// bounded sweep, JIT config, provider start — with a sweep that only ends
// when its context does.
func TestStartInstance_ProceedsAfterAnOverrunningReclaim(t *testing.T) {
	chk := &fakeChecker{needs: true, sweepBlocks: true}
	s := NewScaleSetController(1, 0, 1, &mockProvider{}, &mockScaleset{}, slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	// A parent deadline shorter than preJobReclaimTimeout ends the blocked
	// sweep here, so this costs milliseconds; the bound startInstance applies
	// on its own is asserted above.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	name, err := s.startInstance(ctx)
	if err != nil {
		t.Fatalf("an abandoned reclaim must not block the job: %v", err)
	}
	if name == "" {
		t.Error("startInstance returned no runner name")
	}
}
