package config

import (
	"fmt"
	"time"

	"github.com/ysya/runscaler/internal/backoff"
)

type RetryConfig struct {
	Initial       time.Duration `mapstructure:"initial"`
	Max           time.Duration `mapstructure:"max"`
	CooldownAfter int           `mapstructure:"cooldown-after"`
	Cooldown      time.Duration `mapstructure:"cooldown"`
}

func DefaultRetry() RetryConfig {
	return RetryConfig{Initial: DefaultRetryInitial, Max: DefaultRetryMax, CooldownAfter: DefaultRetryCooldownAfter, Cooldown: DefaultRetryCooldown}
}

// Delay is the jittered wait after the given number of consecutive failed
// starts; sample in [0, 1] picks the point within the jitter window.
func (r RetryConfig) Delay(failures int, sample float64) time.Duration {
	return backoff.Delay(failures, r.Initial, r.Max, r.Cooldown, r.CooldownAfter, sample)
}

// CoolingDown reports whether that many consecutive failures have moved
// from the exponential ramp to the flat cooldown.
func (r RetryConfig) CoolingDown(failures int) bool { return failures >= r.CooldownAfter }

func defaultRetrySettings() map[string]any {
	r := DefaultRetry()
	return map[string]any{"initial": r.Initial, "max": r.Max, "cooldown-after": r.CooldownAfter, "cooldown": r.Cooldown}
}

func (r RetryConfig) Validate() error {
	if r.Initial <= 0 || r.Max < r.Initial || r.Cooldown < r.Max || r.CooldownAfter < 1 {
		return fmt.Errorf("retry requires initial > 0, max >= initial, cooldown >= max and cooldown-after >= 1")
	}
	return nil
}

func (ss *ScaleSetConfig) EffectiveRetry() RetryConfig {
	if ss.Retry != nil {
		return *ss.Retry
	}
	return DefaultRetry()
}

func (ss *ScaleSetConfig) IsDockerReadinessEnabled() bool {
	return ss.Docker.Readiness == nil || *ss.Docker.Readiness
}

func (ss *ScaleSetConfig) DockerReadyTimeout() time.Duration {
	if ss.Docker.ReadyTimeout != nil {
		return *ss.Docker.ReadyTimeout
	}
	return DefaultReadyTimeout
}

type DiagnosticsConfig struct {
	Enabled     *bool `mapstructure:"enabled"`
	CaptureLogs bool  `mapstructure:"capture-logs"`
}

func (d DiagnosticsConfig) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }
