package backoff

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDelayBoundsAndCooldown(t *testing.T) {
	for _, tc := range []struct {
		failures int
		sample   float64
		want     time.Duration
	}{{1, 0, 2500 * time.Millisecond}, {1, 1, 5 * time.Second}, {2, 0, 5 * time.Second}, {2, 1, 10 * time.Second}, {5, 0, 5 * time.Minute}, {1000000, 1, 5 * time.Minute}} {
		if got := Delay(tc.failures, 5*time.Second, time.Minute, 5*time.Minute, 5, tc.sample); got != tc.want {
			t.Fatalf("Delay(%d,%v)=%v want %v", tc.failures, tc.sample, got, tc.want)
		}
	}
}

func TestExponentialDoublesToTheCap(t *testing.T) {
	for _, tc := range []struct {
		initial, maximum time.Duration
		want             []time.Duration // for 0, 1, 2, ... failures
	}{
		{5 * time.Second, 80 * time.Second, []time.Duration{0, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 80 * time.Second}},
		{5 * time.Second, time.Minute, []time.Duration{0, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}},
		{time.Second, 5 * time.Second, []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}},
	} {
		for failures, want := range tc.want {
			if got := Exponential(failures, tc.initial, tc.maximum); got != want {
				t.Errorf("Exponential(%d, %v, %v) = %v, want %v", failures, tc.initial, tc.maximum, got, want)
			}
		}
	}
	if got := Exponential(1_000_000, time.Second, time.Hour); got != time.Hour {
		t.Errorf("a long failure streak = %v, want the cap without overflow", got)
	}
}

func TestJitterSpansHalfToFull(t *testing.T) {
	for _, tc := range []struct {
		sample float64
		want   time.Duration
	}{{-1, 5 * time.Second}, {0, 5 * time.Second}, {0.5, 7500 * time.Millisecond}, {1, 10 * time.Second}, {2, 10 * time.Second}} {
		if got := Jitter(10*time.Second, tc.sample); got != tc.want {
			t.Errorf("Jitter(10s, %v) = %v, want %v", tc.sample, got, tc.want)
		}
	}
}

func TestSleepStopsWhenTheContextEnds(t *testing.T) {
	if err := Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("a short sleep = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled sleep = %v, want context.Canceled", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("a cancelled sleep waited out its delay")
	}
}
