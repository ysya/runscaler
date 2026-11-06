package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/actions/scaleset"

	"github.com/ysya/runscaler/internal/capacity"
)

// GitHub assigns a job to a runner before the listener delivers JobStarted,
// sometimes minutes before. These tests cover runners that look idle locally
// but hold a job at GitHub, which refuses to deregister them.

var errJobStillRunning = fmt.Errorf("request DELETE failed: %w: runner busy", scaleset.JobStillRunningError)

// addRegisteredIdle adds an idle runner that knows its GitHub runner ID, as a
// runner started by startInstance does.
func addRegisteredIdle(s *ScaleSetController, name, instanceID string, runnerID int) {
	s.instances.addIdle(name, instanceID)
	s.instances.markReady(name, instanceID, runnerID)
}

func TestDrainKeepsRunnerGitHubAssignedBeforeJobStarted(t *testing.T) {
	mb := &mockProvider{}
	ms := &mockScaleset{removeErr: map[int]error{1: errJobStillRunning}}
	s := NewScaleSetController(1, 0, 5, mb, ms, slog.New(slog.DiscardHandler))
	addRegisteredIdle(s, "runner-assigned", "res-assigned", 1)
	addRegisteredIdle(s, "runner-idle", "res-idle", 2)

	done := make(chan error, 1)
	go func() { done <- s.Drain(context.Background()) }()

	waitFor(t, func() bool { return mb.removedCount() == 1 }, "unassigned idle runner was not removed")
	if containsString(mb.removed, "res-assigned") {
		t.Fatal("drain destroyed a runner GitHub had assigned a job to")
	}
	if phase, _ := s.instances.phase("runner-assigned"); phase != instanceBusy {
		t.Fatalf("assigned runner phase = %v, want busy", phase)
	}
	if got := ms.deregisteredIDs(); !slices.Contains(got, 1) || !slices.Contains(got, 2) {
		t.Fatalf("deregistered = %v, want both runners asked", got)
	}

	// The late JobStarted attaches the job, and its completion ends the drain.
	ctx := context.Background()
	if err := s.HandleJobStarted(ctx, &scaleset.JobStarted{
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-1"},
		RunnerName:     "runner-assigned",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleJobCompleted(ctx, &scaleset.JobCompleted{
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-1"},
		RunnerName:     "runner-assigned",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not finish after the kept runner's job completed")
	}
}

func TestScaleDownKeepsRunnerGitHubAssignedBeforeJobStarted(t *testing.T) {
	s, mb, ms := newTestController(t, 0, 10)
	ms.removeErr = map[int]error{1: errJobStillRunning}
	addRegisteredIdle(s, "runner-assigned", "res-assigned", 1)
	addRegisteredIdle(s, "runner-idle", "res-idle", 2)

	if _, err := s.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return mb.removedCount() == 1 }, "unassigned idle runner was not scaled down")
	waitFor(t, func() bool {
		phase, ok := s.instances.phase("runner-assigned")
		return ok && phase == instanceBusy
	}, "assigned runner was not kept as busy")
	if containsString(mb.removed, "res-assigned") {
		t.Fatal("scale-down destroyed a runner GitHub had assigned a job to")
	}
}

func TestScaleDownProceedsWhenRunnerAlreadyGone(t *testing.T) {
	s, mb, ms := newTestController(t, 0, 10)
	ms.removeErr = map[int]error{1: fmt.Errorf("request DELETE failed: %w", scaleset.RunnerNotFoundError)}
	addRegisteredIdle(s, "runner-gone", "res-gone", 1)

	if _, err := s.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return mb.removedCount() == 1 }, "runner GitHub no longer knows was not removed")
}

func TestUnconfirmedIdleRemovalKeepsLocalCapacity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryError error
		wantStarts int
	}{
		{name: "assigned runner stays", retryError: errJobStillRunning, wantStarts: 1},
		{name: "deregistered runner is replaced", wantStarts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				broker := capacity.NewBroker(2)
				mb := &mockProvider{}
				ms := &mockScaleset{
					removeErr:     map[int]error{1: tc.retryError},
					removeErrOnce: map[int]error{1: errors.New("broker unavailable")},
				}
				s := NewScaleSetController(1, 0, 1, mb, ms, slog.New(slog.DiscardHandler),
					WithCapacityAllocator(broker.Register("test", 0)))
				t.Cleanup(func() { s.Shutdown(context.Background()) })
				ctx := context.Background()
				if _, err := s.startInstance(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := s.HandleDesiredRunnerCount(ctx, 0); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if got := ms.deregisteredIDs(); !slices.Equal(got, []int{1}) {
					t.Fatalf("deregistered = %v, want [1] before retry", got)
				}

				// Host capacity is available, but this runner can still be restored
				// to busy, so fresh demand must not exceed the local max of one.
				if _, err := s.HandleDesiredRunnerCount(ctx, 1); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if got := mb.startedCount(); got != 1 {
					t.Fatalf("started %d runners before deregistration was confirmed, want 1", got)
				}

				time.Sleep(5 * time.Second)
				synctest.Wait()
				if got := mb.startedCount(); got != tc.wantStarts {
					t.Fatalf("starts after retry = %d, want %d", got, tc.wantStarts)
				}
				if got := broker.InUse(); got != 1 {
					t.Fatalf("leased capacity after retry = %d, want 1", got)
				}
			})
		})
	}
}

