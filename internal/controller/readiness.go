package controller

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ysya/runscaler/internal/provider"
)

func (s *ScaleSetController) waitReady(ctx context.Context, ready provider.InstanceReadiness, name, id string, attempt startAttempt) {
	defer s.observerWG.Done()
	err := ready.WaitReady(ctx, id)
	if ctx.Err() != nil {
		return
	}
	if errors.Is(err, provider.ErrExitedBeforeReady) {
		s.handleExit(ctx, name, id)
		return
	}
	failed := err != nil && !errors.Is(err, provider.ErrReadinessDisabled)
	reason := ""
	if failed {
		reason = "readiness_unavailable"
		if errors.Is(err, provider.ErrReadinessTimeout) {
			reason = "readiness_timeout"
		}
		if errors.Is(err, provider.ErrExitedBeforeReady) {
			reason = "runner_exited_before_ready"
		}
	}
	if !s.instances.readinessResult(name, id, reason) {
		return
	}
	if !failed {
		s.runtimeMu.Lock()
		s.runtime.Readiness = "verified"
		if err != nil {
			s.runtime.Readiness = "unverified"
		}
		s.runtimeMu.Unlock()
		s.recordStartSuccessFor(attempt)
		if err != nil {
			s.logger.Warn("Runner readiness is disabled; startup is unverified", slog.String("name", name))
		}
	} else {
		s.recordStartFailureFor(s.workCtx, reason, attempt)
	}
	s.signalReconcile()
}
