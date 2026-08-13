package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"

	"github.com/ysya/runscaler/internal/backend"
)

// ScalesetAPI abstracts the scaleset client methods used by Scaler.
type ScalesetAPI interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

// DiskChecker is the pre-job-start disk-pressure check startRunner consults
// before generating a JIT config for a new runner. internal/diskguard.Guard
// satisfies it: NeedsReclaim is a cheap per-filesystem statfs (no volume
// walking, no Measure), Sweep does the actual reclaim work when needed.
type DiskChecker interface {
	NeedsReclaim() (bool, error)
	Sweep(ctx context.Context) error
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
	reconcileMu    sync.Mutex
	desiredRunners int
	diskChecker    DiskChecker
}

// Compile-time check that Scaler implements listener.Scaler.
var _ listener.Scaler = (*Scaler)(nil)

// Option configures optional Scaler behavior at construction time.
type Option func(*Scaler)

// WithDiskChecker wires a pre-job-start disk-pressure check into the
// Scaler: startRunner consults it before starting a new runner and
// reclaims first if free space is low. Omitting this option (the default)
// leaves diskChecker nil, so startRunner skips the check entirely.
func WithDiskChecker(c DiskChecker) Option {
	return func(s *Scaler) {
		s.diskChecker = c
	}
}

// NewScaler creates a new Scaler instance. opts is variadic so existing
// call sites compile unchanged; see WithDiskChecker for the only option
// defined so far.
func NewScaler(scaleSetID, minRunners, maxRunners int, b backend.RunnerBackend, client ScalesetAPI, logger *slog.Logger, opts ...Option) *Scaler {
	s := &Scaler{
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
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// HandleDesiredRunnerCount scales runners up to match demand.
// Scale down is handled naturally via HandleJobCompleted.
func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	s.desiredRunners = count
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
				s.logger.Error("Failed to start runner, continuing with available runners",
					slog.Any("error", err),
				)
				break
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

	// Cheap statfs before committing to a job: a runner that fills the disk
	// mid-build can take the whole host down. Reclaim failures are logged,
	// never fatal — refusing to serve jobs is worse than a full disk warning.
	if s.diskChecker != nil {
		if need, err := s.diskChecker.NeedsReclaim(); err != nil {
			s.logger.Warn("Disk check failed, starting runner anyway", slog.Any("error", err))
		} else if need {
			s.logger.Info("Free space below threshold, reclaiming before starting runner")
			if err := s.diskChecker.Sweep(ctx); err != nil {
				s.logger.Warn("Reclaim before runner start failed", slog.Any("error", err))
			}
		}
	}

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
	if watcher, ok := s.backend.(backend.RunnerWatcher); ok {
		go s.watchRunner(ctx, watcher, name, resourceID)
	}
	return name, nil
}

func (s *Scaler) watchRunner(ctx context.Context, watcher backend.RunnerWatcher, name, resourceID string) {
	for {
		err := watcher.WaitRunner(ctx, resourceID)
		if ctx.Err() != nil || !s.runners.contains(name, resourceID) {
			return
		}
		if err == nil {
			break
		}
		s.logger.Warn("Runner monitor failed; retrying", slog.String("name", name), slog.Any("error", err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	if !s.runners.markDead(name, resourceID) {
		return // normal JobCompleted path already removed it from state
	}
	s.logger.Warn("Runner exited before job completion; removing stale capacity", slog.String("name", name))
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	if err := s.backend.RemoveRunner(cleanCtx, resourceID); err != nil {
		s.logger.Warn("Failed to clean up exited runner", slog.String("name", name), slog.Any("error", err))
	}
	cancel()

	// Replace the lost capacity immediately instead of waiting for another
	// desired-count message, which may never arrive while a job remains queued.
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	target := min(s.maxRunners, s.minRunners+s.desiredRunners)
	if s.runners.count() >= target {
		return
	}
	if _, err := s.startRunner(ctx); err != nil {
		s.logger.Error("Failed to replace exited runner", slog.Any("error", err))
	}
}

// Shutdown force-removes all managed runners in parallel.
// The provided context should already be detached from cancellation
// (e.g. via context.WithoutCancel); this method adds a timeout to
// prevent hanging if a backend operation is unresponsive.
func (s *Scaler) Shutdown(ctx context.Context) {
	s.logger.Info("Shutting down runners")

	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	s.runners.mu.Lock()

	var wg sync.WaitGroup
	removeRunner := func(label, name, resourceID string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.logger.Debug("Removing "+label+" runner", slog.String("name", name))
			if err := s.backend.RemoveRunner(shutdownCtx, resourceID); err != nil {
				s.logger.Error("Failed to remove "+label+" runner", slog.String("name", name), slog.Any("error", err))
			}
		}()
	}

	for name, resourceID := range s.runners.idle {
		removeRunner("idle", name, resourceID)
	}
	for name, resourceID := range s.runners.busy {
		removeRunner("busy", name, resourceID)
	}
	clear(s.runners.idle)
	clear(s.runners.busy)

	s.runners.mu.Unlock()

	wg.Wait()
	s.backend.Shutdown(shutdownCtx)
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

func (r *runnerState) contains(name, resourceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if got, ok := r.idle[name]; ok && got == resourceID {
		return true
	}
	got, ok := r.busy[name]
	return ok && got == resourceID
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

func (r *runnerState) markDead(name, resourceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if got, ok := r.idle[name]; ok && got == resourceID {
		delete(r.idle, name)
		return true
	}
	if got, ok := r.busy[name]; ok && got == resourceID {
		delete(r.busy, name)
		return true
	}
	return false
}
