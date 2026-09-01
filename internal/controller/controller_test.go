package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/ysya/runscaler/internal/capacity"
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

type blockingProvider struct {
	started chan string
	release chan struct{}
}

func (p *blockingProvider) StartInstance(ctx context.Context, name, _ string) (string, error) {
	p.started <- name
	select {
	case <-p.release:
		return "instance-" + name, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *blockingProvider) RemoveInstance(context.Context, string) error { return nil }
func (p *blockingProvider) Shutdown(context.Context)                     {}

type blockingCleanupProvider struct {
	removeStarted chan string
	release       chan struct{}
}

type selectiveCleanupProvider struct {
	mu        sync.Mutex
	started   []string
	attempted []string
	removed   []string
	failID    string
}

func (p *selectiveCleanupProvider) StartInstance(_ context.Context, name, _ string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := "instance-" + name
	p.started = append(p.started, id)
	return id, nil
}

func (p *selectiveCleanupProvider) RemoveInstance(_ context.Context, instanceID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempted = append(p.attempted, instanceID)
	if instanceID == p.failID {
		return errors.New("permanent cleanup failure")
	}
	p.removed = append(p.removed, instanceID)
	return nil
}

func (p *selectiveCleanupProvider) Shutdown(context.Context) {}

func (p *selectiveCleanupProvider) snapshot() (started, attempted, removed []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.started...), append([]string(nil), p.attempted...), append([]string(nil), p.removed...)
}

func (p *selectiveCleanupProvider) setFailure(instanceID string) {
	p.mu.Lock()
	p.failID = instanceID
	p.mu.Unlock()
}

func (p *blockingCleanupProvider) StartInstance(_ context.Context, name, _ string) (string, error) {
	return "instance-" + name, nil
}

func (p *blockingCleanupProvider) RemoveInstance(ctx context.Context, instanceID string) error {
	p.removeStarted <- instanceID
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *blockingCleanupProvider) Shutdown(context.Context) {}

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

func (m *watcherProvider) removedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.removed)
}

func (m *watcherProvider) firstStarted() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.started[0]
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

func (m *mockProvider) startedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.started)
}

func (m *mockProvider) removedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.removed)
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

type errorScaleset struct{ err error }

func (s *errorScaleset) GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return nil, s.err
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

func newTestController(t *testing.T, minRunners, maxRunners int) (*ScaleSetController, *mockProvider, *mockScaleset) {
	t.Helper()
	mb := &mockProvider{}
	ms := &mockScaleset{}
	s := NewScaleSetController(1, minRunners, maxRunners, mb, ms, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	return s, mb, ms
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatal(message)
	}
}

// --- instanceState tests ---

func TestRunnerStateLifecycle(t *testing.T) {
	rs := newInstanceState()

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
	if phase, ok := rs.phase("runner-1"); !ok || phase != instanceBusy {
		t.Errorf("runner-1 phase = %v, %v; want busy", phase, ok)
	}

	instanceID, ready, ok := rs.markDone("runner-1")
	if !ok {
		t.Fatal("markDone should return ok=true for busy runner")
	}
	if !ready {
		t.Fatal("markDone should report an existing instance ID as ready")
	}
	if instanceID != "instance-1" {
		t.Errorf("markDone returned %q, want %q", instanceID, "instance-1")
	}
	if rs.count() != 1 {
		t.Fatalf("count after markDone = %d, want 1", rs.count())
	}

	// markDone on idle runner (no job started)
	instanceID, ready, ok = rs.markDone("runner-2")
	if !ok {
		t.Fatal("markDone should return ok=true for idle runner")
	}
	if !ready {
		t.Fatal("markDone should report an existing instance ID as ready")
	}
	if instanceID != "instance-2" {
		t.Errorf("markDone(idle) returned %q, want %q", instanceID, "instance-2")
	}
	if rs.count() != 0 {
		t.Fatalf("count after all done = %d, want 0", rs.count())
	}
}

func TestRunnerStateMarkBusyReturnsFalse(t *testing.T) {
	rs := newInstanceState()

	if rs.markBusy("nonexistent") {
		t.Error("markBusy on non-existent runner should return false")
	}
}

