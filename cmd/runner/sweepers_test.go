package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunPeriodicSweepsImmediatelyThenOnInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sweeps atomic.Int32
	runPeriodic(ctx, 5*time.Millisecond, func() { sweeps.Add(1) })
	deadline := time.Now().Add(2 * time.Second)
	for sweeps.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if sweeps.Load() < 3 {
		t.Fatalf("sweeps = %d, want an initial sweep followed by ticks", sweeps.Load())
	}
	cancel()
	time.Sleep(20 * time.Millisecond)
	settled := sweeps.Load()
	time.Sleep(30 * time.Millisecond)
	if sweeps.Load() != settled {
		t.Fatalf("sweeps continued after cancel: %d -> %d", settled, sweeps.Load())
	}
}
