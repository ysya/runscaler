package controller

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/ysya/runscaler/internal/diagnostics"
	"github.com/ysya/runscaler/internal/metrics"
	"github.com/ysya/runscaler/internal/provider"
)

func WithDiagnostics(store *diagnostics.Store, scaleSet, kind string, secrets ...string) Option {
	return func(s *ScaleSetController) {
		s.diagnostics = store
		s.diagnosticScaleSet = scaleSet
		s.diagnosticProvider = kind
		s.diagnosticSecrets = slices.Clone(secrets)
	}
}

func (s *ScaleSetController) collectFailure(ctx context.Context, name, id string) {
	collector, ok := s.provider.(provider.InstanceDiagnostics)
	if !ok || s.diagnostics == nil {
		return
	}
	reason, jit, claimed := s.instances.claimDiagnostics(name, id)
	if !claimed {
		return
	}
	diagCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d, err := collector.CollectDiagnostics(diagCtx, id)
	// Keep known metadata even when a bounded log read fails. Provider errors
	// can contain private daemon data and are deliberately not logged here.
	if err != nil {
		s.logger.Warn("Runner failure diagnostics collection incomplete", slog.String("name", name))
	}
	s.runtimeMu.Lock()
	s.runtime.LastFailure = &metrics.FailureSummary{Reason: reason, Timestamp: time.Now().UTC(), ExitCode: d.ExitCode, OOMKilled: d.OOMKilled, FinishedAt: d.FinishedAt}
	s.runtimeMu.Unlock()
	secrets := append(slices.Clone(s.diagnosticSecrets), jit)
	_, err = s.diagnostics.Save(diagCtx, diagnostics.Record{ScaleSet: s.diagnosticScaleSet, Provider: s.diagnosticProvider, Runner: name, Instance: id, Reason: reason, ExitDiagnostics: d}, secrets...)
	if err != nil {
		s.logger.Warn("Runner failure report could not be saved", slog.String("name", name))
	}
}
