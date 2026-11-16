package controller

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/actions/scaleset"
)

type failedStartProvider struct {
	mockProvider
	lastName string
}

func (p *failedStartProvider) StartInstance(_ context.Context, name, _ string) (string, error) {
	p.lastName = name
	return "", errors.New("provider failed to boot")
}

func TestFailedStartDeregistersKnownJITRunner(t *testing.T) {
	p, api := &failedStartProvider{}, &mockScaleset{}
	s := NewScaleSetController(1, 0, 1, p, api, slog.New(slog.DiscardHandler))
	defer s.Shutdown(context.Background())
	if _, err := s.startInstance(context.Background()); err == nil {
		t.Fatal("start unexpectedly succeeded")
	}
	waitFor(t, func() bool { return len(api.deregisteredIDs()) == 1 }, "failed start left a known JIT registration")
	if ids := api.deregisteredIDs(); ids[0] != 1 {
		t.Fatalf("removed unrelated registration: %v", ids)
	}
	waitFor(t, func() bool { return !s.HasRunner(p.lastName) }, "removed registration remains tracked")
}

func TestFailedStartRegistrationCleanupRetriesIndependentlyOfDemand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, api := &failedStartProvider{}, &mockScaleset{removeErrOnce: map[int]error{1: errors.New("temporary outage")}}
		s := NewScaleSetController(1, 0, 1, p, api, slog.New(slog.DiscardHandler))
		defer s.Shutdown(context.Background())
		_, _ = s.startInstance(context.Background())
		synctest.Wait()
		if len(api.deregisteredIDs()) != 1 || !s.HasRunner(p.lastName) {
			t.Fatal("failed cleanup was not retained")
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if len(api.deregisteredIDs()) != 2 || s.HasRunner(p.lastName) {
			t.Fatal("registration cleanup did not retry with zero demand")
		}
	})
}

func TestFailedStartKeepsRegistrationWhenGitHubReportsJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, api := &failedStartProvider{}, &mockScaleset{removeErr: map[int]error{1: scaleset.JobStillRunningError}}
		s := NewScaleSetController(1, 0, 1, p, api, slog.New(slog.DiscardHandler))
		defer s.Shutdown(context.Background())
		_, _ = s.startInstance(context.Background())
		synctest.Wait()
		if !s.HasRunner(p.lastName) {
			t.Fatal("busy registration was forgotten")
		}
		if p.removedCount() != 0 {
			t.Fatal("busy registration caused provider removal")
		}
		api.mu.Lock()
		delete(api.removeErr, 1)
		api.mu.Unlock()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if s.HasRunner(p.lastName) {
			t.Fatal("completed registration did not converge")
		}
	})
}

type unknownJITScaleset struct{ mockScaleset }

func (s *unknownJITScaleset) GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "fixture"}, nil
}

func TestFailedStartWithoutRunnerIDDoesNotDeleteGuessedRegistration(t *testing.T) {
	p, api := &failedStartProvider{}, &unknownJITScaleset{}
	notices := make(chan struct{}, 1)
	s := NewScaleSetController(1, 0, 1, p, api, slog.New(slog.DiscardHandler), WithStartFailureNotifier(func() { notices <- struct{}{} }))
	_, _ = s.startInstance(context.Background())
	s.Shutdown(context.Background())
	if len(api.deregisteredIDs()) != 0 {
		t.Fatal("unknown registration ID was removed")
	}
	select {
	case <-notices:
	default:
		t.Fatal("unknown-ID failure did not notify recovery sweep")
	}
}

func TestShutdownCleansRegistrationFromCancelledProvisioning(t *testing.T) {
	p, api := &blockingProvider{started: make(chan string, 1), release: make(chan struct{})}, &mockScaleset{}
	s := NewScaleSetController(1, 0, 1, p, api, slog.New(slog.DiscardHandler))
	_, _ = s.HandleDesiredRunnerCount(context.Background(), 1)
	var name string
	select {
	case name = <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider never started")
	}
	s.instances.mu.Lock()
	id := s.instances.instances[name].runnerID
	s.instances.mu.Unlock()
	if id != 1 {
		t.Errorf("JIT ID not saved before provisioning: %d", id)
	}
	s.Shutdown(context.Background())
	if ids := api.deregisteredIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("cancelled provisioning leaked its registration: %v", ids)
	}
}
