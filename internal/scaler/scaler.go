package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"

	"github.com/ysya/runscaler/internal/backend"
)

// ScalesetAPI abstracts the scaleset client methods used by Scaler.
type ScalesetAPI interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

// Scaler implements listener.Scaler to handle scaling decisions
// and manage runner lifecycle via a pluggable RunnerBackend.
type Scaler struct {
	runners        runnerState
	scaleSetID     int
	backend        backend.RunnerBackend
	scalesetClient ScalesetAPI
	minRunners     int
	maxRunners     int
	logger         *slog.Logger
}

// Compile-time check that Scaler implements listener.Scaler.
var _ listener.Scaler = (*Scaler)(nil)

// NewScaler creates a new Scaler instance.
func NewScaler(scaleSetID, minRunners, maxRunners int, b backend.RunnerBackend, client ScalesetAPI, logger *slog.Logger) *Scaler {
	return &Scaler{
		scaleSetID:     scaleSetID,
		backend:        b,
		scalesetClient: client,
		minRunners:     minRunners,
		maxRunners:     maxRunners,
		logger:         logger,
		runners: runnerState{
			idle: make(map[string]string),
			busy: make(map[string]string),
		},
	}
}

// HandleDesiredRunnerCount scales runners up to match demand.
// Scale down is handled naturally via HandleJobCompleted.
func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	currentCount := s.runners.count()
	targetRunnerCount := min(s.maxRunners, s.minRunners+count)

	switch {
	case targetRunnerCount == currentCount:
		return currentCount, nil
	case targetRunnerCount > currentCount:
		scaleUp := targetRunnerCount - currentCount
		s.logger.Info(
			"Scaling up runners",
			slog.Int("currentCount", currentCount),
			slog.Int("desiredCount", targetRunnerCount),
			slog.Int("scaleUp", scaleUp),
		)
		for range scaleUp {
			if _, err := s.startRunner(ctx); err != nil {
				return 0, fmt.Errorf("failed to start runner: %w", err)
			}
		}
		return s.runners.count(), nil
	default:
		// Scale down is handled by HandleJobCompleted removing containers.
		return currentCount, nil
	}
}

// HandleJobStarted marks a runner as busy when a job is assigned.
func (s *Scaler) HandleJobStarted(ctx context.Context, jobInfo *scaleset.JobStarted) error {
	s.logger.Debug(
		"Job started",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
		slog.String("runnerName", jobInfo.RunnerName),
	)
	if !s.runners.markBusy(jobInfo.RunnerName) {
		s.logger.Warn("Job started for unknown runner (already removed?)", slog.String("runnerName", jobInfo.RunnerName))
	}
	return nil
}

// HandleJobCompleted removes the runner after job finishes.
func (s *Scaler) HandleJobCompleted(ctx context.Context, jobInfo *scaleset.JobCompleted) error {
	s.logger.Debug(
		"Job completed",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
		slog.String("runnerName", jobInfo.RunnerName),
	)

	resourceID, ok := s.runners.markDone(jobInfo.RunnerName)
	if !ok {
		s.logger.Warn("Job completed for unknown runner (already removed?)", slog.String("runnerName", jobInfo.RunnerName))
		return nil
	}
	if err := s.backend.RemoveRunner(ctx, resourceID); err != nil {
		return fmt.Errorf("failed to remove runner: %w", err)
	}

	return nil
}

// startRunner creates and starts a new ephemeral runner.
func (s *Scaler) startRunner(ctx context.Context) (string, error) {
	name := fmt.Sprintf("runner-%s", uuid.NewString()[:8])

	jit, err := s.scalesetClient.GenerateJitRunnerConfig(
		ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{
			Name: name,
		},
		s.scaleSetID,
	)
	if err != nil {
		return "", fmt.Errorf("failed to generate JIT config: %w", err)
	}

	resourceID, err := s.backend.StartRunner(ctx, name, jit.EncodedJITConfig)
	if err != nil {
		return "", err
	}

	s.runners.addIdle(name, resourceID)
	return name, nil
}

// Shutdown force-removes all managed runners.
func (s *Scaler) Shutdown(ctx context.Context) {
	s.logger.Info("Shutting down runners")
	s.runners.mu.Lock()
	defer s.runners.mu.Unlock()

	for name, resourceID := range s.runners.idle {
		s.logger.Debug("Removing idle runner", slog.String("name", name))
		if err := s.backend.RemoveRunner(ctx, resourceID); err != nil {
			s.logger.Error("Failed to remove idle runner", slog.String("name", name), slog.Any("error", err))
		}
	}
	clear(s.runners.idle)

	for name, resourceID := range s.runners.busy {
		s.logger.Debug("Removing busy runner", slog.String("name", name))
		if err := s.backend.RemoveRunner(ctx, resourceID); err != nil {
			s.logger.Error("Failed to remove busy runner", slog.String("name", name), slog.Any("error", err))
		}
	}
	clear(s.runners.busy)

	s.backend.Shutdown(ctx)
}

// RunnerCounts returns the number of idle and busy runners.
func (s *Scaler) RunnerCounts() (idle, busy int) {
	return s.runners.counts()
}

// --- Runner State ---

// runnerState tracks active runners with thread-safe access.
// Keys are runner names, values are backend-specific resource IDs.
type runnerState struct {
	mu   sync.Mutex
	idle map[string]string // name -> resourceID
	busy map[string]string // name -> resourceID
}

func (r *runnerState) count() int {
	r.mu.Lock()
	count := len(r.idle) + len(r.busy)
	r.mu.Unlock()
	return count
}

func (r *runnerState) counts() (idle, busy int) {
	r.mu.Lock()
	idle = len(r.idle)
	busy = len(r.busy)
	r.mu.Unlock()
	return
}

func (r *runnerState) addIdle(name, resourceID string) {
	r.mu.Lock()
	r.idle[name] = resourceID
	r.mu.Unlock()
}

func (r *runnerState) markBusy(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	resourceID, ok := r.idle[name]
	if !ok {
		return false
	}
	delete(r.idle, name)
	r.busy[name] = resourceID
	return true
}

func (r *runnerState) markDone(name string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if resourceID, ok := r.busy[name]; ok {
		delete(r.busy, name)
		return resourceID, true
	}
	if resourceID, ok := r.idle[name]; ok {
		delete(r.idle, name)
		return resourceID, true
	}
	return "", false
}
