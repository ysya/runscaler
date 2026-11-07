package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/config"
)

func TestPoolRetryCooldownCancellationAndReset(t *testing.T) {
	r := poolRetry{settings: config.DefaultRetry()}
	other := poolRetry{settings: config.DefaultRetry()}
	for i := 0; i < 4; i++ {
		r.failure()
	}
	if got := r.failure(); got != 5*time.Minute {
		t.Fatalf("cooldown=%v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitPoolRetry(ctx, time.Hour) {
		t.Fatal("cancelled wait succeeded")
	}
	r.reset()
	if d := r.failure(); d < 2500*time.Millisecond || d > 5*time.Second {
		t.Fatalf("reset delay=%v", d)
	}
	if other.failures != 0 {
		t.Fatal("slots shared failure history")
	}
}

func TestPoolWorkerBacksOffBootFailuresAndCancelsCooldown(t *testing.T) {
	cmd := &mockCommandRunner{fallback: cmdResult{err: errors.New("clone failed")}}
	p := newTestTartProvider(cmd)
	p.poolSize = 1
	p.retryConfig = config.RetryConfig{Initial: 10 * time.Millisecond, Max: 10 * time.Millisecond, CooldownAfter: 2, Cooldown: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartPool(ctx)
	deadline := time.Now().Add(time.Second)
	for cmd.callCount("tart clone") < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := cmd.callCount("tart clone"); got != 2 {
		t.Fatalf("boot attempts=%d", got)
	}
	time.Sleep(25 * time.Millisecond)
	if got := cmd.callCount("tart clone"); got != 2 {
		t.Fatalf("cooldown was bypassed: attempts=%d", got)
	}
	cancel()
	done := make(chan struct{})
	go func() { p.poolWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pool cooldown ignored cancellation")
	}
	if len(p.coordinator.slots) != cap(p.coordinator.slots) {
		t.Fatal("failed pool boot leaked host slot")
	}
}
