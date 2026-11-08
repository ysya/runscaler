// Package backoff holds the retry-delay arithmetic and the cancellable wait
// that runner's retry loops share: startup and warm-pool retries, listener
// reconnects, runner-removal retries and guest-agent polling.
package backoff

import (
	"context"
	"time"
)

// Exponential is the wait after the given number of consecutive failures:
// initial for the first, doubling with each further failure, capped at
// maximum. No failures means no wait.
func Exponential(failures int, initial, maximum time.Duration) time.Duration {
	if failures <= 0 {
		return 0
	}
	delay := initial
	for n := 1; n < failures && delay < maximum; n++ {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	return min(delay, maximum)
}

// Jitter picks a point in [d/2, d] by sample, clamped to [0, 1], so owners
// retrying at the same moment spread out instead of retrying in lockstep.
func Jitter(d time.Duration, sample float64) time.Duration {
	sample = min(max(sample, 0), 1)
	return d/2 + time.Duration(float64(d-d/2)*sample)
}

// Delay is the startup-retry wait: Exponential with Jitter, replaced by a
// flat cooldown once failures reach cooldownAfter.
func Delay(failures int, initial, maximum, cooldown time.Duration, cooldownAfter int, sample float64) time.Duration {
	if failures <= 0 {
		return 0
	}
	if failures >= cooldownAfter {
		return cooldown
	}
	return Jitter(Exponential(failures, initial, maximum), sample)
}

// Sleep waits d, or until ctx ends, whichever comes first; in the latter case
// it returns ctx's error.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
