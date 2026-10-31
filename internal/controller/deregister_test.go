package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/actions/scaleset"
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

func TestJobCompletingDuringDeregistrationLetsRemovalProceed(t *testing.T) {
	rs := newInstanceState()
	rs.addIdle("runner-1", "res-1")
	rs.markReady("runner-1", "res-1", 1)
	rs.idleForRemoval()

	if !rs.markBusy("runner-1", jobRef{id: "job-1"}) {
		t.Fatal("JobStarted during removal should record the job")
	}
	if !rs.completeUnassignedRemoval("runner-1", "succeeded") {
		t.Fatal("JobCompleted during removal should be recorded")
	}
	if _, needed := rs.deregistration("runner-1", "res-1"); needed {
		t.Fatal("a runner whose job completed needs no deregistration")
	}
	if rs.restoreBusy("runner-1", "res-1") {
		t.Fatal("a runner whose job completed must not be restored to busy")
	}
}
