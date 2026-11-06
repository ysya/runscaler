package capacity

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitForWaiters(t *testing.T, b *Broker, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		got := len(b.waiters)
		b.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("broker waiters did not reach %d", want)
}

func TestBrokerBlocksAtLimitAndReleaseWakesWaiter(t *testing.T) {
	b := NewBroker(1)
	a := b.Register("one", 0)
	first, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := b.InUse(); got != 1 {
		t.Fatalf("InUse() = %d, want 1", got)
	}

	acquired := make(chan *Lease, 1)
	go func() {
		lease, acquireErr := a.Acquire(context.Background())
		if acquireErr == nil {
			acquired <- lease
		}
	}()

	select {
	case <-acquired:
		t.Fatal("second lease acquired before capacity was released")
	case <-time.After(25 * time.Millisecond):
	}

	first.Release()
	select {
	case second := <-acquired:
		second.Release()
	case <-time.After(time.Second):
		t.Fatal("waiting acquire did not wake after release")
	}
	if got := b.InUse(); got != 0 {
		t.Fatalf("InUse() = %d after releases, want 0", got)
	}
}

func TestBrokerReservesOtherScaleSetMinimum(t *testing.T) {
	b := NewBroker(2)
	a := b.Register("a", 1)
	c := b.Register("b", 1)

	first, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	surplus := make(chan *Lease, 1)
	go func() {
		lease, acquireErr := a.Acquire(context.Background())
		if acquireErr == nil {
			surplus <- lease
		}
	}()
	select {
	case lease := <-surplus:
		lease.Release()
		t.Fatal("scale set a consumed capacity reserved for scale set b")
	case <-time.After(25 * time.Millisecond):
	}

	reserved, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reserved.Release()
	first.Release()

	select {
	case lease := <-surplus:
		lease.Release()
	case <-time.After(time.Second):
		t.Fatal("surplus waiter did not wake when reserved capacity was released")
	}
}

func TestLeaseReleaseIsIdempotent(t *testing.T) {
	b := NewBroker(1)
	a := b.Register("one", 0)
	lease, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	lease.Release()
	if got := b.InUse(); got != 0 {
		t.Fatalf("InUse() = %d, want 0", got)
	}
}

func TestBrokerServesSurplusInFIFOOrder(t *testing.T) {
	b := NewBroker(1)
	a := b.Register("a", 0)
	c := b.Register("b", 0)
	holder, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	first := make(chan *Lease, 1)
	second := make(chan *Lease, 1)
	go func() {
		lease, acquireErr := a.Acquire(context.Background())
		if acquireErr == nil {
			first <- lease
		}
	}()
	waitForWaiters(t, b, 1)
	go func() {
		lease, acquireErr := c.Acquire(context.Background())
		if acquireErr == nil {
			second <- lease
		}
	}()
	waitForWaiters(t, b, 2)

	holder.Release()
	var firstLease *Lease
	select {
	case firstLease = <-first:
	case <-second:
		t.Fatal("later surplus waiter acquired capacity first")
	case <-time.After(time.Second):
		t.Fatal("first surplus waiter did not acquire capacity")
	}
	firstLease.Release()
	select {
	case lease := <-second:
		lease.Release()
	case <-time.After(time.Second):
		t.Fatal("second surplus waiter did not acquire after release")
	}
}

func TestBrokerAcquireHonorsCancellation(t *testing.T) {
	b := NewBroker(1)
	a := b.Register("one", 0)
	lease, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, acquireErr := a.Acquire(ctx)
		result <- acquireErr
	}()
	waitForWaiters(t, b, 1)
	cancel()
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire() error = %v, want context.Canceled", err)
	}
	waitForWaiters(t, b, 0)
}
