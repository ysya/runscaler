package controller

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/ysya/runscaler/internal/config"
)

func TestRetryDemandCannotBypassDeadline(t *testing.T) {
	s := NewScaleSetController(1, 1, 2, &mockProvider{}, &errorScaleset{err: errors.New("failure")}, slog.Default(), WithRetry(config.RetryConfig{Initial: 200 * time.Millisecond, Max: time.Second, CooldownAfter: 3, Cooldown: time.Second}))
	defer s.Shutdown(context.Background())
	_, _ = s.HandleDesiredRunnerCount(context.Background(), 0)
	waitFor(t, func() bool { return s.RuntimeStatus().StartFailures == 1 }, "no first failure")
	deadline := s.RuntimeStatus().NextRetryAt
	for time.Now().Before(deadline.Add(-5 * time.Millisecond)) {
		_, _ = s.HandleDesiredRunnerCount(context.Background(), 0)
		if s.RuntimeStatus().StartAttempts != 1 {
			t.Fatal("demand bypassed retry deadline")
		}
		time.Sleep(time.Millisecond)
	}
	if s.RuntimeStatus().NextRetryAt.IsZero() {
		t.Fatal("empty queue cleared minimum-runner retry")
	}
}
func TestOlderReadinessSuccessDoesNotClearLaterFailure(t *testing.T) {
	s := NewScaleSetController(1, 0, 2, &mockProvider{}, &mockScaleset{}, slog.Default())
	defer s.Shutdown(context.Background())
	old := s.recordStartAttempt(time.Now())
	later := s.recordStartAttempt(time.Now())
	s.recordStartFailureFor(context.Background(), "readiness_timeout", later)
	s.recordStartSuccessFor(old)
	if s.RuntimeStatus().ConsecutiveStartFailures != 1 {
		t.Fatal("older startup cleared newer failure")
	}
	s.recordStartSuccessFor(s.recordStartAttempt(time.Now()))
	if s.RuntimeStatus().ConsecutiveStartFailures != 0 {
		t.Fatal("fresh success did not reset retry")
	}
}

func TestOverlappingAttemptSuccessDoesNotClearNewFailureGeneration(t *testing.T) {
	s := NewScaleSetController(1, 0, 2, &mockProvider{}, &mockScaleset{}, slog.Default())
	defer s.Shutdown(context.Background())
	a := s.recordStartAttempt(time.Now())
	b := s.recordStartAttempt(time.Now())
	s.recordStartFailureFor(context.Background(), "readiness_timeout", a)
	deadline := s.RuntimeStatus().NextRetryAt
	s.recordStartSuccessFor(b)
	if s.RuntimeStatus().ConsecutiveStartFailures != 1 || s.RuntimeStatus().NextRetryAt != deadline {
		t.Fatal("overlapping success erased a newer failure generation")
	}
}

func TestWorkedRunnerExitDoesNotCountAsStartupFailure(t *testing.T) {
	p := &watcherProvider{waits: make(map[string]chan struct{})}
	s := NewScaleSetController(1, 0, 1, p, &mockScaleset{}, slog.Default())
	defer s.Shutdown(context.Background())
	name, err := s.startInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: name})
	p.crashFirst()
	waitFor(t, func() bool { return p.removedCount() == 1 }, "exited runner not cleaned")
	if status := s.RuntimeStatus(); status.StartFailures != 0 || !status.NextRetryAt.IsZero() {
		t.Fatalf("job exit caused startup retry: %+v", status)
	}
}
