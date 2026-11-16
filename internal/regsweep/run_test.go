package regsweep

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/actions/scaleset"
)

// countingLister counts list calls; each one is one sweep.
type countingLister struct{ calls atomic.Int32 }

func (c *countingLister) ListRunners(context.Context) ([]Runner, error) {
	c.calls.Add(1)
	return nil, nil
}

func startRun(t *testing.T, interval time.Duration) (*Sweeper, *countingLister, context.CancelFunc) {
	t.Helper()
	l := &countingLister{}
	s, err := New(Config{Mode: ModeRemove, ScaleSetID: 1, Interval: interval}, l, &fakeScaleSet{}, &fakeLocal{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	return s, l, cancel
}

func wantSweeps(t *testing.T, l *countingLister, want int32, when string) {
	t.Helper()
	synctest.Wait()
	if got := l.calls.Load(); got != want {
		t.Fatalf("%s: %d sweeps, want %d", when, got, want)
	}
}

// The timer sweeps at startup and then once per interval, never more often.
func TestRunSweepsAtStartupAndOncePerInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, l, cancel := startRun(t, 10*time.Minute)
		defer cancel()
		wantSweeps(t, l, 1, "at startup")
		time.Sleep(10*time.Minute - time.Second)
		wantSweeps(t, l, 1, "just before the first interval")
		time.Sleep(time.Second)
		wantSweeps(t, l, 2, "at the first interval")
		time.Sleep(25 * time.Minute)
		wantSweeps(t, l, 4, "after 35 minutes")
	})
}

// A start failure triggers one sweep; a burst of triggers coalesces into one
// sweep no sooner than TriggerGap after the previous sweep started.
func TestRunTriggerSweepsOnceAndCoalesces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, l, cancel := startRun(t, 10*time.Minute)
		defer cancel()
		wantSweeps(t, l, 1, "at startup")

		time.Sleep(5 * time.Minute)
		s.Trigger()
		wantSweeps(t, l, 2, "trigger long after the last sweep")

		for range 5 {
			s.Trigger()
			time.Sleep(time.Second)
		}
		wantSweeps(t, l, 2, "burst inside the gap")
		time.Sleep(DefaultTriggerGap - 6*time.Second)
		wantSweeps(t, l, 2, "just before one gap after the triggered sweep")
		time.Sleep(time.Second)
		wantSweeps(t, l, 3, "one gap after the triggered sweep")
		time.Sleep(3*time.Minute + 59*time.Second)
		wantSweeps(t, l, 3, "no repeat without a new trigger")
		time.Sleep(time.Second)
		wantSweeps(t, l, 4, "the interval timer still runs")
	})
}

// Trigger never blocks, also before Run starts and while Run is not reading.
func TestTriggerNeverBlocks(t *testing.T) {
	s, err := New(Config{Mode: ModeLog, ScaleSetID: 1, Interval: time.Minute}, &countingLister{}, &fakeScaleSet{}, &fakeLocal{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		s.Trigger()
	}
}

func TestRunStopsWithContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &countingLister{}
		s, _ := New(Config{Mode: ModeRemove, ScaleSetID: 1, Interval: time.Minute}, l, &fakeScaleSet{}, &fakeLocal{}, slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { s.Run(ctx); close(done) }()
		synctest.Wait()
		cancel()
		<-done
		before := l.calls.Load()
		time.Sleep(time.Hour)
		if l.calls.Load() != before {
			t.Fatal("swept after the context ended")
		}
	})
}

// staticLister returns the same runners on every sweep.
type staticLister []Runner

func (l staticLister) ListRunners(context.Context) ([]Runner, error) { return l, nil }

// slowScaleSet takes 31 s for each lookup of a slow ID, or until the sweep's
// context ends, as a slow GitHub would.
type slowScaleSet struct {
	fakeScaleSet
	slow map[int]bool
}

func (s *slowScaleSet) GetRunner(ctx context.Context, id int) (*scaleset.RunnerReference, error) {
	if s.slow[id] {
		select {
		case <-time.After(31 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.fakeScaleSet.GetRunner(ctx, id)
}

// Slow lookups that use up the sweep timeout before any count bound is
// reached must not starve a later runner: the next sweep starts after them.
func TestSweepReachesLaterRunnerPastTimedOutSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var runners []Runner
		api := &slowScaleSet{slow: map[int]bool{}}
		for id := 1; id <= 4; id++ {
			runners = append(runners, leaked(int64(id)))
			api.slow[id] = true
		}
		api.ours(1, 2, 3, 4)
		for id := 1; id <= 4; id++ {
			api.refs[id] = &scaleset.RunnerReference{ID: id, RunnerScaleSetID: testScaleSetID + 1}
		}
		mine := leaked(5)
		runners = append(runners, mine)
		api.ours(mine.ID)
		s, err := New(Config{Mode: ModeRemove, ScaleSetID: testScaleSetID, Interval: 10 * time.Minute}, staticLister(runners), api, &fakeLocal{}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		first := s.Sweep(context.Background())
		if first.Deferred != 0 || first.Removed != 0 {
			t.Fatalf("first sweep = %+v, want the timeout (not a count bound) to cut it short", first)
		}
		for range 2 {
			s.Sweep(context.Background())
		}
		if got := api.removedIDs(); len(got) != 1 || got[0] != mine.ID {
			t.Fatalf("removed %v within 3 time-limited sweeps, want [%d]", got, mine.ID)
		}
	})
}
