package controller

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"testing"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"

	"github.com/ysya/runscaler/internal/metrics"
)

// fakeAcquirer stands in for the listener session's AcquireJobs. before runs
// at the start of each call so a test can see what the controller knew then.
type fakeAcquirer struct {
	calls  [][]int64
	err    error
	before func()
}

func (f *fakeAcquirer) AcquireJobs(_ context.Context, requestIDs []int64) ([]int64, error) {
	f.calls = append(f.calls, slices.Clone(requestIDs))
	if f.before != nil {
		f.before()
	}
	if f.err != nil {
		return nil, f.err
	}
	return requestIDs, nil
}

// sessionStub is an in-memory listener session. It serves scripted messages,
// records acks, and cancels the run once the messages are used up, so the
// SDK's own listener can drive MessageHandler.
type sessionStub struct {
	fakeAcquirer
	messages []*scaleset.RunnerScaleSetMessage
	acked    []int
	cancel   context.CancelFunc
}

func (c *sessionStub) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), Statistics: stats(0)}
}

func (c *sessionStub) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	if len(c.messages) == 0 {
		c.cancel()
		return nil, ctx.Err()
	}
	msg := c.messages[0]
	c.messages = c.messages[1:]
	return msg, nil
}

func (c *sessionStub) DeleteMessage(_ context.Context, messageID int) error {
	c.acked = append(c.acked, messageID)
	return nil
}

func newTestMessageHandler(t *testing.T, minRunners, maxRunners int) (*MessageHandler, *ScaleSetController, *fakeAcquirer, *metrics.Recorder) {
	t.Helper()
	s, _, _ := newTestController(t, minRunners, maxRunners)
	acquirer := &fakeAcquirer{}
	recorder := &metrics.Recorder{}
	return NewMessageHandler(acquirer, s, recorder, slog.New(slog.DiscardHandler)), s, acquirer, recorder
}

func stats(assigned int) *scaleset.RunnerScaleSetStatistic {
	return &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned}
}

func initialMessage(assigned int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{MessageID: listener.InitialMessageID, Statistics: stats(assigned)}
}

func jobAvailable(requestID int64) *scaleset.JobAvailable {
	return &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: requestID}}
}

func TestMessageHandlerInitialStatisticsSetDemand(t *testing.T) {
	h, s, acquirer, recorder := newTestMessageHandler(t, 0, 5)

	if err := h.Scale(context.Background(), initialMessage(3)); err != nil {
		t.Fatalf("Scale(initial) error: %v", err)
	}

	if got := s.targetRunnerCount(); got != 3 {
		t.Errorf("target runners = %d, want 3", got)
	}
	if len(acquirer.calls) != 0 {
		t.Errorf("AcquireJobs calls = %v, want none for the initial statistics", acquirer.calls)
	}
	snap := recorder.Snapshot()
	if snap.Statistics == nil || snap.Statistics.TotalAssignedJobs != 3 {
		t.Errorf("recorded statistics = %+v, want 3 assigned jobs", snap.Statistics)
	}
	if snap.DesiredRunners != 3 {
		t.Errorf("recorded desired runners = %d, want 3", snap.DesiredRunners)
	}
}

func TestMessageHandlerAcquiresOfferedJobsBeforeUpdatingDemand(t *testing.T) {
	h, s, acquirer, _ := newTestMessageHandler(t, 0, 5)
	ctx := context.Background()
	if err := h.Scale(ctx, initialMessage(0)); err != nil {
		t.Fatalf("Scale(initial) error: %v", err)
	}
	targetAtAcquire := -1
	acquirer.before = func() { targetAtAcquire = s.targetRunnerCount() }

	err := h.Scale(ctx, &scaleset.RunnerScaleSetMessage{
		MessageID:            1,
		Statistics:           stats(2),
		JobAvailableMessages: []*scaleset.JobAvailable{jobAvailable(101), jobAvailable(102)},
	})
	if err != nil {
		t.Fatalf("Scale() error: %v", err)
	}

	if want := [][]int64{{101, 102}}; !reflect.DeepEqual(acquirer.calls, want) {
		t.Errorf("AcquireJobs calls = %v, want %v", acquirer.calls, want)
	}
	if targetAtAcquire != 0 {
		t.Errorf("target runners while acquiring = %d, want 0: jobs must be acquired before demand changes", targetAtAcquire)
	}
	if got := s.targetRunnerCount(); got != 2 {
		t.Errorf("target runners = %d, want 2", got)
	}
}

// A failed acquire must leave the message unprocessed. The listener does not
// ack it, so it arrives again and is handled from the start.
func TestMessageHandlerAcquireFailureLeavesMessageForRedelivery(t *testing.T) {
	h, s, acquirer, recorder := newTestMessageHandler(t, 0, 5)
	ctx := context.Background()
	s.instances.addIdle("runner-abc", "instance-abc")
	if err := h.Scale(ctx, initialMessage(1)); err != nil {
		t.Fatalf("Scale(initial) error: %v", err)
	}
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:            7,
		Statistics:           stats(2),
		JobAvailableMessages: []*scaleset.JobAvailable{jobAvailable(201)},
		JobStartedMessages: []*scaleset.JobStarted{{
			JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 200, JobID: "job-200"},
			RunnerName:     "runner-abc",
		}},
	}

	errAcquire := errors.New("acquire failed")
	acquirer.err = errAcquire
	if err := h.Scale(ctx, msg); !errors.Is(err, errAcquire) {
		t.Fatalf("Scale() error = %v, want the acquire error", err)
	}
	if got := s.targetRunnerCount(); got != 1 {
		t.Errorf("target runners after failed acquire = %d, want 1", got)
	}
	if phase, _ := s.instances.phase("runner-abc"); phase != instanceIdle {
		t.Errorf("runner phase after failed acquire = %v, want idle", phase)
	}
	if got := recorder.Snapshot().JobsStarted; got != 0 {
		t.Errorf("jobs started after failed acquire = %d, want 0", got)
	}

	acquirer.err = nil
	if err := h.Scale(ctx, msg); err != nil {
		t.Fatalf("Scale(redelivered) error: %v", err)
	}
	if want := [][]int64{{201}, {201}}; !reflect.DeepEqual(acquirer.calls, want) {
		t.Errorf("AcquireJobs calls = %v, want %v", acquirer.calls, want)
	}
	if got := s.targetRunnerCount(); got != 2 {
		t.Errorf("target runners after redelivery = %d, want 2", got)
	}
	if phase, _ := s.instances.phase("runner-abc"); phase != instanceBusy {
		t.Errorf("runner phase after redelivery = %v, want busy", phase)
	}
	if got := recorder.Snapshot().JobsStarted; got != 1 {
		t.Errorf("jobs started after redelivery = %d, want 1", got)
	}
}

// Completions come before the message's desired count, which clears the
// completion barrier. The reverse order would leave scale-up blocked.
func TestMessageHandlerHandlesCompletionsBeforeDemand(t *testing.T) {
	h, s, _, recorder := newTestMessageHandler(t, 0, 5)
	ctx := context.Background()
	s.instances.addIdle("runner-abc", "instance-abc")
	if err := h.Scale(ctx, initialMessage(1)); err != nil {
		t.Fatalf("Scale(initial) error: %v", err)
	}
	s.instances.markBusy("runner-abc", jobRef{})

	err := h.Scale(ctx, &scaleset.RunnerScaleSetMessage{
		MessageID:  4,
		Statistics: stats(0),
		JobCompletedMessages: []*scaleset.JobCompleted{{
			JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 300, JobID: "job-300"},
			RunnerName:     "runner-abc",
			Result:         "succeeded",
		}},
	})
	if err != nil {
		t.Fatalf("Scale() error: %v", err)
	}

	if got := s.instances.count(); got != 0 {
		t.Errorf("runner count = %d, want 0 after the job completed", got)
	}
	if _, canScale := s.scaleTarget(); !canScale {
		t.Error("scale-up blocked after the message; want its desired count to clear the completion barrier")
	}
	if got := recorder.Snapshot().JobsCompleted; got != 1 {
		t.Errorf("jobs completed = %d, want 1", got)
	}
}