func TestScaleDownRetriesWhenDeregistrationFails(t *testing.T) {
	s, mb, ms := newTestController(t, 0, 10)
	ms.removeErr = map[int]error{1: errors.New("broker unavailable")}
	addRegisteredIdle(s, "runner-1", "res-1", 1)

	if _, err := s.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(ms.deregisteredIDs()) == 1 }, "deregistration was not attempted")
	time.Sleep(50 * time.Millisecond)
	if mb.removedCount() != 0 {
		t.Fatal("instance destroyed although GitHub did not confirm deregistration")
	}
	if phase, _ := s.instances.phase("runner-1"); phase != instanceRemoving {
		t.Fatalf("phase = %v, want removing pending retry", phase)
	}
}

func TestCompletedJobRemovalSkipsDeregistration(t *testing.T) {
	s, mb, ms := newTestController(t, 0, 10)
	addRegisteredIdle(s, "runner-1", "res-1", 1)
	s.instances.markBusy("runner-1", jobRef{id: "job-1"})

	if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-1"},
		RunnerName:     "runner-1",
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return mb.removedCount() == 1 }, "completed runner was not removed")
	if got := ms.deregisteredIDs(); len(got) != 0 {
		t.Fatalf("deregistered = %v, want none: an ephemeral runner leaves GitHub when its job ends", got)
	}
}

func TestDrainWaitsWhenDeregistrationIsUnconfirmed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, mb, ms := newTestController(t, 0, 10)
		ms.removeErr = map[int]error{1: errors.New("broker unavailable")}
		addRegisteredIdle(s, "runner-assigned", "res-assigned", 1)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		if err := s.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Drain = %v, want deadline exceeded while deregistration is unconfirmed", err)
		}
		if mb.removedCount() != 0 {
			t.Fatal("drain destroyed a runner without confirming deregistration")
		}
	})
}

func TestDrainRetriesDeregistrationBeforeRemovingInstance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, mb, ms := newTestController(t, 0, 10)
		ms.removeErrOnce = map[int]error{1: errors.New("broker unavailable")}
		addRegisteredIdle(s, "runner-1", "res-1", 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if err := s.Drain(ctx); err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if mb.removedCount() != 1 {
			t.Fatal("drain did not remove the runner after deregistration recovered")
		}
		if got := ms.deregisteredIDs(); !slices.Equal(got, []int{1, 1}) {
			t.Fatalf("deregistered = %v, want [1 1]", got)
		}
	})
}

func TestDrainResumesPendingScaleDownRemoval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, mb, ms := newTestController(t, 0, 10)
		addRegisteredIdle(s, "runner-1", "res-1", 1)
		s.instances.markIdleAboveTarget(0)
		name, instanceID, ok := s.instances.claimRemoval(time.Now())
		if !ok {
			t.Fatal("scale-down did not claim a runner")
		}
		// A cleanup cancelled by drain leaves a removing runner awaiting retry.
		if _, tracked := s.instances.retryRemoval(name, instanceID, time.Now()); !tracked {
			t.Fatal("scale-down removal was not tracked for retry")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if err := s.Drain(ctx); err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if mb.removedCount() != 1 {
			t.Fatal("drain skipped a runner already awaiting scale-down cleanup")
		}
		if got := ms.deregisteredIDs(); !slices.Equal(got, []int{1}) {
			t.Fatalf("deregistered = %v, want [1]", got)
		}
	})
}

func TestJobCompletingDuringDeregistrationLetsRemovalProceed(t *testing.T) {
	rs := newInstanceState()
	rs.addIdle("runner-1", "res-1")
	rs.markReady("runner-1", "res-1", 1)
	rs.idleForRemoval()

	if !rs.markBusy("runner-1", jobRef{id: "job-1"}) {
		t.Fatal("JobStarted during removal should record the job")
	}
	if _, ready, ok := rs.markDone("runner-1", reasonJobCompleted, "succeeded"); !ok || !ready {
		t.Fatal("JobCompleted during removal should be recorded")
	}
	if _, needed := rs.deregistration("runner-1", "res-1"); needed {
		t.Fatal("a runner whose job completed needs no deregistration")
	}
	if rs.restoreBusy("runner-1", "res-1") {
		t.Fatal("a runner whose job completed must not be restored to busy")
	}
	if _, _, ok := rs.markDone("runner-1", reasonJobCompleted, "succeeded"); ok {
		t.Fatal("duplicate completion must not be recorded twice")
	}
}
