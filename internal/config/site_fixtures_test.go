package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestSiteConfigFixturesLoad(t *testing.T) {
	for _, name := range []string{"docker-pat.toml", "docker-app.toml", "tart-pat.toml", "tart-app.toml"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "site", name))
			if err != nil {
				t.Fatal(err)
			}
			// PAT templates intentionally require a local credential before use.
			raw := strings.Replace(string(data), `token = ""`, `token = "synthetic-test-token"`, 1)
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
			if len(sets) != 1 {
				t.Fatalf("resolved %d scale sets, want one", len(sets))
			}
			if err := sets[0].Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
