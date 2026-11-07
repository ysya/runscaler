// Package backoff contains delay arithmetic shared by independent retry owners.
package backoff

import "time"

func Delay(failures int, initial, maximum, cooldown time.Duration, cooldownAfter int, sample float64) time.Duration {
	if failures <= 0 {
		return 0
	}
	if failures >= cooldownAfter {
		return cooldown
	}
	delay := initial
	for n := 1; n < failures && delay < maximum; n++ {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	if sample < 0 {
		sample = 0
	}
	if sample > 1 {
		sample = 1
	}
	return delay/2 + time.Duration(float64(delay-delay/2)*sample)
}
