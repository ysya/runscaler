package config

import (
	"fmt"
	"time"
)

type RetryConfig struct {
	Initial       time.Duration `mapstructure:"initial"`
	Max           time.Duration `mapstructure:"max"`
	CooldownAfter int           `mapstructure:"cooldown-after"`
	Cooldown      time.Duration `mapstructure:"cooldown"`
}

func DefaultRetry() RetryConfig {
	return RetryConfig{Initial: 5 * time.Second, Max: time.Minute, CooldownAfter: 5, Cooldown: 5 * time.Minute}
}

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
	return 5 * time.Minute
}

type DiagnosticsConfig struct {
	Enabled     *bool `mapstructure:"enabled"`
	CaptureLogs bool  `mapstructure:"capture-logs"`
}

func (d DiagnosticsConfig) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }
