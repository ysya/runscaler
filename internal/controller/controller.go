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

	"github.com/ysya/runscaler/internal/capacity"
	"github.com/ysya/runscaler/internal/provider"
)

// ScaleSetClient abstracts the GitHub scale-set client methods used by the
// controller.
type ScaleSetClient interface {
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
	instances       instanceState
	scaleSetID      int
	provider        provider.InstanceProvider
	scalesetClient  ScaleSetClient
	minRunners      int
	maxRunners      int
	logger          *slog.Logger
	desiredMu       sync.Mutex
	desiredRunners  int
	desiredRevision uint64
	// pendingCompletions blocks scale-up between JobCompleted and the desired
	// count callback that follows it in the same listener message. Without the
	// barrier, fast cleanup could replace a runner using stale demand.
	pendingCompletions int
	// demandStale is set when a runner exits without a GitHub completion
	// message. Replacing it from the previous desired count can create a
	// permanent surplus runner if a delayed completion belongs to the dead
	// runner, so scaling resumes only after the listener refreshes demand.
	demandStale bool
	diskChecker DiskChecker
	capacity    *capacity.Allocator
	reconcile   chan struct{}
	workCtx     context.Context
	cancelWork  context.CancelFunc
	workerWG    sync.WaitGroup
	cleanupWG   sync.WaitGroup
	stopOnce    sync.Once
	draining    atomic.Bool
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

// WithCapacityAllocator shares a process-wide runner limit with other scale
// set controllers. The manager registers every scale set before starting any
// controller so minimum reservations are effective from the beginning.
func WithCapacityAllocator(a *capacity.Allocator) Option {
	return func(s *ScaleSetController) {
		if a != nil {
			s.capacity = a
		}
	}
}

// NewScaleSetController creates a controller for one GitHub runner scale set.
// See WithDiskChecker for the optional pre-start capacity check.
func NewScaleSetController(scaleSetID, minRunners, maxRunners int, p provider.InstanceProvider, client ScaleSetClient, logger *slog.Logger, opts ...Option) *ScaleSetController {
	privateBroker := capacity.NewBroker(maxRunners)
	s := &ScaleSetController{
		scaleSetID:     scaleSetID,
		provider:       p,
		scalesetClient: client,
		minRunners:     minRunners,
		maxRunners:     maxRunners,
		logger:         logger,
		instances:      newInstanceState(),
		capacity:       privateBroker.Register("standalone", minRunners),
		reconcile:      make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.workCtx, s.cancelWork = context.WithCancel(context.Background())
	s.workerWG.Add(1)
	go s.reconcileLoop()
	return s
}

// HandleDesiredRunnerCount records the latest demand and wakes the background
// reconciler. Provider and GitHub API I/O never blocks the listener callback.
// Completed runners are removed directly; fresh lower demand also converges
// surplus idle runners without preempting busy jobs.
func (s *ScaleSetController) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	if s.draining.Load() {
		return s.instances.count(), nil
	}
	if err := ctx.Err(); err != nil {
		return s.instances.count(), err
	}
	s.desiredMu.Lock()
	s.desiredRunners = count
	s.desiredRevision++
	s.pendingCompletions = 0
	s.demandStale = false
	s.desiredMu.Unlock()
	s.signalReconcile()
	// The listener records this value as desired-runners. Reconciliation is
	// asynchronous, so report the bounded target rather than a timing-dependent
	// snapshot of instances that happen to be ready at callback return.
	return s.targetRunnerCount(), nil
}

// HandleJobStarted marks a runner as busy when a job is assigned.
func (s *ScaleSetController) HandleJobStarted(ctx context.Context, jobInfo *scaleset.JobStarted) error {
	s.logger.Debug(
		"Job started",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
		slog.String("runnerName", jobInfo.RunnerName),
	)
	marked := s.instances.markBusy(jobInfo.RunnerName)
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

	_, ready, ok := s.instances.markDone(jobInfo.RunnerName)
	if !ok {
		s.logger.Warn("Job completed for unknown runner (already removed?)", slog.String("runnerName", jobInfo.RunnerName))
		return nil
	}
	s.desiredMu.Lock()
	s.pendingCompletions++
	s.desiredMu.Unlock()
	if !ready {
		// The provider has started the runner but has not returned its instance
		// ID yet. startInstance observes the removing phase and completes cleanup.
		return nil
	}
	// Provider cleanup belongs to the background reconciler. The scaleset
	// listener deletes a message before invoking this callback, so blocking or
	// returning a cleanup error cannot improve delivery semantics and only
	// delays later job messages.
	s.signalReconcile()
	return nil
}

func (s *ScaleSetController) reconcileLoop() {
	defer s.workerWG.Done()
	for {
		select {
		case <-s.workCtx.Done():
			return
		case <-s.reconcile:
		}

		for s.reconcileOne() {
		}
	}
}

// reconcileOne claims at most one cleanup or starts at most one runner.
// Returning true asks the loop to immediately re-evaluate work; false means it
// should sleep until signalled.
func (s *ScaleSetController) reconcileOne() bool {
	if s.draining.Load() || s.workCtx.Err() != nil {
		return false
	}
	if name, instanceID, ok := s.instances.claimRemoval(time.Now()); ok {
		s.cleanupWG.Add(1)
		go s.removeClaimedInstance(name, instanceID)
		return true
	}
	target, canScale := s.scaleTarget()
	if !canScale {
		return false
	}
	current := s.instances.count()
	if current > target && s.instances.markIdleAboveTarget(target) {
		return true
	}
	if current >= target {
		return false
	}

	s.logger.Info("Scaling up runner",
		slog.Int("currentCount", current),
		slog.Int("desiredCount", target),
	)
	lease, err := s.capacity.Acquire(s.workCtx)
	if err != nil {
		return false
	}
	// Demand may have changed while this scale set waited for host capacity.
	target, canScale = s.scaleTarget()
	if s.draining.Load() || !canScale || s.instances.count() >= target {
		lease.Release()
		return false
	}
	if _, err := s.startInstanceWithLease(s.workCtx, lease); err != nil {
		if s.workCtx.Err() == nil && !s.draining.Load() {
			s.logger.Error("Failed to start runner, will retry",
				slog.Any("error", err),
			)
			time.AfterFunc(5*time.Second, s.signalReconcile)
		}
		return false
	}
	return true
}

func (s *ScaleSetController) targetRunnerCount() int {
	s.desiredMu.Lock()
	desired := s.desiredRunners
	s.desiredMu.Unlock()
	return min(s.maxRunners, s.minRunners+desired)
}

func (s *ScaleSetController) scaleTarget() (target int, canScale bool) {
	s.desiredMu.Lock()
	desired := s.desiredRunners
	canScale = s.pendingCompletions == 0 && !s.demandStale
	s.desiredMu.Unlock()
	return min(s.maxRunners, s.minRunners+desired), canScale
}

func (s *ScaleSetController) removeClaimedInstance(name, instanceID string) {
	defer s.cleanupWG.Done()
	removeCtx, cancel := context.WithTimeout(s.workCtx, 30*time.Second)
	err := s.provider.RemoveInstance(removeCtx, instanceID)
	cancel()
	if err == nil {
		s.finishRemoval(name, instanceID)
		return
	}

	delay, tracked := s.instances.retryRemoval(name, instanceID, time.Now())
	if !tracked || s.workCtx.Err() != nil {
		return
	}
	s.logger.Error("Failed to remove runner, will retry",
		slog.String("name", name),
		slog.Duration("retryIn", delay),
		slog.Any("error", err),
	)
	time.AfterFunc(delay, s.signalReconcile)
}

func (s *ScaleSetController) signalReconcile() {
	if s.draining.Load() {
		return
	}
	select {
	case s.reconcile <- struct{}{}:
	default:
	}
}

// startInstance creates and starts a new ephemeral runner.
func (s *ScaleSetController) startInstance(ctx context.Context) (string, error) {
	lease, err := s.capacity.Acquire(ctx)
	if err != nil {
		return "", err
	}
	return s.startInstanceWithLease(ctx, lease)
}

func (s *ScaleSetController) startInstanceWithLease(ctx context.Context, lease *capacity.Lease) (string, error) {
	name := fmt.Sprintf("runner-%s", uuid.NewString()[:8])
	if !s.instances.reserve(name, lease) {
		lease.Release()
		return "", fmt.Errorf("runner name collision: %s", name)
	}
	fail := func(err error) (string, error) {
		s.instances.failProvision(name)
		lease.Release()
		return "", err
	}

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
		return fail(fmt.Errorf("failed to generate JIT config: %w", err))
	}

	instanceID, err := s.provider.StartInstance(ctx, name, jit.EncodedJITConfig)
	if err != nil {
		return fail(err)
	}

	removeNow, tracked := s.instances.markReady(name, instanceID)
	if !tracked {
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = s.provider.RemoveInstance(cleanCtx, instanceID)
		cancel()
		lease.Release()
		return "", fmt.Errorf("runner %s was no longer tracked after provider start", name)
	}
	if removeNow && !s.draining.Load() {
		// The background worker will observe the removing phase on its next
		// reconcile pass and perform provider cleanup.
		s.signalReconcile()
		return name, nil
	}
	if s.draining.Load() {
		if !removeNow {
			_, _, _ = s.instances.markDone(name)
		}
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := s.provider.RemoveInstance(cleanCtx, instanceID)
		cancel()
		if err != nil {
			return "", fmt.Errorf("remove runner started during shutdown: %w", err)
		}
		s.finishRemoval(name, instanceID)
		return name, nil
	}
	if watcher, ok := s.provider.(provider.InstanceWatcher); ok {
		go s.watchInstance(s.workCtx, watcher, name, instanceID)
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
//   - It must stay small next to a reconcile burst that may perform the check
//     once per new runner. Reconciliation is off the listener path, but an
//     excessive bound would still delay usable capacity for queued jobs.
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
	s.desiredMu.Lock()
	crashRevision := s.desiredRevision
	s.desiredMu.Unlock()
	lease, removed := s.instances.markDead(name, instanceID)
	if !removed {
		return // normal JobCompleted path already removed it from state
	}
	s.desiredMu.Lock()
	// Do not overwrite a desired callback that arrived after the crash was
	// observed. Otherwise establish the barrier before releasing capacity, so
	// a controller waiting in Acquire cannot wake and scale from stale demand.
	if s.desiredRevision == crashRevision {
		s.demandStale = true
	}
	s.desiredMu.Unlock()
	lease.Release()
	s.logger.Warn("Runner exited before job completion; removing stale capacity", slog.String("name", name))
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	if err := s.provider.RemoveInstance(cleanCtx, instanceID); err != nil {
		s.logger.Warn("Failed to clean up exited runner", slog.String("name", name), slog.Any("error", err))
	}
	cancel()

	// Wake cleanup/accounting, but wait for a fresh desired-count callback
	// before replacing the runner. The previous desired value may still include
	// the job that was running on this now-dead runner.
	s.signalReconcile()
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
	s.stopWork()
	started := time.Now()

	// Let cancellation unwind a capacity wait or provider start before taking
	// the idle snapshot. The state transition to removing is atomic with
	// HandleJobStarted's idle→busy transition, preserving the drain boundary.
	if err := s.waitWorker(ctx); err != nil {
		return err
	}
	for name, instanceID := range s.instances.idleForRemoval() {
		if err := s.provider.RemoveInstance(ctx, instanceID); err != nil {
			s.logger.Warn("Failed to remove idle runner during drain",
				slog.String("name", name),
				slog.Any("error", err))
			continue
		}
		s.finishRemoval(name, instanceID)
	}

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
	s.draining.Store(true)
	s.stopWork()

	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_ = s.waitWorker(shutdownCtx)

	var wg sync.WaitGroup
	removeRunner := func(name, instanceID string, lease *capacity.Lease) {
		if instanceID == "" {
			lease.Release()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer lease.Release()
			s.logger.Debug("Removing runner", slog.String("name", name))
			if err := s.provider.RemoveInstance(shutdownCtx, instanceID); err != nil {
				s.logger.Error("Failed to remove runner", slog.String("name", name), slog.Any("error", err))
			}
		}()
	}

	for name, instance := range s.instances.detachAll() {
		removeRunner(name, instance.id, instance.lease)
	}

	wg.Wait()
	s.provider.Shutdown(shutdownCtx)
}

func (s *ScaleSetController) stopWork() {
	s.stopOnce.Do(s.cancelWork)
}

func (s *ScaleSetController) waitWorker(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.workerWG.Wait()
		// Once the worker has stopped, no new cleanup goroutines can be added.
		s.cleanupWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *ScaleSetController) finishRemoval(name, instanceID string) {
	lease, ok := s.instances.finishRemoval(name, instanceID)
	if !ok {
		return
	}
	lease.Release()
	s.signalReconcile()
}

// InstanceCounts returns the number of idle and busy runners.
func (s *ScaleSetController) InstanceCounts() (idle, busy int) {
	return s.instances.counts()
}

// InstanceLifecycleCounts exposes the complete controller state machine for
// health reporting without changing the older idle/busy counter contract.
func (s *ScaleSetController) InstanceLifecycleCounts() (provisioning, idle, busy, removing int) {
	return s.instances.lifecycleCounts()
}
