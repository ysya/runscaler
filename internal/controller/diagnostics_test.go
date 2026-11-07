package controller

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/capacity"
	"github.com/ysya/runscaler/internal/diagnostics"
	"github.com/ysya/runscaler/internal/provider"
)

type diagnosticWatcher struct {
	watcherProvider
	fail      atomic.Bool
	attempts  atomic.Int64
	collected atomic.Int64
}

func (p *diagnosticWatcher) RemoveInstance(ctx context.Context, id string) error {
	p.attempts.Add(1)
	if p.fail.Load() {
		return errors.New("cleanup failed")
	}
	return p.watcherProvider.RemoveInstance(ctx, id)
}
func (p *diagnosticWatcher) CollectDiagnostics(context.Context, string) (provider.ExitDiagnostics, error) {
	p.collected.Add(1)
	return provider.ExitDiagnostics{}, errors.New("private diagnostic failure")
}

func TestExitedCleanupFailureRetainsLeaseAndDiagnosticsNeverBlockCleanup(t *testing.T) {
	p := &diagnosticWatcher{watcherProvider: watcherProvider{waits: make(map[string]chan struct{})}}
	p.fail.Store(true)
	broker := capacity.NewBroker(1)
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	s := NewScaleSetController(1, 0, 1, p, &mockScaleset{}, slog.New(slog.DiscardHandler), WithCapacityAllocator(broker.Register("test", 0)), WithDiagnostics(diagnostics.NewStore(filepath.Join(parent, "reports"), false), "test", "docker"))
	defer s.Shutdown(context.Background())
	name, err := s.startInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.crashFirst()
	waitFor(t, func() bool {
		s.instances.mu.Lock()
		defer s.instances.mu.Unlock()
		return s.instances.instances[name] != nil && s.instances.instances[name].cleanupAttempts > 0
	}, "cleanup was not attempted")
	if broker.InUse() != 1 {
		t.Fatal("failed cleanup released lease")
	}
	if phase, ok := s.instances.phase(name); !ok || phase != instanceRemoving {
		t.Fatal("exited runner detached before cleanup")
	}
	p.fail.Store(false)
	s.instances.mu.Lock()
	s.instances.instances[name].cleanupRetryAt = time.Time{}
	s.instances.mu.Unlock()
	s.signalReconcile()
	waitFor(t, func() bool { return broker.InUse() == 0 }, "cleanup retry did not release lease")
	if p.collected.Load() != 1 {
		t.Fatal("diagnostics were repeated on cleanup retry")
	}
	if files, _ := os.ReadDir(filepath.Join(parent, "reports")); len(files) != 1 {
		t.Fatal("safe partial report was not saved")
	}
}
