package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"

	"github.com/ysya/runscaler/internal/provider"
)

// ScalesetAPI abstracts the scaleset client methods used by ScaleSetController.
type ScalesetAPI interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

// DiskChecker is the pre-job-start disk-pressure check startInstance consults
// before generating a JIT config for a new runner. internal/diskguard.Guard
// satisfies it: NeedsReclaim is a cheap per-filesystem statfs (no volume
// walking, no Measure), Sweep does the actual reclaim work when needed.
type DiskChecker interface {
	NeedsReclaim() (bool, error)
	Sweep(ctx context.Context) error
}

// ScaleSetController implements listener.Scaler to handle scaling decisions
// and manage runner lifecycle via a pluggable InstanceProvider.
type ScaleSetController struct {
	instances      instanceState
	scaleSetID     int
	provider       provider.InstanceProvider
	scalesetClient ScalesetAPI
	minRunners     int
	maxRunners     int
	logger         *slog.Logger
	reconcileMu    sync.Mutex
	desiredRunners int
	diskChecker    DiskChecker
	draining       atomic.Bool
}

// Compile-time check that ScaleSetController implements listener.Scaler.
var _ listener.Scaler = (*ScaleSetController)(nil)

// Option configures optional ScaleSetController behavior at construction time.
type Option func(*ScaleSetController)

// WithDiskChecker wires a pre-job-start disk-pressure check into the
// ScaleSetController: startInstance consults it before starting a new runner and
// reclaims first if free space is low. Omitting this option (the default)
// leaves diskChecker nil, so startInstance skips the check entirely.
func WithDiskChecker(c DiskChecker) Option {
	return func(s *ScaleSetController) {
		s.diskChecker = c
	}
}

// NewScaleSetController creates a controller for one GitHub runner scale set.
// See WithDiskChecker for the optional pre-start capacity check.
func NewScaleSetController(scaleSetID, minRunners, maxRunners int, p provider.InstanceProvider, client ScalesetAPI, logger *slog.Logger, opts ...Option) *ScaleSetController {
	s := &ScaleSetController{
		scaleSetID:     scaleSetID,
		provider:       p,
		scalesetClient: client,
		minRunners:     minRunners,
		maxRunners:     maxRunners,
		logger:         logger,
		instances: instanceState{
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
func (s *ScaleSetController) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if s.draining.Load() {
		return s.instances.count(), nil
	}
	s.desiredRunners = count
	currentCount := s.instances.count()
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
			if _, err := s.startInstance(ctx); err != nil {
				s.logger.Error("Failed to start runner, continuing with available runners",
					slog.Any("error", err),
				)
				break
			}
		}
		return s.instances.count(), nil
	default:
		// Scale down is handled by HandleJobCompleted removing containers.
		return currentCount, nil
	}
}

// HandleJobStarted marks a runner as busy when a job is assigned.
func (s *ScaleSetController) HandleJobStarted(ctx context.Context, jobInfo *scaleset.JobStarted) error {
	s.logger.Debug(
		"Job started",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
		slog.String("runnerName", jobInfo.RunnerName),
	)
	// Serialize the idle→busy transition with Drain's idle removal. If the
	// job-start message wins the lock, Drain sees a busy runner and preserves
	// it; if Drain wins, this runner was idle at the drain boundary and is
	// removed before the callback can claim it.
	s.reconcileMu.Lock()
	marked := s.instances.markBusy(jobInfo.RunnerName)
	s.reconcileMu.Unlock()
	if !marked {
		s.logger.Warn("Job started for unknown runner (already removed?)", slog.String("runnerName", jobInfo.RunnerName))
	}
	return nil
}

// HandleJobCompleted removes the runner after job finishes.
func (s *ScaleSetController) HandleJobCompleted(ctx context.Context, jobInfo *scaleset.JobCompleted) error {
	s.logger.Debug(
		"Job completed",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
		slog.String("runnerName", jobInfo.RunnerName),
	)

	instanceID, ok := s.instances.markDone(jobInfo.RunnerName)
	if !ok {
		s.logger.Warn("Job completed for unknown runner (already removed?)", slog.String("runnerName", jobInfo.RunnerName))
		return nil
	}
	if err := s.provider.RemoveInstance(ctx, instanceID); err != nil {
		return fmt.Errorf("failed to remove runner: %w", err)
	}

	return nil
}

