package provider

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/ysya/runscaler/internal/backoff"
	"github.com/ysya/runscaler/internal/config"
)

// Each warm-pool slot owns its retry history; it never changes controller
// startup accounting or listener/cleanup retry policies.
type poolRetry struct {
	settings config.RetryConfig
	failures int
}

func (r *poolRetry) failure() time.Duration {
	r.failures++
	s := r.settings
	return backoff.Delay(r.failures, s.Initial, s.Max, s.Cooldown, s.CooldownAfter, rand.Float64())
}
func (r *poolRetry) reset() { r.failures = 0 }
func waitPoolRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