func TestRunnerStateMarkDoneReturnsFalse(t *testing.T) {
	rs := newInstanceState()

	if _, _, ok := rs.markDone("nonexistent"); ok {
		t.Error("markDone on non-existent runner should return ok=false")
	}
}

func TestRunnerStateConcurrency(t *testing.T) {
	rs := newInstanceState()

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

func TestRunnerStateTracksProvisioningToRemoving(t *testing.T) {
	b := capacity.NewBroker(1)
	a := b.Register("test", 0)
	lease, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rs := newInstanceState()
	if !rs.reserve("runner-1", lease) {
		t.Fatal("reserve returned false")
	}
	if phase, _ := rs.phase("runner-1"); phase != instanceProvisioning {
		t.Fatalf("phase = %v, want provisioning", phase)
	}
	if !rs.markBusy("runner-1") {
		t.Fatal("job start should be recorded while provider startup is in flight")
	}
	if remove, tracked := rs.markReady("runner-1", "instance-1"); remove || !tracked {
		t.Fatalf("markReady() = remove %v tracked %v, want false/true", remove, tracked)
	}
	if phase, _ := rs.phase("runner-1"); phase != instanceBusy {
		t.Fatalf("phase = %v after markReady, want busy", phase)
	}
	instanceID, ready, ok := rs.markDone("runner-1")
	if !ok || !ready || instanceID != "instance-1" {
		t.Fatalf("markDone() = %q/%v/%v, want instance-1/true/true", instanceID, ready, ok)
	}
	released, ok := rs.finishRemoval("runner-1", "instance-1")
	if !ok {
		t.Fatal("finishRemoval returned false")
	}
	released.Release()
	if got := b.InUse(); got != 0 {
		t.Fatalf("capacity in use = %d, want 0", got)
	}
}

// --- ScaleSetController tests ---

func TestHandleDesiredRunnerCount_ScaleUp(t *testing.T) {
	s, mb, ms := newTestController(t, 0, 10)
	ctx := context.Background()

	got, err := s.HandleDesiredRunnerCount(ctx, 3)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}
	if got < 0 || got > 3 {
		t.Errorf("returned in-progress count = %d, want between 0 and 3", got)
	}
	waitFor(t, func() bool { return mb.startedCount() == 3 }, "background reconcile did not start 3 runners")
	if len(mb.started) != 3 {
		t.Errorf("runners started = %d, want 3", len(mb.started))
	}
	if ms.generated != 3 {
		t.Errorf("JIT configs generated = %d, want 3", ms.generated)
	}
}

func TestHandleDesiredRunnerCountDoesNotBlockOnProvider(t *testing.T) {
	p := &blockingProvider{started: make(chan string, 2), release: make(chan struct{})}
	b := capacity.NewBroker(1)
	a := b.Register("test", 0)
	s := NewScaleSetController(1, 0, 2, p, &mockScaleset{}, slog.New(slog.DiscardHandler), WithCapacityAllocator(a))
	defer s.Shutdown(context.Background())

	returned := make(chan error, 1)
	go func() {
		_, err := s.HandleDesiredRunnerCount(context.Background(), 2)
		returned <- err
	}()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("desired-count callback blocked on provider startup")
	}

	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("background reconciler did not start provider work")
	}
	provisioning, idle, busy, removing := s.InstanceLifecycleCounts()
	if provisioning != 1 || idle != 0 || busy != 0 || removing != 0 {
		t.Fatalf("lifecycle counts = %d/%d/%d/%d, want 1/0/0/0", provisioning, idle, busy, removing)
	}

	// A repeat desired-count message must observe the provisioning reservation
	// instead of launching duplicate work.
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.started:
		t.Fatal("duplicate provider start launched while the first was provisioning")
	case <-time.After(25 * time.Millisecond):
	}
	close(p.release)
}

