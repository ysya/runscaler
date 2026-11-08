package provider

import (
	"math/rand/v2"
	"time"

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
	return r.settings.Delay(r.failures, rand.Float64())
}
func (r *poolRetry) reset() { r.failures = 0 }
