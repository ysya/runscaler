package controller

import (
	"testing"
	"time"
)

func TestRemovalRetryDelayDoublesToEightySeconds(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 80 * time.Second}
	for i, w := range want {
		if got := removalRetryDelay(i + 1); got != w {
			t.Errorf("removalRetryDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
}
