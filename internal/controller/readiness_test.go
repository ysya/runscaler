package controller

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/ysya/runscaler/internal/provider"
)

type readinessProvider struct {
	mockProvider
	ready   chan error
	entered chan struct{}
}

func (p *readinessProvider) WaitReady(ctx context.Context, _ string) error {
	close(p.entered)
	select {
	case err := <-p.ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestReadinessKeepsProvisioningAndWorkWins(t *testing.T) {
	p := &readinessProvider{ready: make(chan error, 1), entered: make(chan struct{})}
	s := NewScaleSetController(1, 0, 1, p, &mockScaleset{}, slog.Default())
	defer s.Shutdown(context.Background())
	name, err := s.startInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-p.entered
	if phase, _ := s.instances.phase(name); phase != instanceProvisioning {
		t.Fatalf("phase=%v", phase)
	}
	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: name}); err != nil {
		t.Fatal(err)
	}
	p.ready <- nil
	waitFor(t, func() bool { phase, _ := s.instances.phase(name); return phase == instanceBusy }, "work must remain busy")
	if got := s.RuntimeStatus().StartFailures; got != 0 {
		t.Fatalf("failures=%d", got)
	}
}

func TestReadinessFailureProtectsAssignedRunner(t *testing.T) {
	p := &readinessProvider{ready: make(chan error, 1), entered: make(chan struct{})}
	ms := &mockScaleset{removeErr: map[int]error{1: scaleset.JobStillRunningError}}
	s := NewScaleSetController(1, 0, 1, p, ms, slog.Default())
	defer s.Shutdown(context.Background())
	name, err := s.startInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-p.entered
	p.ready <- provider.ErrReadinessTimeout
	waitFor(t, func() bool { phase, _ := s.instances.phase(name); return phase == instanceBusy }, "GitHub-assigned work was not retained")
	if p.removedCount() != 0 {
		t.Fatal("assigned runner removed")
	}
}

func TestReadinessMissingRunnerIDKeepsLease(t *testing.T) {
	p := &readinessProvider{ready: make(chan error, 1), entered: make(chan struct{})}
	s := NewScaleSetController(1, 0, 1, p, &missingIDClient{}, slog.Default())
	defer s.Shutdown(context.Background())
	name, err := s.startInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-p.entered
	p.ready <- provider.ErrReadinessUnavailable
	waitFor(t, func() bool { phase, _ := s.instances.phase(name); return phase == instanceRemoving }, "failure was not tracked")
	time.Sleep(20 * time.Millisecond)
	if p.removedCount() != 0 || s.instances.count() != 1 {
		t.Fatal("unknown GitHub identity must not destroy a live runner")
	}
}

type missingIDClient struct{ mockScaleset }

func (*missingIDClient) GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return &scaleset.RunnerScaleSetJitRunnerConfig{}, nil
}

func TestReadinessDrainCancelsWorker(t *testing.T) {
	p := &readinessProvider{ready: make(chan error), entered: make(chan struct{})}
	s := NewScaleSetController(1, 0, 1, p, &mockScaleset{}, slog.Default())
	defer s.Shutdown(context.Background())
	if _, err := s.startInstance(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-p.entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if s.RuntimeStatus().StartFailures != 0 {
		t.Fatal("cancellation counted as failure")
	}
}

func TestReadinessMissingIDDrainWaitsForDeadline(t *testing.T) {
	p := &readinessProvider{ready: make(chan error), entered: make(chan struct{})}
	s := NewScaleSetController(1, 0, 1, p, &missingIDClient{}, slog.New(slog.DiscardHandler))
	defer s.Shutdown(context.Background())
	if _, err := s.startInstance(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-p.entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unknown runner ID drain=%v", err)
	}
	if p.removedCount() != 0 {
		t.Fatal("drain destroyed unverified runner with unknown GitHub identity")
	}
}

type watchedReadinessProvider struct {
	readinessProvider
	exited chan struct{}
}

func (p *watchedReadinessProvider) WaitInstance(ctx context.Context, _ string) error {
	select {
	case <-p.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestReadinessConfirmedExitCannotRestoreBusy(t *testing.T) {
	p := &watchedReadinessProvider{readinessProvider: readinessProvider{ready: make(chan error, 1), entered: make(chan struct{})}, exited: make(chan struct{})}
	ms := &mockScaleset{removeErr: map[int]error{1: scaleset.JobStillRunningError}}
	s := NewScaleSetController(1, 0, 1, p, ms, slog.New(slog.DiscardHandler))
	defer s.Shutdown(context.Background())
	if _, err := s.startInstance(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-p.entered
	p.ready <- provider.ErrExitedBeforeReady
	waitFor(t, func() bool { return p.removedCount() == 1 }, "confirmed exited runner was restored busy")
	if s.RuntimeStatus().StartFailures != 1 {
		t.Fatal("exit was not counted once")
	}
}

type blockedBusyClient struct {
	mockScaleset
	entered, release chan struct{}
}

func (c *blockedBusyClient) RemoveRunner(ctx context.Context, _ int64) error {
	close(c.entered)
	select {
	case <-c.release:
		return scaleset.JobStillRunningError
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestReadinessTimeoutThenExitTakesOverDeregistration(t *testing.T) {
	p := &watchedReadinessProvider{readinessProvider: readinessProvider{ready: make(chan error, 1), entered: make(chan struct{})}, exited: make(chan struct{})}
	client := &blockedBusyClient{entered: make(chan struct{}), release: make(chan struct{})}
	s := NewScaleSetController(1, 0, 1, p, client, slog.New(slog.DiscardHandler))
	defer s.Shutdown(context.Background())
	name, err := s.startInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-p.entered
	p.ready <- provider.ErrReadinessTimeout
	<-client.entered
	close(p.exited)
	waitFor(t, func() bool {
		s.instances.mu.Lock()
		defer s.instances.mu.Unlock()
		return s.instances.instances[name].removeReason == reasonExited
	}, "watcher did not take over failed readiness")
	close(client.release)
	waitFor(t, func() bool { return p.removedCount() == 1 }, "exited runner was kept after GitHub busy response")
	if s.RuntimeStatus().StartFailures != 1 {
		t.Fatal("one failed startup was counted twice")
	}
}