// startInstance creates and starts a new ephemeral runner.
func (s *ScaleSetController) startInstance(ctx context.Context) (string, error) {
	name := fmt.Sprintf("runner-%s", uuid.NewString()[:8])

	// Cheap statfs before committing to a job: a runner that fills the disk
	// mid-build can take the whole host down. Reclaim failures are logged,
	// never fatal — refusing to serve jobs is worse than a full disk warning.
	if s.diskChecker != nil {
		if need, err := s.diskChecker.NeedsReclaim(); err != nil {
			s.logger.Warn("Disk check failed, starting runner anyway", slog.Any("error", err))
		} else if need {
			s.logger.Info("Free space below threshold, reclaiming before starting runner")
			s.reclaimBeforeStart(ctx)
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

	instanceID, err := s.provider.StartInstance(ctx, name, jit.EncodedJITConfig)
	if err != nil {
		return "", err
	}

	s.instances.addIdle(name, instanceID)
	if watcher, ok := s.provider.(provider.InstanceWatcher); ok {
		go s.watchInstance(ctx, watcher, name, instanceID)
	}
	return name, nil
}

// preJobReclaimTimeout bounds the synchronous reclaim startInstance performs
// when free space is low. This is how long a job start may be delayed, so
// it is chosen against both edges rather than picked for roundness:
//
//   - It must comfortably exceed the cheapest, highest-value tier's real
//     cost, or the reclaim would time out before doing the one thing it is
//     best at. Pruning a large dangling-image backlog has taken about a
//     minute on a real host, so anything at or under that would routinely
//     be cut off mid-Tier1.
//   - It must stay small next to the scale-up loop that multiplies it.
//     startInstance runs once per runner inside HandleDesiredRunnerCount while
//     reconcileMu is held, so scaling up five runners can serialize five
//     bounded reclaims with this scale set's listener blocked throughout.
//
// Nothing is lost when the deadline cuts a sweep short: the guard's own
// periodic ticker and the four store sweepers reclaim the remainder on
// their normal schedules. The job starts either way — the spec's
// error-handling table is explicit that a reclaim timeout warns and
// proceeds, never refuses service.
const preJobReclaimTimeout = 2 * time.Minute

// reclaimBeforeStart runs the disk guard's sweep under preJobReclaimTimeout.
// The individual stores carry their own 10-minute bounds, but nothing bounds
// the aggregate: a sweep walks every store on every filesystem, so without
// this the sum of those bounds is what a job start could wait for.
func (s *ScaleSetController) reclaimBeforeStart(ctx context.Context) {
	s.reclaimBeforeStartWith(ctx, preJobReclaimTimeout)
}

// reclaimBeforeStartWith is the testable core: it sweeps under the supplied
// timeout, so a test can drive the deadline path without waiting out
// preJobReclaimTimeout for real.
func (s *ScaleSetController) reclaimBeforeStartWith(ctx context.Context, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := s.diskChecker.Sweep(ctx); err != nil {
		s.logger.Warn("Reclaim before runner start failed", slog.Any("error", err))
	}
	// Only the deadline is worth reporting: a cancelled parent means the
	// process is shutting down, which is not a reclaim problem.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		s.logger.Warn("Reclaim before runner start timed out, starting runner anyway",
			slog.Duration("timeout", timeout))
	}
}

