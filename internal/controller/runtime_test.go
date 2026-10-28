package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/capacity"
)

func TestRuntimeReportsStartupFailureAndRecoveryWithoutSecrets(t *testing.T) {
	s, _, _ := newTestController(t, 0, 1)
	s.desiredMu.Lock()
	s.desiredRunners = 1
	s.desiredMu.Unlock()
	s.scalesetClient = &errorScaleset{err: errors.New("synthetic-private-credential")}
	if _, err := s.startInstance(context.Background()); err == nil {
		t.Fatal("expected JIT failure")
	}
	failed := s.RuntimeStatus()
	if !failed.Degraded || failed.StartAttempts != 1 || failed.StartFailures != 1 || failed.ConsecutiveStartFailures != 1 || failed.FailureReason != "jit_config_failed" || failed.LastStartAttempt.IsZero() {
		t.Fatalf("failure not reported: %+v", failed)
	}
	encoded, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "synthetic-private-credential") {
		t.Fatal("runtime diagnostics exposed an error credential")
	}
	s.desiredMu.Lock()
	s.desiredRunners = 0
	s.desiredMu.Unlock()
	if s.RuntimeStatus().Degraded {
		t.Fatal("historical failure degraded a scale set with no demand")
	}
	s.desiredMu.Lock()
	s.desiredRunners = 1
	s.desiredMu.Unlock()
	s.scalesetClient = &mockScaleset{}
	if _, err := s.startInstance(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered := s.RuntimeStatus()
	if recovered.Degraded || recovered.StartAttempts != 2 || recovered.StartFailures != 1 || recovered.ConsecutiveStartFailures != 0 || recovered.FailureReason != "" || recovered.LastSuccessfulStart.IsZero() {
		t.Fatalf("startup recovery not reported: %+v", recovered)
	}
	if err := s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.RuntimeStatus().Draining {
		t.Fatal("drain boundary not reported")
	}
}

type runtimeFailingProvider struct {
	mockProvider
	cancel context.CancelFunc
}

func (p *runtimeFailingProvider) StartInstance(ctx context.Context, _, _ string) (string, error) {
	if p.cancel != nil {
		p.cancel()
		return "", ctx.Err()
	}
	return "", errors.New("synthetic-provider-secret")
}

func TestRuntimeDistinguishesProviderFailureFromShutdownCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "cancelled"}[cancelled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &runtimeFailingProvider{}
			if cancelled {
				p.cancel = cancel
			}
			s := NewScaleSetController(1, 0, 1, p, &mockScaleset{}, slog.New(slog.DiscardHandler))
			defer s.Shutdown(context.Background())
			if _, err := s.startInstance(ctx); err == nil {
				t.Fatal("expected failed start")
			}
			got := s.RuntimeStatus()
			if got.StartAttempts != 1 {
				t.Fatalf("attempts = %d, want 1", got.StartAttempts)
			}
			if cancelled {
				if got.StartFailures != 0 || got.FailureReason != "" {
					t.Fatalf("cancellation counted as provider failure: %+v", got)
				}
			} else if got.StartFailures != 1 || got.FailureReason != "provider_start_failed" {
				t.Fatalf("provider failure not classified: %+v", got)
			}
		})
	}
}

func TestRuntimeCapacityWaitIsVisibleAndHealthy(t *testing.T) {
	b := capacity.NewBroker(1)
	s := NewScaleSetController(1, 0, 1, &mockProvider{}, &errorScaleset{err: errors.New("jit unavailable")}, slog.New(slog.DiscardHandler), WithCapacityAllocator(b.Register("waiting", 0)))
	defer s.Shutdown(context.Background())
	if _, err := s.startInstance(context.Background()); err == nil {
		t.Fatal("expected initial JIT failure")
	}
	s.scalesetClient = &mockScaleset{}
	holder := b.Register("holder", 0)
	lease, err := holder.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.RuntimeStatus().WaitingForCapacity }, "capacity wait not visible")
	status := s.RuntimeStatus()
	if status.Degraded || status.StartAttempts != 1 || status.StartFailures != 1 {
		t.Fatalf("normal capacity wait counted as failure: %+v", status)
	}
	lease.Release()
	waitFor(t, func() bool { return !s.RuntimeStatus().LastSuccessfulStart.IsZero() }, "runner did not start after capacity became available")
	if s.RuntimeStatus().WaitingForCapacity {
		t.Fatal("capacity wait did not clear after startup")
	}
}

func TestRuntimeFailedSupplyStaysDegradedUntilRetryIsReady(t *testing.T) {
	p := &blockingProvider{started: make(chan string, 1), release: make(chan struct{})}
	s := NewScaleSetController(1, 0, 1, p, &errorScaleset{err: errors.New("jit unavailable")}, slog.New(slog.DiscardHandler))
	defer s.Shutdown(context.Background())
	s.desiredMu.Lock()
	s.desiredRunners = 1
	s.desiredMu.Unlock()
	if _, err := s.startInstance(context.Background()); err == nil {
		t.Fatal("expected initial failure")
	}
	s.scalesetClient = &mockScaleset{}
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not reach provider")
	}
	if !s.RuntimeStatus().Degraded {
		t.Fatal("an unfinished retry hid the provisioning failure")
	}
	close(p.release)
	waitFor(t, func() bool { return !s.RuntimeStatus().LastSuccessfulStart.IsZero() }, "retry did not complete")
	if s.RuntimeStatus().Degraded {
		t.Fatal("successful retry did not restore health")
	}
}
