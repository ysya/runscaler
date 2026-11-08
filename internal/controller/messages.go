package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/ysya/runscaler/internal/metrics"
)

// JobAcquirer claims the jobs a message offers. The listener's message session
// client satisfies it.
type JobAcquirer interface {
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
}

// MessageHandler passes one listener session's messages to a controller,
// doing what the SDK listener did itself before v0.5.0. The listener now acks
// a message only after Scale returns nil, so an error leaves the message to
// be delivered again instead of losing it.
//
// Jobs are acquired first, so a failed acquire leaves nothing half handled,
// and acquiring an already acquired job again is a no-op. Job starts and
// completions follow, then the desired runner count, which clears the
// completion barrier they set. An idle poll carries no message; the last
// statistics then refresh the desired count, which the controller needs to
// resume scaling after a runner exits without a completion message.
//
// Scale is not safe for concurrent use; the listener never calls it
// concurrently.
type MessageHandler struct {
	acquirer       JobAcquirer
	controller     *ScaleSetController
	recorder       *metrics.Recorder
	logger         *slog.Logger
	lastStatistics *scaleset.RunnerScaleSetStatistic
}

var _ listener.Scaler = (*MessageHandler)(nil)

// NewMessageHandler returns a handler for one listener session.
func NewMessageHandler(acquirer JobAcquirer, s *ScaleSetController, recorder *metrics.Recorder, logger *slog.Logger) *MessageHandler {
	return &MessageHandler{acquirer: acquirer, controller: s, recorder: recorder, logger: logger}
}

// Scale handles one poll result: a message, or nil for an idle poll.
func (h *MessageHandler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		if h.lastStatistics == nil {
			return nil
		}
		return h.updateDemand(ctx, h.lastStatistics)
	}

	if msg.Statistics != nil {
		h.lastStatistics = msg.Statistics
		h.recorder.RecordStatistics(msg.Statistics)
	}
	if len(msg.JobAvailableMessages) > 0 {
		if err := h.acquire(ctx, msg.JobAvailableMessages); err != nil {
			return err
		}
	}
	for _, job := range msg.JobStartedMessages {
		h.recorder.RecordJobStarted(job)
		if err := h.controller.HandleJobStarted(ctx, job); err != nil {
			return fmt.Errorf("failed to handle job started: %w", err)
		}
	}
	for _, job := range msg.JobCompletedMessages {
		h.recorder.RecordJobCompleted(job)
		if err := h.controller.HandleJobCompleted(ctx, job); err != nil {
			return fmt.Errorf("failed to handle job completed: %w", err)
		}
	}
	if msg.Statistics == nil {
		return nil
	}
	return h.updateDemand(ctx, msg.Statistics)
}

func (h *MessageHandler) acquire(ctx context.Context, jobs []*scaleset.JobAvailable) error {
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.RunnerRequestID)
	}
	h.logger.Info("Acquiring jobs", slog.Int("count", len(ids)))
	acquired, err := h.acquirer.AcquireJobs(ctx, ids)
	if err != nil {
		return fmt.Errorf("failed to acquire jobs: %w", err)
	}
	h.logger.Info("Jobs acquired", slog.Int("count", len(acquired)))
	return nil
}

func (h *MessageHandler) updateDemand(ctx context.Context, statistics *scaleset.RunnerScaleSetStatistic) error {
	desired, err := h.controller.HandleDesiredRunnerCount(ctx, statistics.TotalAssignedJobs)
	if err != nil {
		return fmt.Errorf("failed to handle desired runner count: %w", err)
	}
	h.recorder.RecordDesiredRunners(desired)
	return nil
}
