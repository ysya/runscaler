package config

import (
	"strings"
	"testing"
	"time"
)

func TestSweepDefaults(t *testing.T) {
	ss := validScaleSetConfig()
	if ss.EffectiveSweepMode() != SweepModeOff || ss.EffectiveSweepInterval() != 10*time.Minute {
		t.Fatalf("defaults = %q %v, want off and 10m", ss.EffectiveSweepMode(), ss.EffectiveSweepInterval())
	}
	if err := ss.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSweepValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		modify  func(*ScaleSetConfig)
		wantErr string
	}{
		{"log on org", func(c *ScaleSetConfig) { c.SweepMode = SweepModeLog }, ""},
		{"remove on org", func(c *ScaleSetConfig) { c.SweepMode = SweepModeRemove; c.SweepInterval = time.Minute }, ""},
		{"off on repository", func(c *ScaleSetConfig) { c.RegistrationURL = "https://github.com/org/repo"; c.SweepMode = SweepModeOff }, ""},
		{"unknown mode", func(c *ScaleSetConfig) { c.SweepMode = "delete" }, "sweep-mode must be"},
		{"interval below a minute", func(c *ScaleSetConfig) { c.SweepMode = SweepModeLog; c.SweepInterval = 30 * time.Second }, "sweep-interval must be at least 1m"},
		{"negative interval", func(c *ScaleSetConfig) { c.SweepInterval = -time.Minute }, "sweep-interval must be at least"},
		{"repository scope", func(c *ScaleSetConfig) { c.RegistrationURL = "https://github.com/org/repo"; c.SweepMode = SweepModeLog }, "requires an organization registration URL"},
		{"enterprise scope", func(c *ScaleSetConfig) {
			c.RegistrationURL = "https://github.com/enterprises/ent"
			c.SweepMode = SweepModeRemove
		}, "repository and enterprise scale sets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ss := validScaleSetConfig()
			tc.modify(&ss)
			err := ss.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoad_SweepKeysPerScaleSetAndInherited(t *testing.T) {
	cfg := loadTOML(t, `
sweep-mode = "log"

[[scaleset]]
url = "https://github.com/org"
name = "a"
token = "ghp_x"

[[scaleset]]
url = "https://github.com/org"
name = "b"
token = "ghp_x"
sweep-mode = "remove"
sweep-interval = "15m"
`)
	if len(cfg.Warnings) != 0 {
		t.Fatalf("warnings: %v", cfg.Warnings)
	}
	sets := cfg.ResolveScaleSets()
	if sets[0].EffectiveSweepMode() != SweepModeLog || sets[0].EffectiveSweepInterval() != DefaultSweepInterval {
		t.Fatalf("scaleset a = %q %v", sets[0].EffectiveSweepMode(), sets[0].EffectiveSweepInterval())
	}
	if sets[1].EffectiveSweepMode() != SweepModeRemove || sets[1].EffectiveSweepInterval() != 15*time.Minute {
		t.Fatalf("scaleset b = %q %v", sets[1].EffectiveSweepMode(), sets[1].EffectiveSweepInterval())
	}
}
