package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/actions/scaleset"
)

// notifyRecorder counts start-failure notices and records, for each one,
// whether the failed runner was still local when the notice came.
type notifyRecorder struct {
	mu         sync.Mutex
	s          *ScaleSetController
	name       func() string
	notices    int
	stillLocal []bool
}

func (n *notifyRecorder) notify() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notices++
	if n.s != nil && n.name != nil {
		n.stillLocal = append(n.stillLocal, n.s.HasRunner(n.name()))
	}
}

func (n *notifyRecorder) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.notices
}

// namingFailProvider fails every start and remembers the runner name.
type namingFailProvider struct {
	mockProvider
	mu   sync.Mutex
	last string
}

func (p *namingFailProvider) StartInstance(_ context.Context, name, _ string) (string, error) {
	p.mu.Lock()
	p.last = name
	p.mu.Unlock()
	return "", errors.New("boot failed")
}

func (p *namingFailProvider) lastName() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// A start that fails after the JIT registration sends one notice, and only
// after the runner has left local state.
func TestStartFailureNotifiesAfterRunnerLeavesLocalState(t *testing.T) {
	p := &namingFailProvider{}
	rec := &notifyRecorder{name: p.lastName}
	s := NewScaleSetController(1, 0, 1, p, &mockScaleset{}, slog.New(slog.DiscardHandler), WithStartFailureNotifier(rec.notify))
	rec.mu.Lock()
	rec.s = s
	rec.mu.Unlock()
	defer s.Shutdown(context.Background())
	if _, err := s.startInstance(context.Background()); err == nil {
		t.Fatal("expected failed start")
	}
	if rec.count() != 1 {
		t.Fatalf("notices = %d, want 1", rec.count())
	}
	if rec.stillLocal[0] {
		t.Fatal("notice came while the failed runner was still local")
	}
}

// A JIT request that fails may still have created the registration on
// GitHub, so it notifies too.
func TestJITFailureNotifies(t *testing.T) {
	rec := &notifyRecorder{}
	s := NewScaleSetController(1, 0, 1, &mockProvider{}, &errorScaleset{err: errors.New("jit unavailable")}, slog.New(slog.DiscardHandler), WithStartFailureNotifier(rec.notify))
	defer s.Shutdown(context.Background())
	if _, err := s.startInstance(context.Background()); err == nil {
		t.Fatal("expected failed start")
	}
	if rec.count() != 1 {
		t.Fatalf("notices = %d, want 1", rec.count())
	}
}

// Reconcile passes, scale-ups, completions and scale-downs that succeed send
// no notice: the sweep never runs per reconcile pass.
func TestReconcileWithoutStartFailureNeverNotifies(t *testing.T) {
	rec := &notifyRecorder{}
	mp := &mockProvider{}
	s := NewScaleSetController(1, 0, 3, mp, &mockScaleset{}, slog.New(slog.DiscardHandler), WithStartFailureNotifier(rec.notify))
	defer s.Shutdown(context.Background())
	for round := range 5 {
		if _, err := s.HandleDesiredRunnerCount(context.Background(), 3); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { idle, _ := s.InstanceCounts(); return idle == 3 }, "scale-up did not finish")
		mp.mu.Lock()
		started := append([]string(nil), mp.started...)
		mp.mu.Unlock()
		name := started[len(started)-1]
		_ = s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: name})
		_ = s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: name})
		if _, err := s.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return s.instances.count() == 0 }, "scale-down did not finish")
		if rec.count() != 0 {
			t.Fatalf("round %d: %d notices without a start failure", round, rec.count())
		}
	}
}

func TestHasRunnerCoversEveryPhase(t *testing.T) {
	r := newInstanceState()
	if !r.reserve("runner-0000000a", nil) {
		t.Fatal("reserve failed")
	}
	for _, phase := range []instancePhase{instanceProvisioning, instanceIdle, instanceBusy, instanceRemoving} {
		r.instances["runner-0000000a"].phase = phase
		if !r.hasName("runner-0000000a") {
			t.Fatalf("phase %d: name not reported local", phase)
		}
	}
	r.instances["runner-0000000a"].phase = instanceProvisioning
	r.failProvision("runner-0000000a")
	if r.hasName("runner-0000000a") {
		t.Fatal("failed provision still reported local")
	}
}
