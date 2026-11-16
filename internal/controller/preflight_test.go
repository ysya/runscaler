package controller

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

type preflightProvider struct {
	mockProvider
	err error
}

func (p *preflightProvider) CheckStart(context.Context) error { return p.err }

func TestPreflightRefusalDoesNotRegisterRunner(t *testing.T) {
	p := &preflightProvider{err: errors.New("insufficient clone space")}
	api := &mockScaleset{}
	notices := make(chan struct{}, 1)
	s := NewScaleSetController(1, 0, 1, p, api, slog.New(slog.DiscardHandler), WithStartFailureNotifier(func() { notices <- struct{}{} }))
	defer s.Shutdown(context.Background())
	if _, err := s.startInstance(context.Background()); !errors.Is(err, p.err) {
		t.Fatalf("start error = %v", err)
	}
	api.mu.Lock()
	generated := api.generated
	api.mu.Unlock()
	if generated != 0 {
		t.Fatalf("registered %d runners despite preflight refusal", generated)
	}
	if s.instances.count() != 0 {
		t.Fatal("refused start retained capacity")
	}
	select {
	case <-notices:
		t.Fatal("preflight refusal unnecessarily triggered registration sweep")
	default:
	}
}
