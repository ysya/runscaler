package controller

import (
	"context"
	"time"

	"github.com/ysya/runscaler/internal/metrics"
)

// RuntimeStatus reports provisioning state without provider or GitHub I/O.
// Historical failures do not degrade a scale set whose demand is satisfied,
// and a normal wait for global host capacity is not an operational failure.
func (s *ScaleSetController) RuntimeStatus() metrics.RuntimeStatus {
	s.runtimeMu.Lock()
	status := s.runtime
	s.runtimeMu.Unlock()
	status.Draining = s.draining.Load()
	target, canScale := s.scaleTarget()
	idle, busy := s.instances.counts()
	status.Degraded = !status.Draining && !status.WaitingForCapacity && canScale && status.ConsecutiveStartFailures > 0 && idle+busy < target
	return status
}

func (s *ScaleSetController) recordStartAttempt(started time.Time) {
	s.runtimeMu.Lock()
	s.runtime.StartAttempts++
	s.runtime.LastStartAttempt = started
	s.runtimeMu.Unlock()
}

func (s *ScaleSetController) recordStartFailure(ctx context.Context, reason string) {
	if ctx.Err() != nil || s.draining.Load() {
		return
	}
	s.runtimeMu.Lock()
	s.runtime.StartFailures++
	s.runtime.ConsecutiveStartFailures++
	s.runtime.FailureReason = reason
	s.runtimeMu.Unlock()
}

func (s *ScaleSetController) recordStartSuccess() {
	s.runtimeMu.Lock()
	s.runtime.LastSuccessfulStart = time.Now()
	s.runtime.ConsecutiveStartFailures = 0
	s.runtime.FailureReason = ""
	s.runtimeMu.Unlock()
}

func (s *ScaleSetController) setCapacityWaiting(waiting bool) {
	s.runtimeMu.Lock()
	s.runtime.WaitingForCapacity = waiting
	s.runtimeMu.Unlock()
}
