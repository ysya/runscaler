package controller

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/ysya/runscaler/internal/config"
)

func WithRetry(settings config.RetryConfig) Option {
	return func(s *ScaleSetController) { s.retryConfig = settings }
}

func (s *ScaleSetController) recordStartFailureFor(ctx context.Context, reason string, attempt startAttempt) {
	if ctx.Err() != nil || s.draining.Load() {
		return
	}
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if s.draining.Load() {
		return
	}
	if (reason == "readiness_timeout" || reason == "readiness_unavailable" || reason == "runner_exited_before_ready") && s.instances.hasWork(attempt) {
		return
	}
	s.runtime.StartFailures++
	s.runtime.ConsecutiveStartFailures++
	s.runtime.FailureReason = reason
	s.failureGeneration++
	s.lastFailureAttempt = attempt.id
	failures := int(s.runtime.ConsecutiveStartFailures)
	delay := s.retryConfig.Delay(failures, rand.Float64())
	s.runtime.NextRetryAt = time.Now().Add(delay)
	s.runtime.RetryState = "backoff"
	if s.retryConfig.CoolingDown(failures) {
		s.runtime.RetryState = "cooldown"
	}
	if s.retryTimer != nil {
		s.retryTimer.Stop()
	}
	s.retryTimer = time.AfterFunc(delay, s.signalReconcile)
}

func (s *ScaleSetController) recordStartSuccessFor(attempt startAttempt) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	s.runtime.LastSuccessfulStart = time.Now()
	if attempt.generation != s.failureGeneration {
		return
	}
	s.runtime.ConsecutiveStartFailures = 0
	s.runtime.FailureReason = ""
	s.clearRetryLocked()
}
func (s *ScaleSetController) clearRetryLocked() {
	if s.retryTimer != nil {
		s.retryTimer.Stop()
		s.retryTimer = nil
	}
	s.runtime.NextRetryAt = time.Time{}
	s.runtime.RetryState = ""
}
func (s *ScaleSetController) retryAllowed() bool {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if time.Now().Before(s.runtime.NextRetryAt) {
		return false
	}
	s.clearRetryLocked()
	return true
}
func (s *ScaleSetController) clearSatisfiedRetry(target int) {
	idle, busy := s.instances.counts()
	if idle+busy < target {
		return
	}
	s.runtimeMu.Lock()
	s.clearRetryLocked()
	s.runtimeMu.Unlock()
}

type startAttempt struct{ id, generation uint64 }

func (s *ScaleSetController) recordWorkSuccessFor(attempt startAttempt) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	s.runtime.LastSuccessfulStart = time.Now()
	// Work may disprove this attempt's own readiness failure, but cannot erase
	// failures from overlapping attempts that happened after it began.
	sameGeneration := attempt.generation == s.failureGeneration
	ownFailure := s.failureGeneration == attempt.generation+1 && s.lastFailureAttempt == attempt.id
	if !sameGeneration && !ownFailure {
		return
	}
	s.runtime.ConsecutiveStartFailures = 0
	s.runtime.FailureReason = ""
	s.clearRetryLocked()
}
