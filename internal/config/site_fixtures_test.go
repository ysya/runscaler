package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestSiteConfigFixturesLoad(t *testing.T) {
	for _, name := range []string{"docker-pat.toml", "docker-app.toml", "tart-pat.toml", "tart-app.toml", "full-multi.toml"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "site", name))
			if err != nil {
				t.Fatal(err)
			}
			// PAT templates intentionally require a local credential before use.
			raw := strings.ReplaceAll(string(data), `token = ""`, `token = "synthetic-test-token"`)
			v := viper.New()
			v.SetConfigType("toml")
			if err := v.ReadConfig(strings.NewReader(raw)); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(v)
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Warnings) != 0 {
				t.Fatalf("unexpected config warnings: %v", cfg.Warnings)
			}
			if err := cfg.ValidateGlobal(); err != nil {
				t.Fatal(err)
			}
			sets := cfg.ResolveScaleSets()
			wantSets := 1
			if name == "full-multi.toml" {
				wantSets = 2
			}
			if len(sets) != wantSets {
				t.Fatalf("resolved %d scale sets, want %d", len(sets), wantSets)
			}
			if err := cfg.ValidateConcurrent(sets); err != nil {
				t.Fatal(err)
			}
			for _, set := range sets {
				if err := set.Validate(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
