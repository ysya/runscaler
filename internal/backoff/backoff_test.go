package backoff

import (
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
