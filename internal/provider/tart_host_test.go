package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

func acquireWithin(t *testing.T, c *TartHostCoordinator, guest TartGuest) (tartSlot, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	return c.acquire(ctx, guest)
}

func TestTartHostCoordinator_MacOSLimitDoesNotBlockLinux(t *testing.T) {
	c := NewTartHostCoordinator(2)
	seen := map[int]bool{}
	for _, guest := range []TartGuest{TartGuestMacOS, TartGuestMacOS, TartGuestLinux, TartGuestLinux, TartGuestLinux} {
		s, err := acquireWithin(t, c, guest)
		if err != nil {
			t.Fatalf("%v guest could not start: %v", guest, err)
		}
		if seen[s.index] || s.macOS != (guest == TartGuestMacOS) {
			t.Fatalf("slot %+v for a %v guest; seen %v", s, guest, seen)
		}
		seen[s.index] = true
	}
	if _, err := acquireWithin(t, c, TartGuestMacOS); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a third macOS guest must wait, got %v", err)
	}
}

func TestTartHostCoordinator_ReusesTheLowestFreeIndex(t *testing.T) {
	c := NewTartHostCoordinator(2)
	a, _ := acquireWithin(t, c, TartGuestLinux)
	b, _ := acquireWithin(t, c, TartGuestLinux)
	if a.index != 0 || b.index != 1 {
		t.Fatalf("indexes = %d, %d; want 0, 1", a.index, b.index)
	}
	c.release(a)
	if again, _ := acquireWithin(t, c, TartGuestMacOS); again.index != 0 {
		t.Fatalf("index after release = %d, want 0", again.index)
	}
}

func TestTartHostCoordinator_CancelledWaitHoldsNothing(t *testing.T) {
	c := NewTartHostCoordinator(1)
	held, _ := acquireWithin(t, c, TartGuestMacOS)
	if _, err := acquireWithin(t, c, TartGuestMacOS); err == nil {
		t.Fatal("the second macOS guest must wait")
	}
	c.release(held)
	if _, err := acquireWithin(t, c, TartGuestMacOS); err != nil {
		t.Fatalf("the permit leaked after a cancelled wait: %v", err)
	}
}

func TestTartHostCoordinator_ReleasingTwiceAddsNoPermit(t *testing.T) {
	c := NewTartHostCoordinator(1)
	held, _ := acquireWithin(t, c, TartGuestMacOS)
	c.release(held)
	c.release(held)
	if _, err := acquireWithin(t, c, TartGuestMacOS); err != nil {
		t.Fatalf("the permit was lost: %v", err)
	}
	if _, err := acquireWithin(t, c, TartGuestMacOS); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a repeated release added a second macOS permit: %v", err)
	}
}

func TestNATMAC(t *testing.T) {
	for index, want := range map[int]string{
		0: "02:00:00:00:00:01", 1: "02:00:00:00:00:02", 2: "02:00:00:00:00:03",
		254: "02:00:00:00:00:ff", 255: "02:00:00:00:01:00",
	} {
		if got := natMAC(index); got != want {
			t.Errorf("natMAC(%d) = %q, want %q", index, got, want)
		}
	}
}

func TestTartHostCoordinator_StaleReleaseDoesNotFreeReusedSlot(t *testing.T) {
	c := NewTartHostCoordinator(1)
	old, _ := acquireWithin(t, c, TartGuestMacOS)
	c.release(old)
	current, _ := acquireWithin(t, c, TartGuestMacOS)
	defer c.release(current)
	c.release(old)
	if _, err := acquireWithin(t, c, TartGuestMacOS); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a stale release freed the current VM's permit: %v", err)
	}
}