func (s *ScaleSetController) watchInstance(ctx context.Context, watcher provider.InstanceWatcher, name, instanceID string) {
	for {
		err := watcher.WaitInstance(ctx, instanceID)
		if ctx.Err() != nil || !s.instances.contains(name, instanceID) {
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
	if !s.instances.markDead(name, instanceID) {
		return // normal JobCompleted path already removed it from state
	}
	s.logger.Warn("Runner exited before job completion; removing stale capacity", slog.String("name", name))
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	if err := s.provider.RemoveInstance(cleanCtx, instanceID); err != nil {
		s.logger.Warn("Failed to clean up exited runner", slog.String("name", name), slog.Any("error", err))
	}
	cancel()

	// Replace the lost capacity immediately instead of waiting for another
	// desired-count message, which may never arrive while a job remains queued.
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if ctx.Err() != nil || s.draining.Load() {
		return
	}
	target := min(s.maxRunners, s.minRunners+s.desiredRunners)
	if s.instances.count() >= target {
		return
	}
	if _, err := s.startInstance(ctx); err != nil {
		s.logger.Error("Failed to replace exited runner", slog.Any("error", err))
	}
}

// Drain stops taking new work and waits for in-flight jobs to finish. Idle
// runners are removed immediately because they hold no work. It returns when
// no busy runners remain or ctx expires, whichever comes first.
//
// The listener must remain running while this method waits: JobCompleted
// messages delivered through the listener are what move busy runners to zero.
// The caller should cancel the listener only after Drain returns, then invoke
// Shutdown to force-remove anything left after a deadline or removal failure.
func (s *ScaleSetController) Drain(ctx context.Context) error {
	s.draining.Store(true)
	started := time.Now()

	// Wait for any in-progress scale-up to finish, then keep job-start
	// callbacks out while deciding which runners were idle at the drain
	// boundary. Successful removals are deleted from state; failed removals
	// stay tracked so the caller's final Shutdown can retry them.
	s.reconcileMu.Lock()
	for name, instanceID := range s.instances.idleSnapshot() {
		if err := s.provider.RemoveInstance(ctx, instanceID); err != nil {
			s.logger.Warn("Failed to remove idle runner during drain",
				slog.String("name", name),
				slog.Any("error", err))
			continue
		}
		s.instances.removeIdle(name, instanceID)
	}
	s.reconcileMu.Unlock()

	_, busy := s.instances.counts()
	s.logger.Info("Draining runners", slog.Int("busy", busy))
	if busy == 0 {
		return nil
	}

	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	progress := time.NewTicker(30 * time.Second)
	defer progress.Stop()

	for {
		select {
		case <-ctx.Done():
			_, busy = s.instances.counts()
			s.logger.Warn("Runner drain ended before all jobs completed",
				slog.Int("busy", busy),
				slog.Duration("waited", time.Since(started)),
				slog.Any("error", ctx.Err()))
			return ctx.Err()
		case <-poll.C:
			_, busy = s.instances.counts()
			if busy == 0 {
				s.logger.Info("Runner drain complete", slog.Duration("waited", time.Since(started)))
				return nil
			}
		case <-progress.C:
			_, busy = s.instances.counts()
			s.logger.Info("Waiting for in-flight jobs to finish",
				slog.Int("busy", busy),
				slog.Duration("waited", time.Since(started)))
		}
	}
}

// IsDraining reports whether this controller has stopped accepting new work.
func (s *ScaleSetController) IsDraining() bool {
	return s.draining.Load()
}

// Shutdown force-removes all managed runners in parallel.
// The provided context should already be detached from cancellation
// (e.g. via context.WithoutCancel); this method adds a timeout to
// prevent hanging if a provider operation is unresponsive.
func (s *ScaleSetController) Shutdown(ctx context.Context) {
	s.logger.Info("Shutting down runners")

	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	s.instances.mu.Lock()

	var wg sync.WaitGroup
	removeRunner := func(label, name, instanceID string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.logger.Debug("Removing "+label+" runner", slog.String("name", name))
			if err := s.provider.RemoveInstance(shutdownCtx, instanceID); err != nil {
				s.logger.Error("Failed to remove "+label+" runner", slog.String("name", name), slog.Any("error", err))
			}
		}()
	}

	for name, instanceID := range s.instances.idle {
		removeRunner("idle", name, instanceID)
	}
	for name, instanceID := range s.instances.busy {
		removeRunner("busy", name, instanceID)
	}
	clear(s.instances.idle)
	clear(s.instances.busy)

	s.instances.mu.Unlock()

	wg.Wait()
	s.provider.Shutdown(shutdownCtx)
}

// InstanceCounts returns the number of idle and busy runners.
func (s *ScaleSetController) InstanceCounts() (idle, busy int) {
	return s.instances.counts()
}

// --- Instance State ---

// instanceState tracks active runners with thread-safe access.
// Keys are runner names, values are provider-specific instance IDs.
type instanceState struct {
	mu   sync.Mutex
	idle map[string]string // name -> instanceID
	busy map[string]string // name -> instanceID
}

func (r *instanceState) count() int {
	r.mu.Lock()
	count := len(r.idle) + len(r.busy)
	r.mu.Unlock()
	return count
}

func (r *instanceState) counts() (idle, busy int) {
	r.mu.Lock()
	idle = len(r.idle)
	busy = len(r.busy)
	r.mu.Unlock()
	return
}

func (r *instanceState) addIdle(name, instanceID string) {
	r.mu.Lock()
	r.idle[name] = instanceID
	r.mu.Unlock()
}

func (r *instanceState) idleSnapshot() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]string, len(r.idle))
	for name, instanceID := range r.idle {
		result[name] = instanceID
	}
	return result
}

func (r *instanceState) removeIdle(name, instanceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if got, ok := r.idle[name]; ok && got == instanceID {
		delete(r.idle, name)
		return true
	}
	return false
}

func (r *instanceState) contains(name, instanceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if got, ok := r.idle[name]; ok && got == instanceID {
		return true
	}
	got, ok := r.busy[name]
	return ok && got == instanceID
}

func (r *instanceState) markBusy(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	instanceID, ok := r.idle[name]
	if !ok {
		return false
	}
	delete(r.idle, name)
	r.busy[name] = instanceID
	return true
}

func (r *instanceState) markDone(name string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if instanceID, ok := r.busy[name]; ok {
		delete(r.busy, name)
		return instanceID, true
	}
	if instanceID, ok := r.idle[name]; ok {
		delete(r.idle, name)
		return instanceID, true
	}
	return "", false
}

func (r *instanceState) markDead(name, instanceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if got, ok := r.idle[name]; ok && got == instanceID {
		delete(r.idle, name)
		return true
	}
	if got, ok := r.busy[name]; ok && got == instanceID {
		delete(r.busy, name)
		return true
	}
	return false
}
