package metrics

import (
	"sync"
	"testing"

	"github.com/actions/scaleset"
)

func TestRecorderJobCounters(t *testing.T) {
	r := &Recorder{}

	r.RecordJobStarted(&scaleset.JobStarted{})
	r.RecordJobStarted(&scaleset.JobStarted{})
	r.RecordJobCompleted(&scaleset.JobCompleted{})

	snap := r.Snapshot()
	if snap.JobsStarted != 2 {
		t.Errorf("JobsStarted = %d, want 2", snap.JobsStarted)
	}
	if snap.JobsCompleted != 1 {
		t.Errorf("JobsCompleted = %d, want 1", snap.JobsCompleted)
	}
}

func TestRecorderDesiredRunners(t *testing.T) {
	r := &Recorder{}

	r.RecordDesiredRunners(5)
	snap := r.Snapshot()
	if snap.DesiredRunners != 5 {
		t.Errorf("DesiredRunners = %d, want 5", snap.DesiredRunners)
	}

	r.RecordDesiredRunners(3)
	snap = r.Snapshot()
	if snap.DesiredRunners != 3 {
		t.Errorf("DesiredRunners = %d, want 3", snap.DesiredRunners)
	}
}

func TestRecorderStatistics(t *testing.T) {
	r := &Recorder{}

	stats := &scaleset.RunnerScaleSetStatistic{
		TotalAvailableJobs: 10,
		TotalAssignedJobs:  3,
		TotalRunningJobs:   2,
	}
	r.RecordStatistics(stats)

	snap := r.Snapshot()
	if snap.Statistics == nil {
		t.Fatal("Statistics is nil")
	}
	if snap.Statistics.TotalAvailableJobs != 10 {
		t.Errorf("TotalAvailableJobs = %d, want 10", snap.Statistics.TotalAvailableJobs)
	}
	if snap.Statistics.TotalAssignedJobs != 3 {
		t.Errorf("TotalAssignedJobs = %d, want 3", snap.Statistics.TotalAssignedJobs)
	}
}

func TestRecorderSnapshotTimestamp(t *testing.T) {
	r := &Recorder{}

	snap := r.Snapshot()
	if !snap.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be zero before any recording")
	}

	r.RecordJobStarted(&scaleset.JobStarted{})
	snap = r.Snapshot()
	if snap.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set after recording")
	}
}

func TestRecorderConcurrentAccess(t *testing.T) {
	r := &Recorder{}
	var wg sync.WaitGroup

	for range 100 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			r.RecordJobStarted(&scaleset.JobStarted{})
		}()
		go func() {
			defer wg.Done()
			r.RecordJobCompleted(&scaleset.JobCompleted{})
		}()
		go func() {
			defer wg.Done()
			r.Snapshot()
		}()
	}
	wg.Wait()

	snap := r.Snapshot()
	if snap.JobsStarted != 100 {
		t.Errorf("JobsStarted = %d, want 100", snap.JobsStarted)
	}
	if snap.JobsCompleted != 100 {
		t.Errorf("JobsCompleted = %d, want 100", snap.JobsCompleted)
	}
}
