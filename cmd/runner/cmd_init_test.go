package main

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

func TestInitGeneratedDockerConfigPassesStrictLoad(t *testing.T) {
	output := filepath.Join(t.TempDir(), "config.toml")
	c := &cobra.Command{}
	f := c.Flags()
	f.String("output", output, "")
	f.String("url", "https://github.com/test-org", "")
	f.String("name", "test-runners", "")
	f.String("token", "fake-token", "")
	f.Int("max-runners", 1, "")
	f.String("backend", "docker", "")
	f.String("runner-image", "test-image", "")
	f.Bool("dind", false, "")
	f.String("shared-volume", "", "")
	for name, value := range map[string]string{
		"output": output, "url": "https://github.com/test-org", "name": "test-runners",
		"token": "fake-token", "max-runners": "1", "backend": "docker",
		"runner-image": "test-image", "dind": "false", "shared-volume": "",
	} {
		if err := f.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := runInit(c, nil); err != nil {
		t.Fatal(err)
	}

	v := viper.New()
	v.SetConfigFile(output)
	if err := v.ReadInConfig(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("generated config warnings = %v", cfg.Warnings)
	}
	sets := cfg.ResolveScaleSets()
	if len(sets) != 1 || sets[0].Validate() != nil {
		t.Fatalf("generated scale set invalid: %+v", sets)
	}
}