func TestSharedCapacityBrokerLimitsScaleSetsHostWide(t *testing.T) {
	b := capacity.NewBroker(1)
	a1 := b.Register("one", 0)
	a2 := b.Register("two", 0)
	p1 := &mockProvider{}
	p2 := &mockProvider{}
	s1 := NewScaleSetController(1, 0, 2, p1, &mockScaleset{}, slog.New(slog.DiscardHandler), WithCapacityAllocator(a1))
	s2 := NewScaleSetController(2, 0, 2, p2, &mockScaleset{}, slog.New(slog.DiscardHandler), WithCapacityAllocator(a2))
	defer s1.Shutdown(context.Background())
	defer s2.Shutdown(context.Background())

	if _, err := s1.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p1.startedCount()+p2.startedCount() == 1 }, "neither scale set acquired global capacity")
	time.Sleep(25 * time.Millisecond)
	if got := p1.startedCount() + p2.startedCount(); got != 1 {
		t.Fatalf("provider starts across scale sets = %d, want host-wide limit 1", got)
	}
	if got := b.InUse(); got != 1 {
		t.Fatalf("capacity in use = %d, want 1", got)
	}
}

func TestStartInstanceReleasesCapacityWhenJITFails(t *testing.T) {
	b := capacity.NewBroker(1)
	a := b.Register("test", 0)
	s := NewScaleSetController(1, 0, 1, &mockProvider{}, &errorScaleset{err: errors.New("jit unavailable")}, slog.New(slog.DiscardHandler), WithCapacityAllocator(a))
	defer s.Shutdown(context.Background())

	if _, err := s.startInstance(context.Background()); err == nil {
		t.Fatal("startInstance succeeded, want JIT error")
	}
	if got := b.InUse(); got != 0 {
		t.Fatalf("capacity in use after failed startup = %d, want 0", got)
	}
	if got := s.instances.count(); got != 0 {
		t.Fatalf("active instances after failed startup = %d, want 0", got)
	}
}

func TestHandleJobCompletedDoesNotBlockOnProviderCleanup(t *testing.T) {
	p := &blockingCleanupProvider{removeStarted: make(chan string, 1), release: make(chan struct{})}
	b := capacity.NewBroker(1)
	a := b.Register("test", 0)
	s := NewScaleSetController(1, 0, 1, p, &mockScaleset{}, slog.New(slog.DiscardHandler), WithCapacityAllocator(a))
	defer s.Shutdown(context.Background())

	name, err := s.startInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: name}); err != nil {
		t.Fatal(err)
	}

	returned := make(chan error, 1)
	go func() {
		returned <- s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: name})
	}()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("job-completed callback blocked on provider cleanup")
	}
	select {
	case <-p.removeStarted:
	case <-time.After(time.Second):
		t.Fatal("background reconciler did not begin provider cleanup")
	}
	if got := b.InUse(); got != 1 {
		t.Fatalf("capacity released before provider cleanup finished: in use = %d", got)
	}
	_, _, _, removing := s.InstanceLifecycleCounts()
	if removing != 1 {
		t.Fatalf("removing count = %d, want 1", removing)
	}
	close(p.release)
	waitFor(t, func() bool { return b.InUse() == 0 }, "provider cleanup did not release capacity")
}

func TestCleanupFailureDoesNotBlockOtherInstances(t *testing.T) {
	b := capacity.NewBroker(2)
	a := b.Register("test", 0)
	p := &selectiveCleanupProvider{}
	s := NewScaleSetController(1, 0, 2, p, &mockScaleset{}, slog.New(slog.DiscardHandler), WithCapacityAllocator(a))
	defer s.Shutdown(context.Background())

	if _, err := s.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		started, _, _ := p.snapshot()
		return len(started) == 2
	}, "initial runners did not start")
	started, _, _ := p.snapshot()
	p.setFailure(started[0])
	for _, instanceID := range started {
		name := strings.TrimPrefix(instanceID, "instance-")
		if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: name}); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, func() bool {
		_, attempted, removed := p.snapshot()
		return len(attempted) >= 2 && len(removed) == 1
	}, "a permanent removal failure blocked unrelated cleanup")
	if got := b.InUse(); got != 1 {
		t.Fatalf("capacity in use = %d, want only the failed cleanup lease", got)
	}
	_, _, _, removing := s.InstanceLifecycleCounts()
	if removing != 1 {
		t.Fatalf("removing runners = %d, want 1 failed cleanup", removing)
	}
}

