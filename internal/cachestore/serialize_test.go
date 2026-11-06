package cachestore

import (
	"context"
	"errors"
	"testing"
	"time"
)

// notEnteredWindow is how long a test waits before concluding that a
// second caller is genuinely blocked on the semaphore rather than merely
// not yet scheduled. A false pass here (the caller was slow, not blocked)
// is ruled out by the follow-up assertions in each test: the blocked call
// must go on to enter once released, and maxActive must stay at 1.
const notEnteredWindow = 200 * time.Millisecond

// startCall runs fn in a goroutine and returns a channel that receives its
// error when it finishes. The goroutine is not synchronized with the
// caller beyond that channel — that is exactly what the tests below need
// in order to race two callers against one store.
func startCall(fn func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	return done
}

// TestSerialize_SecondReclaimWaitsForTheFirst is the regression test for
// the hazard serialize exists to close: cmd/runner hands one store instance
// to both a periodic sweeper and the disk guard, so two callers that share
// no lock between them can call Reclaim on it at the same moment. The
// second must wait for the first, not run alongside it and not silently
// return "freed nothing".
func TestSerialize_SecondReclaimWaitsForTheFirst(t *testing.T) {
	inner := newBlockingStore()
	s := serialize(inner)
	ctx := context.Background()

	// Caller A — stands in for the disk guard's sweep — enters Reclaim and
	// blocks there until this test releases it.
	firstDone := startCall(func() error { _, err := s.Reclaim(ctx, Tier4); return err })
	<-inner.entered

	// Caller B — stands in for the periodic sweeper's ticker — calls
	// Reclaim on the same instance while A is still inside it.
	secondDone := startCall(func() error { _, err := s.Reclaim(ctx, Tier4); return err })

	select {
	case <-inner.entered:
		t.Fatal("a second Reclaim entered the store while the first was still running — " +
			"two helper containers would walk and delete the same volume at once")
	case err := <-secondDone:
		t.Fatalf("second Reclaim returned early (err=%v) instead of waiting — a caller that "+
			"reports freeing nothing without doing anything makes the guard escalate a tier "+
			"on the strength of work that was merely skipped", err)
	case <-time.After(notEnteredWindow):
	}

	// Releasing A must let B through — and B must do real work, not skip.
	inner.release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatalf("first Reclaim error: %v", err)
	}
	select {
	case <-inner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("second Reclaim never entered the store after the first finished")
	}
	inner.release <- struct{}{}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Reclaim error: %v", err)
	}

	if maxActive, completed := inner.stats(); maxActive != 1 || completed != 2 {
		t.Errorf("maxActive=%d completed=%d, want 1 and 2 (both callers ran, never at once)", maxActive, completed)
	}
}

// TestSerialize_MeasureAndReclaimDoNotInterleave pins the other half of the
// contract. Reclaim's own before/after `du` and a concurrent Measure over
// the same volume would each report a number taken while the other was
// deleting, so the two must exclude each other and not only themselves.
func TestSerialize_MeasureAndReclaimDoNotInterleave(t *testing.T) {
	inner := newBlockingStore()
	s := serialize(inner)
	ctx := context.Background()

	reclaimDone := startCall(func() error { _, err := s.Reclaim(ctx, Tier4); return err })
	<-inner.entered

	measureDone := startCall(func() error { _, err := s.Measure(ctx); return err })
	select {
	case <-inner.entered:
		t.Fatal("Measure entered the store while Reclaim was mid-delete — it would report a size taken from a half-deleted store")
	case err := <-measureDone:
		t.Fatalf("Measure returned early (err=%v) instead of waiting for the in-flight Reclaim", err)
	case <-time.After(notEnteredWindow):
	}

	inner.release <- struct{}{}
	if err := <-reclaimDone; err != nil {
		t.Fatalf("Reclaim error: %v", err)
	}
	<-inner.entered
	inner.release <- struct{}{}
	if err := <-measureDone; err != nil {
		t.Fatalf("Measure error: %v", err)
	}

	if maxActive, completed := inner.stats(); maxActive != 1 || completed != 2 {
		t.Errorf("maxActive=%d completed=%d, want 1 and 2", maxActive, completed)
	}
}

// TestSerialize_WaitingHonorsContext pins that waiting is bounded by the
// caller's own deadline rather than by the holder's. This is what keeps the
// job-start path's timeout (see controller.startInstance's pre-job Sweep)
// meaningful: without it, a caller could inherit a 10-minute store-level
// timeout it never agreed to.
func TestSerialize_WaitingHonorsContext(t *testing.T) {
	inner := newBlockingStore()
	s := serialize(inner)

	holderDone := startCall(func() error { _, err := s.Reclaim(context.Background(), Tier4); return err })
	<-inner.entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Reclaim(ctx, Tier4)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reclaim error = %v, want a context.DeadlineExceeded — a waiting caller must be "+
			"released by its own deadline, not the holder's", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waiting caller took %s to give up — it did not honor its own deadline", elapsed)
	}

	inner.release <- struct{}{}
	if err := <-holderDone; err != nil {
		t.Fatalf("holder Reclaim error: %v", err)
	}
	if maxActive, completed := inner.stats(); maxActive != 1 || completed != 1 {
		t.Errorf("maxActive=%d completed=%d, want 1 and 1 (the timed-out caller must never reach the store)", maxActive, completed)
	}
}

// TestConstructorsReturnSerializedStores pins the wiring itself. The
// guarantee above is only worth anything if every store the rest of the
// process can get hold of carries it — these constructors are the only way
// to obtain one from outside this package.
func TestConstructorsReturnSerializedStores(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	stores := map[string]CacheStore{
		"docker-garbage":     NewDockerGarbageStore(fake, DockerGarbageConfig{}),
		"docker-build-cache": NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{}),
		"buildx":             NewBuildxStore(fake, BuildxConfig{}),
		"shared-volume":      NewSharedVolumeStore(fake, SharedVolumeConfig{MountPath: "/shared", MaxAge: time.Hour}),
		"cache-volume":       NewCacheVolumeStore(fake, CacheVolumeConfig{MountPath: "/cache"}),
		"tart":               NewTartStore(nil, TartConfig{Home: "/Volumes/A"}),
	}
	for name, s := range stores {
		if _, ok := s.(*serialized); !ok {
			t.Errorf("%s store is %T, not *serialized — the periodic sweepers and the disk guard "+
				"share these instances, so an unwrapped one can be reclaimed from twice at once", name, s)
		}
	}
}
