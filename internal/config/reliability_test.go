package config

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestReliabilityDefaultsAndExplicitPresence(t *testing.T) {
	ss := ScaleSetConfig{}
	if !ss.IsDockerReadinessEnabled() || ss.DockerReadyTimeout() != 5*time.Minute {
		t.Fatal("unexpected readiness defaults")
	}
	r := ss.EffectiveRetry()
	if r.Initial != 5*time.Second || r.Max != time.Minute || r.CooldownAfter != 5 || r.Cooldown != 5*time.Minute {
		t.Fatalf("retry defaults=%+v", r)
	}
	if !(DiagnosticsConfig{}).IsEnabled() {
		t.Fatal("diagnostics should default on")
	}
	cfg := loadTOML(t, `[docker]
readiness=false
ready-timeout="2m"
[retry]
initial="10s"
[diagnostics]
enabled=false
[[scaleset]]
name="inherited"
[[scaleset]]
name="overridden"
[scaleset.docker]
readiness=true
`)
	sets := cfg.ResolveScaleSets()
	if len(cfg.Warnings) != 0 || cfg.Diagnostics.IsEnabled() || sets[0].IsDockerReadinessEnabled() || !sets[1].IsDockerReadinessEnabled() || sets[0].DockerReadyTimeout() != 2*time.Minute || sets[0].EffectiveRetry().Initial != 10*time.Second || sets[0].EffectiveRetry().Max != time.Minute {
		t.Fatalf("inheritance lost: %+v warnings=%q", sets, cfg.Warnings)
	}
}

func TestReliabilitySettingsRejectInvalidValues(t *testing.T) {
	for _, extra := range []string{`[docker]
ready-timeout="0s"`, `[retry]
initial="0s"`, `[retry]
initial="2m"
max="1m"`, `[retry]
cooldown-after=0`, `[retry]
cooldown="1s"`} {
		v := viper.New()
		v.SetConfigType("toml")
		body := "url='https://github.com/example'\nname='test'\ntoken='synthetic'\n" + extra
		if err := v.ReadConfig(strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(v)
		if err != nil {
			t.Fatal(err)
		}
		ss := cfg.ResolveScaleSets()[0]
		if err := ss.Validate(); err == nil {
			t.Fatalf("invalid reliability settings accepted: %s", extra)
		}
	}
}
