// Package regsweep removes leaked JIT runner registrations of one scale set
// from GitHub.
//
// A start that fails after GenerateJitRunnerConfig (the provider cannot boot
// the instance, a clone floor refuses it, the runner never becomes ready)
// leaves a registration that never connects. GitHub keeps such a
// registration offline for about a day before it removes it. The controller
// cannot deregister it on the failure path because it may not know the
// runner ID, and a process that died leaves its registrations behind too.
//
// A sweep lists the runner group of the scale set through the GitHub REST
// API and removes a runner only when every one of these holds:
//
//   - GitHub reports it offline and not busy;
//   - its name has the exact form runner names get (provider.NewRunnerName);
//   - the controller has no local instance of that name in any phase, read
//     after the list returned;
//   - the scale set API reports that the runner belongs to this scale set.
//
// SAFETY: the sweep assumes that exactly one runscaler process serves each
// scale set. A second process that serves the same scale set has local
// instances this process cannot see, and the sweep would remove its
// registrations while their instances boot.
package regsweep

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

// Mode selects what a sweep does with a confirmed leaked registration.
type Mode string

const (
	// ModeLog reports each leaked registration and removes nothing.
	ModeLog Mode = "log"
	// ModeRemove removes each leaked registration from GitHub.
	ModeRemove Mode = "remove"
)

// Bounds that apply when Config leaves them zero.
const (
	DefaultTimeout     = 2 * time.Minute
	DefaultTriggerGap  = time.Minute
	DefaultMaxRemovals = 20
	DefaultMaxLookups  = 100
)

// runnerName is the exact form of provider.NewRunnerName: "runner-" and the
// first 8 characters of a random UUID, which are lowercase hex.
var runnerName = regexp.MustCompile(`^runner-[0-9a-f]{8}$`)

// Runner is one entry of the GitHub REST runner list.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// Lister lists every runner of the scale set's runner group.
type Lister interface {
	ListRunners(ctx context.Context) ([]Runner, error)
}

// ScaleSetAPI is the part of the scale set client a sweep uses to confirm
// and remove a registration.
type ScaleSetAPI interface {
	GetRunner(ctx context.Context, runnerID int) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// LocalRunners reports whether the controller tracks a runner name in any
// phase. It must read the controller state when it is called.
type LocalRunners interface {
	HasRunner(name string) bool
}

// LocalRunnersFunc adapts a function to LocalRunners.
type LocalRunnersFunc func(name string) bool

// HasRunner calls f.
func (f LocalRunnersFunc) HasRunner(name string) bool { return f(name) }

// Config configures a Sweeper.
type Config struct {
	Mode       Mode
	ScaleSetID int
	// Interval is the period between timer sweeps.
	Interval time.Duration
	// TriggerGap is the shortest time between the start of the previous
	// sweep and a sweep that Trigger requests.
	TriggerGap time.Duration
	// Timeout bounds one sweep, the list and every lookup and removal.
	Timeout time.Duration
	// MaxRemovals bounds the registrations one sweep removes (or, in log
	// mode, reports), so a fault cannot clear a runner group in one pass.
	MaxRemovals int
	// MaxLookups bounds the GetRunner calls of one sweep.
	MaxLookups int
}

// Result counts what one sweep did.
type Result struct {
	Listed     int // runners in the group list
	Candidates int // confirmed leaked registrations of this scale set
	Removed    int
	Gone       int // removed by someone else between list and action
	Deferred   int // left for a later sweep by MaxRemovals or MaxLookups
	Errors     int
	ListErr    error
}

// Sweeper runs sweeps for one scale set.
type Sweeper struct {
	cfg     Config
	lister  Lister
	api     ScaleSetAPI
	local   LocalRunners
	logger  *slog.Logger
	trigger chan struct{}

	// resumeAfter is the runner ID after which the next sweep starts, set
	// when a bound cut the previous sweep short. Runners that stay in the
	// list (another scale set's, or ones whose removal GitHub refuses) would
	// otherwise use up the bounds of every sweep and starve later ones.
	mu          sync.Mutex
	resumeAfter int64
}

// New returns a Sweeper. It refuses a configuration that could remove
// registrations of another scale set.
func New(cfg Config, lister Lister, api ScaleSetAPI, local LocalRunners, logger *slog.Logger) (*Sweeper, error) {
	if cfg.Mode != ModeLog && cfg.Mode != ModeRemove {
		return nil, fmt.Errorf("unsupported sweep mode %q", cfg.Mode)
	}
	if cfg.ScaleSetID <= 0 {
		return nil, fmt.Errorf("sweep needs the scale set ID, got %d", cfg.ScaleSetID)
	}
	if cfg.Interval <= 0 {
		return nil, fmt.Errorf("sweep interval must be greater than zero")
	}
	if lister == nil || api == nil || local == nil || logger == nil {
		return nil, errors.New("sweep needs a lister, a scale set API, local runner state and a logger")
	}
	if cfg.TriggerGap <= 0 {
		cfg.TriggerGap = DefaultTriggerGap
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxRemovals <= 0 {
		cfg.MaxRemovals = DefaultMaxRemovals
	}
	if cfg.MaxLookups <= 0 {
		cfg.MaxLookups = DefaultMaxLookups
	}
	return &Sweeper{cfg: cfg, lister: lister, api: api, local: local, logger: logger, trigger: make(chan struct{}, 1)}, nil
}

// Trigger asks for one sweep soon, for example after a start failure. It
// never blocks and may be called before Run starts. Requests that arrive
// before the requested sweep starts coalesce into it.
func (s *Sweeper) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Run sweeps once at once, then every Interval, plus once after a Trigger,
// no sooner than TriggerGap after the start of the previous sweep. It
// returns when ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	lastStart := time.Now()
	s.Sweep(ctx)
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	var delayed *time.Timer
	var delayedC <-chan time.Time
	stopDelayed := func() {
		if delayed != nil {
			delayed.Stop()
			delayed, delayedC = nil, nil
		}
	}
	defer stopDelayed()
	sweep := func() {
		// A sweep that starts now also covers a requested one.
		stopDelayed()
		lastStart = time.Now()
		s.Sweep(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		case <-delayedC:
			delayed, delayedC = nil, nil
			sweep()
		case <-s.trigger:
			if delayed != nil {
				continue // already requested
			}
			wait := s.cfg.TriggerGap - time.Since(lastStart)
			if wait <= 0 {
				sweep()
				continue
			}
			delayed = time.NewTimer(wait)
			delayedC = delayed.C
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// Sweep lists the runner group once and acts on each confirmed leaked
// registration. It logs one line per candidate and one summary line. When a
// bound (lookups, removals or the timeout) cuts it short, the next sweep
// starts after the last runner it looked up.
func (s *Sweeper) Sweep(parent context.Context) Result {
	ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
	defer cancel()
	var res Result
	runners, err := s.lister.ListRunners(ctx)
	res.Listed = len(runners)
	if err != nil {
		res.ListErr = err
		res.Errors++
		s.summary(res)
		return res
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lookups := 0
	var lastLookup int64
	timedOut := false
	for _, r := range s.rotate(runners) {
		if ctx.Err() != nil {
			res.Errors++
			timedOut = true
			break
		}
		if r.Status != "offline" || r.Busy || !runnerName.MatchString(r.Name) {
			continue
		}
		// Read local state now, after the list returned, never from a
		// snapshot taken before it. The controller reserves a name before it
		// asks GitHub for the registration, so a listed registration of this
		// process is local until the controller has dealt with it: it can
		// leave local state after the list, never join it.
		if s.local.HasRunner(r.Name) {
			continue
		}
		if res.Candidates >= s.cfg.MaxRemovals || lookups >= s.cfg.MaxLookups {
			res.Deferred++
			continue
		}
		lookups++
		lastLookup = r.ID
		ref, err := s.api.GetRunner(ctx, int(r.ID))
		if isNotFound(err) {
			res.Gone++
			continue
		}
		if err != nil {
			res.Errors++
			s.logger.Warn("Registration sweep could not look up runner", slog.Int64("runnerID", r.ID), slog.String("name", r.Name), slog.Any("error", err))
			continue
		}
		if ref == nil || ref.RunnerScaleSetID != s.cfg.ScaleSetID {
			seen := 0
			if ref != nil {
				seen = ref.RunnerScaleSetID
			}
			s.logger.Info("Registration sweep keeps runner of another scale set",
				slog.Int64("runnerID", r.ID), slog.String("name", r.Name), slog.Int("runnerScaleSetID", seen), slog.Int("scaleSetID", s.cfg.ScaleSetID))
			continue
		}
		res.Candidates++
		if s.cfg.Mode == ModeLog {
			s.logger.Info("Leaked runner registration (sweep-mode log, not removed)", slog.Int64("runnerID", r.ID), slog.String("name", r.Name))
			continue
		}
		err = s.api.RemoveRunner(ctx, r.ID)
		switch {
		case err == nil:
			res.Removed++
			s.logger.Info("Removed leaked runner registration", slog.Int64("runnerID", r.ID), slog.String("name", r.Name))
		case isNotFound(err):
			res.Gone++
		default:
			// For example a job holds the runner. Leave it for the next sweep.
			res.Errors++
			s.logger.Warn("Registration sweep could not remove runner; leaving it for the next sweep", slog.Int64("runnerID", r.ID), slog.String("name", r.Name), slog.Any("error", err))
		}
	}
	// Any bound that cut the sweep short (lookups, removals or the timeout)
	// moves the next start past the runners this sweep looked up.
	if res.Deferred > 0 || timedOut {
		s.resumeAfter = lastLookup
	} else {
		s.resumeAfter = 0
	}
	s.summary(res)
	return res
}

// rotate orders runners by ID, starting after resumeAfter and wrapping
// around, so every runner is reached within a bounded number of sweeps.
func (s *Sweeper) rotate(runners []Runner) []Runner {
	sorted := slices.Clone(runners)
	slices.SortFunc(sorted, func(a, b Runner) int { return cmp.Compare(a.ID, b.ID) })
	start, _ := slices.BinarySearchFunc(sorted, s.resumeAfter+1, func(r Runner, id int64) int { return cmp.Compare(r.ID, id) })
	return append(sorted[start:], sorted[:start]...)
}

func (s *Sweeper) summary(res Result) {
	attrs := []any{
		slog.String("mode", string(s.cfg.Mode)),
		slog.Int("listed", res.Listed),
		slog.Int("candidates", res.Candidates),
		slog.Int("removed", res.Removed),
		slog.Int("gone", res.Gone),
		slog.Int("deferred", res.Deferred),
		slog.Int("errors", res.Errors),
	}
	if res.ListErr != nil {
		attrs = append(attrs, slog.Any("listError", res.ListErr))
		s.logger.Warn("Registration sweep could not list runners", attrs...)
		return
	}
	s.logger.Info("Registration sweep finished", attrs...)
}

func isNotFound(err error) bool {
	return err != nil && (errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError))
}
