package controller

import (
	"context"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/actions/scaleset"

	"github.com/ysya/runscaler/internal/capacity"
)

func TestAutoMinRunners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		p := &mockProvider{}
		s := NewScaleSetController(1, 0, 3, p, &mockScaleset{}, slog.New(slog.DiscardHandler), WithAutoMinRunners())
		defer s.Shutdown(ctx)

		demand := func(jobs, want int) {
			t.Helper()
			got, err := s.HandleDesiredRunnerCount(ctx, jobs)
			if err != nil || got != want {
				t.Fatalf("demand %d: target = %d, err = %v, want %d", jobs, got, err, want)
			}
			synctest.Wait()
		}
		checkCount := func(want int) {
			t.Helper()
			synctest.Wait()
			if got := s.instances.count(); got != want {
				t.Fatalf("runner count = %d, want %d", got, want)
			}
		}

		demand(0, 0)
		checkCount(0)
		demand(1, 2)
		checkCount(2)
		first := p.started[0]
		if err := s.HandleJobStarted(ctx, &scaleset.JobStarted{RunnerName: first}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(7 * time.Minute)
		if err := s.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: first}); err != nil {
			t.Fatal(err)
		}
		demand(0, 1)
		checkCount(1)
		if p.startedCount() != 2 {
			t.Fatal("serial successor should already have a fresh runner available")
		}

		time.Sleep(4 * time.Minute)
		demand(3, 3)
		checkCount(3)
		demand(0, 1)
		time.Sleep(4 * time.Minute)
		demand(0, 1) // repeated idle statistics must not extend the grace period
		time.Sleep(time.Minute)
		checkCount(0) // expiry alone wakes the controller
	})
}

func TestAutoMinRunnersCapacityWaitExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		broker := capacity.NewBroker(1)
		auto := broker.Register("automatic", 0)
		other, err := broker.Register("other", 0).Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p := &mockProvider{}
		s := NewScaleSetController(1, 0, 3, p, &mockScaleset{}, slog.New(slog.DiscardHandler), WithCapacityAllocator(auto), WithAutoMinRunners())
		defer s.Shutdown(ctx)
		for _, jobs := range []int{1, 0} {
			if _, err := s.HandleDesiredRunnerCount(ctx, jobs); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if s.RuntimeStatus().WaitingForCapacity {
			t.Fatal("expired spare still waiting for shared capacity")
		}
		other.Release()
		synctest.Wait()
		if got := p.startedCount(); got != 0 {
			t.Fatalf("started %d runners after the idle window expired", got)
		}
	})
}