// An idle poll carries no message, yet the controller still needs a fresh
// desired count: a runner that exits without a completion message blocks
// scale-up until demand is refreshed (see handleExit).
func TestMessageHandlerIdlePollRefreshesDemandFromLastStatistics(t *testing.T) {
	h, s, acquirer, _ := newTestMessageHandler(t, 0, 5)
	ctx := context.Background()

	// Before any statistics an idle poll has nothing to report.
	if err := h.Scale(ctx, nil); err != nil {
		t.Fatalf("Scale(nil) before statistics error: %v", err)
	}
	if err := h.Scale(ctx, initialMessage(2)); err != nil {
		t.Fatalf("Scale(initial) error: %v", err)
	}
	s.desiredMu.Lock()
	s.demandStale = true
	s.desiredMu.Unlock()

	if err := h.Scale(ctx, nil); err != nil {
		t.Fatalf("Scale(nil) error: %v", err)
	}

	if _, canScale := s.scaleTarget(); !canScale {
		t.Error("scale-up still blocked after an idle poll; want demand refreshed from the last statistics")
	}
	if got := s.targetRunnerCount(); got != 2 {
		t.Errorf("target runners = %d, want 2 from the last statistics", got)
	}
	if len(acquirer.calls) != 0 {
		t.Errorf("AcquireJobs calls = %v, want none", acquirer.calls)
	}
}

// A message without statistics must not erase the demand an idle poll falls
// back to.
func TestMessageHandlerMessageWithoutStatisticsKeepsLastDemand(t *testing.T) {
	h, s, _, recorder := newTestMessageHandler(t, 0, 5)
	ctx := context.Background()
	if err := h.Scale(ctx, initialMessage(2)); err != nil {
		t.Fatalf("Scale(initial) error: %v", err)
	}

	if err := h.Scale(ctx, &scaleset.RunnerScaleSetMessage{MessageID: 3}); err != nil {
		t.Fatalf("Scale(no statistics) error: %v", err)
	}
	s.desiredMu.Lock()
	s.demandStale = true
	s.desiredMu.Unlock()
	if err := h.Scale(ctx, nil); err != nil {
		t.Fatalf("Scale(nil) error: %v", err)
	}

	if _, canScale := s.scaleTarget(); !canScale {
		t.Error("idle poll did not refresh demand; want the statistics from before the message kept")
	}
	if got := s.targetRunnerCount(); got != 2 {
		t.Errorf("target runners = %d, want 2", got)
	}
	if snap := recorder.Snapshot(); snap.Statistics == nil || snap.Statistics.TotalAssignedJobs != 2 {
		t.Errorf("recorded statistics = %+v, want the last non-nil statistics", snap.Statistics)
	}
}

// The SDK listener acks a message only after Scale succeeds. Run against an
// in-memory session, a failed acquire must leave the message unacked for the
// next session, which acquires the job and only then acks it.
func TestSDKListenerAcksMessageOnlyAfterJobsAreAcquired(t *testing.T) {
	s, _, _ := newTestController(t, 0, 5)
	recorder := &metrics.Recorder{}
	offer := &scaleset.RunnerScaleSetMessage{
		MessageID:            1,
		Statistics:           stats(1),
		JobAvailableMessages: []*scaleset.JobAvailable{jobAvailable(101)},
	}
	run := func(acquireErr error) (*sessionStub, error) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		session := &sessionStub{messages: []*scaleset.RunnerScaleSetMessage{offer}, cancel: cancel}
		session.err = acquireErr
		l, err := listener.New(session, listener.Config{ScaleSetID: 1, MaxRunners: 5})
		if err != nil {
			t.Fatal(err)
		}
		return session, l.Run(ctx, NewMessageHandler(session, s, recorder, slog.New(slog.DiscardHandler)))
	}

	errAcquire := errors.New("acquire failed")
	first, err := run(errAcquire)
	if !errors.Is(err, errAcquire) {
		t.Fatalf("Run() error = %v, want the acquire error", err)
	}
	if len(first.acked) != 0 {
		t.Fatalf("acked messages = %v after a failed acquire, want none", first.acked)
	}

	second, err := run(nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled once the messages run out", err)
	}
	if want := [][]int64{{101}}; !reflect.DeepEqual(second.calls, want) {
		t.Errorf("AcquireJobs calls = %v, want %v", second.calls, want)
	}
	if want := []int{1}; !reflect.DeepEqual(second.acked, want) {
		t.Errorf("acked messages = %v, want %v", second.acked, want)
	}
	if got := s.targetRunnerCount(); got != 1 {
		t.Errorf("target runners = %d, want 1", got)
	}
}

// An error from the controller must reach the listener so it does not ack
// the message.
func TestMessageHandlerReturnsDemandErrors(t *testing.T) {
	h, _, _, _ := newTestMessageHandler(t, 0, 5)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := h.Scale(ctx, initialMessage(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Scale() error = %v, want context.Canceled", err)
	}
}
