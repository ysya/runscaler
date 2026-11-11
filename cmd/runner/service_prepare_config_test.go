package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ysya/runscaler/internal/config"
)

func TestServicePreparationUsesManagerDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		port        int
	}{
		{"omitted", "", config.DefaultHealthPort},
		{"explicit-zero", "health-port=0\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			body := "url='https://github.com/example'\nname='fixture'\ntoken='synthetic-fixture-token'\n" + tc.extra
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, raw, err := loadServicePreparationConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LogLevel != config.DefaultLogLevel || cfg.LogFormat != config.DefaultLogFormat || cfg.HealthPort != tc.port || string(raw) != body {
				t.Fatalf("preparation defaults=%+v", cfg)
			}
			ss := cfg.ResolveScaleSets()[0]
			if err := ss.Validate(); err != nil {
				t.Fatal(err)
			}
			if ss.MaxRunners != config.DefaultMaxRunners || ss.RunnerImage != config.DefaultRunnerImage || ss.RunnerGroup != config.DefaultRunnerGroup {
				t.Fatal("preparation lost runner defaults")
			}
		})
	}
}

func TestServicePreparationAcceptsDeprecatedCacheVolumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "url='https://github.com/example'\nname='fixture'\ntoken='synthetic-fixture-token'\n[docker]\ncache-volumes=['gradle-cache:/home/runner/.gradle']\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadServicePreparationConfig(path)
	if err != nil {
		t.Fatalf("deprecated but supported config rejected: %v", err)
	}
	if len(cfg.Warnings) != 0 || len(cfg.Deprecations) != 1 {
		t.Fatalf("Warnings = %q, Deprecations = %q", cfg.Warnings, cfg.Deprecations)
	}
}
