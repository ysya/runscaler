package controller

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/actions/scaleset"
)

type pendingRegistration struct {
	name     string
	attempts int
	retryAt  time.Time
}

// Failed provider starts own no instance capacity, but retain their known
// GitHub identity until deregistration succeeds. Only one request per
// controller is in flight; unrelated instance reconciliation can continue.
func (s *ScaleSetController) queueRegistration(name string, id int) {
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	if s.registrations == nil {
		s.registrations = make(map[int]*pendingRegistration)
	}
	s.registrations[id] = &pendingRegistration{name: name}
}

func (s *ScaleSetController) hasPendingRegistration(name string) bool {
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	for _, r := range s.registrations {
		if r.name == name {
			return true
		}
	}
	return false
}

func (s *ScaleSetController) registrationCount() int {
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	return len(s.registrations)
}

func (s *ScaleSetController) claimRegistration() (int, bool) {
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	if s.registrationInFlight {
		return 0, false
	}
	for id, r := range s.registrations {
		if !time.Now().Before(r.retryAt) {
			s.registrationInFlight = true
			return id, true
		}
	}
	return 0, false
}

func (s *ScaleSetController) removeRegistration(parent context.Context, id int) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	err := s.scalesetClient.RemoveRunner(ctx, int64(id))
	cancel()
	s.registrationMu.Lock()
	s.registrationInFlight = false
	r := s.registrations[id]
	if r == nil {
		s.registrationMu.Unlock()
		return
	}
	if err == nil || errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError) {
		delete(s.registrations, id)
		s.registrationMu.Unlock()
		s.logger.Debug("Cleaned failed-start runner registration", slog.Int("runnerID", id))
		s.signalReconcile()
		return
	}
	r.attempts++
	delay := removalRetryDelay(r.attempts)
	r.retryAt = time.Now().Add(delay)
	s.registrationMu.Unlock()
	// JobStillRunningError also retains the registration. Never force removal
	// or destroy provider resources in response to a registration-only failure.
	s.logger.Warn("Failed-start runner registration retained for retry", slog.Int("runnerID", id), slog.Duration("retryIn", delay), slog.Any("error", err))
	if !s.draining.Load() && parent.Err() == nil {
		time.AfterFunc(delay, s.signalReconcile)
	}
	s.signalReconcile()
}

// Shutdown has stopped reconciliation before this final bounded attempt.
// Failure leaves the remote registration for the opt-in recovery sweep.
func (s *ScaleSetController) flushRegistrations(ctx context.Context) {
	s.registrationMu.Lock()
	ids := make([]int, 0, len(s.registrations))
	for id := range s.registrations {
		ids = append(ids, id)
	}
	s.registrationMu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.removeRegistration(ctx, id)
	}
}