func TestCleanupFailureDoesNotBlockReplacementWhenCapacityRemains(t *testing.T) {
	b := capacity.NewBroker(2)
	a := b.Register("test", 0)
	p := &selectiveCleanupProvider{}
	s := NewScaleSetController(1, 0, 2, p, &mockScaleset{}, slog.New(slog.DiscardHandler), WithCapacityAllocator(a))
	defer s.Shutdown(context.Background())

	if _, err := s.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		started, _, _ := p.snapshot()
		return len(started) == 1
	}, "initial runner did not start")
	started, _, _ := p.snapshot()
	p.setFailure(started[0])
	name := strings.TrimPrefix(started[0], "instance-")
	if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		started, _, _ := p.snapshot()
		return len(started) == 2
	}, "failed cleanup blocked a replacement despite spare global capacity")
	if got := b.InUse(); got != 2 {
		t.Fatalf("capacity in use = %d, want failed cleanup plus replacement", got)
	}
}

func TestJobCompletionWaitsForFreshDesiredBeforeReplacement(t *testing.T) {
	b := capacity.NewBroker(1)
	a := b.Register("test", 0)
	p := &mockProvider{}
	s := NewScaleSetController(1, 0, 1, p, &mockScaleset{}, slog.New(slog.DiscardHandler), WithCapacityAllocator(a))
	defer s.Shutdown(context.Background())

	name, err := s.startInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: name}); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: name}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p.removedCount() == 1 }, "completed runner was not cleaned up")
	time.Sleep(25 * time.Millisecond)
	if got := p.startedCount(); got != 1 {
		t.Fatalf("stale desired count started %d runners, want only the completed runner", got)
	}

	// This mirrors the callback that follows JobCompleted in listener.handleMessage.
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	if got := p.startedCount(); got != 1 {
		t.Fatalf("fresh zero desired count started a replacement; starts = %d", got)
	}
}

func TestExitedRunnerIsRemovedAndReplaced(t *testing.T) {
	b := &watcherProvider{waits: make(map[string]chan struct{})}
	s := NewScaleSetController(1, 0, 2, b, &mockScaleset{}, slog.New(slog.DiscardHandler))
	defer s.Shutdown(context.Background())
	ctx := context.Background()

	if _, err := s.HandleDesiredRunnerCount(ctx, 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.startedCount() == 1 }, "initial runner did not start")
	b.crashFirst()
	waitFor(t, func() bool { return b.removedCount() == 1 }, "crashed runner was not cleaned up")
	if _, err := s.HandleDesiredRunnerCount(ctx, 1); err != nil {
		t.Fatal(err)
	}

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

func TestCrashMarksDemandStaleBeforeCapacityWaiterWakes(t *testing.T) {
	b := capacity.NewBroker(1)
	a := b.Register("test", 0)
	p := &watcherProvider{waits: make(map[string]chan struct{})}
	s := NewScaleSetController(1, 0, 2, p, &mockScaleset{}, slog.New(slog.DiscardHandler), WithCapacityAllocator(a))
	defer s.Shutdown(context.Background())

	// The second desired runner blocks in Allocator.Acquire while the first
	// runner owns the only global slot.
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p.startedCount() == 1 }, "initial runner did not start")
	p.crashFirst()
	waitFor(t, func() bool { return p.removedCount() == 1 }, "crashed runner was not cleaned up")
	time.Sleep(25 * time.Millisecond)
	if got := p.startedCount(); got != 1 {
		t.Fatalf("capacity waiter used stale demand after crash; starts = %d", got)
	}

	if _, err := s.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p.startedCount() == 2 }, "fresh demand did not replace crashed runner")
}

func TestExitedRunnerReplacementConvergesAfterDelayedCompletion(t *testing.T) {
	b := capacity.NewBroker(2)
	a := b.Register("test", 0)
	p := &watcherProvider{waits: make(map[string]chan struct{})}
	s := NewScaleSetController(1, 0, 2, p, &mockScaleset{}, slog.New(slog.DiscardHandler), WithCapacityAllocator(a))
	defer s.Shutdown(context.Background())

	if _, err := s.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p.startedCount() == 1 }, "initial runner did not start")
	oldRunner := strings.TrimPrefix(p.firstStarted(), "instance-")
	p.crashFirst()
	waitFor(t, func() bool { return p.removedCount() == 1 }, "crashed runner was not cleaned up")

	// A fresh poll can still report the old job before GitHub delivers its
	// delayed completion, so a speculative replacement is valid here.
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p.startedCount() == 2 }, "crashed runner was not replaced")
	if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: oldRunner}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p.removedCount() == 2 }, "surplus replacement was not removed")
	waitFor(t, func() bool { return b.InUse() == 0 }, "surplus replacement retained global capacity")
	if idle, busy := s.InstanceCounts(); idle != 0 || busy != 0 {
		t.Fatalf("counts = idle %d busy %d, want 0/0", idle, busy)
	}
}

