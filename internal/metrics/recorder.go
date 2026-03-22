package metrics

import (
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// Compile-time check that Recorder implements listener.MetricsRecorder.
var _ listener.MetricsRecorder = (*Recorder)(nil)

// Snapshot holds a point-in-time copy of the metrics for external consumption.
type Snapshot struct {
	// Latest statistics from the scaleset service.
	Statistics *scaleset.RunnerScaleSetStatistic

	// Cumulative counters since process start.
	JobsStarted   int64
	JobsCompleted int64

	// Current desired runner count as reported by the listener.
	DesiredRunners int

	// Timestamp of the last update.
	UpdatedAt time.Time
}

// Recorder collects metrics from the scaleset listener.
// All methods are safe for concurrent use.
type Recorder struct {
	mu             sync.RWMutex
	statistics     *scaleset.RunnerScaleSetStatistic
	jobsStarted    int64
	jobsCompleted  int64
	desiredRunners int
	updatedAt      time.Time
}

func (r *Recorder) RecordStatistics(statistics *scaleset.RunnerScaleSetStatistic) {
	r.mu.Lock()
	r.statistics = statistics
	r.updatedAt = time.Now()
	r.mu.Unlock()
}

func (r *Recorder) RecordJobStarted(msg *scaleset.JobStarted) {
	r.mu.Lock()
	r.jobsStarted++
	r.updatedAt = time.Now()
	r.mu.Unlock()
}

func (r *Recorder) RecordJobCompleted(msg *scaleset.JobCompleted) {
	r.mu.Lock()
	r.jobsCompleted++
	r.updatedAt = time.Now()
	r.mu.Unlock()
}

func (r *Recorder) RecordDesiredRunners(count int) {
	r.mu.Lock()
	r.desiredRunners = count
	r.updatedAt = time.Now()
	r.mu.Unlock()
}

// Snapshot returns a point-in-time copy of the current metrics.
func (r *Recorder) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return Snapshot{
		Statistics:     r.statistics,
		JobsStarted:   r.jobsStarted,
		JobsCompleted: r.jobsCompleted,
		DesiredRunners: r.desiredRunners,
		UpdatedAt:      r.updatedAt,
	}
}