func TestHandleDesiredRunnerCount_RespectsMax(t *testing.T) {
	s, mb, _ := newTestController(t, 0, 5)
	ctx := context.Background()

	got, err := s.HandleDesiredRunnerCount(ctx, 100)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}
	if got < 0 || got > 5 {
		t.Errorf("returned in-progress count = %d, want between 0 and 5", got)
	}
	waitFor(t, func() bool { return mb.startedCount() == 5 }, "background reconcile did not respect maxRunners")
	if len(mb.started) != 5 {
		t.Errorf("runners started = %d, want 5", len(mb.started))
	}
}

func TestHandleDesiredRunnerCount_WithMinRunners(t *testing.T) {
	s, mb, _ := newTestController(t, 2, 10)
	ctx := context.Background()

	// With 0 assigned jobs, target = min(10, 2+0) = 2
	got, err := s.HandleDesiredRunnerCount(ctx, 0)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount(0) error: %v", err)
	}
	if got < 0 || got > 2 {
		t.Errorf("returned in-progress count = %d, want between 0 and 2", got)
	}
	waitFor(t, func() bool { return mb.startedCount() == 2 }, "background reconcile did not satisfy minRunners")
	if len(mb.started) != 2 {
		t.Errorf("runners started = %d, want 2", len(mb.started))
	}
}

func TestHandleDesiredRunnerCount_NoScaleWhenEqual(t *testing.T) {
	s, mb, _ := newTestController(t, 0, 10)
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

func TestHandleDesiredRunnerCount_ScalesDownSurplusIdle(t *testing.T) {
	s, mb, _ := newTestController(t, 0, 10)
	ctx := context.Background()

	// Pre-populate 5 runners
	for i := range 5 {
		s.instances.addIdle(fmt.Sprintf("runner-%d", i), fmt.Sprintf("r%d", i))
	}

	// A fresh lower desired count removes only the surplus idle runners.
	got, err := s.HandleDesiredRunnerCount(ctx, 2)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount() error: %v", err)
	}
	if got != 2 {
		t.Errorf("returned desired count = %d, want 2", got)
	}
	if got := mb.startedCount(); got != 0 {
		t.Errorf("should not start runners, started = %d", got)
	}
	waitFor(t, func() bool { return mb.removedCount() == 3 }, "surplus idle runners were not removed")
	if idle, busy := s.InstanceCounts(); idle != 2 || busy != 0 {
		t.Fatalf("counts = idle %d busy %d, want 2/0", idle, busy)
	}
}

func TestHandleJobStarted(t *testing.T) {
	s, _, _ := newTestController(t, 0, 10)
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

	if phase, ok := s.instances.phase("runner-abc"); !ok || phase != instanceBusy {
		t.Errorf("runner phase = %v, %v; want busy", phase, ok)
	}
}

func TestHandleJobCompleted(t *testing.T) {
	s, mb, _ := newTestController(t, 0, 10)
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
	waitFor(t, func() bool { return mb.removedCount() == 1 }, "background reconcile did not remove completed runner")
	if len(mb.removed) != 1 {
		t.Errorf("runners removed = %d, want 1", len(mb.removed))
	}
	if mb.removed[0] != "instance-abc" {
		t.Errorf("removed resource = %q, want %q", mb.removed[0], "instance-abc")
	}
}

func TestShutdown(t *testing.T) {
	s, mb, _ := newTestController(t, 0, 10)
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
		_, _, _ = s.instances.markDone("runner-busy")
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
	waitFor(t, func() bool { return b.startedCount() == 1 }, "initial runner did not start")
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
// startInstance may be called once per runner during a background reconcile
// burst, so each individual reclaim still needs an aggregate bound.
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
