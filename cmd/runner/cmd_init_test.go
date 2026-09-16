package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

func TestInitGeneratedDockerConfigPassesStrictLoad(t *testing.T) {
	output := runInitTestConfig(t, "provider")
	assertGeneratedConfigValid(t, output)
}

func TestInitLegacyBackendFlagGeneratesCanonicalProviderKey(t *testing.T) {
	output := runInitTestConfig(t, "backend")
	b, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, `provider = "docker"`) || strings.Contains(text, `backend = "docker"`) {
		t.Fatalf("generated config did not canonicalize backend to provider:\n%s", text)
	}
	assertGeneratedConfigValid(t, output)
}

func runInitTestConfig(t *testing.T, providerFlag string) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), "config.toml")
	runInitWithOutput(t, providerFlag, output)
	return output
}

func runInitWithOutput(t *testing.T, providerFlag, output string) {
	t.Helper()
	c := &cobra.Command{}
	f := c.Flags()
	f.String("output", "", "")
	f.String("url", "", "")
	f.String("name", "", "")
	f.String("token", "", "")
	f.Int("max-runners", 1, "")
	f.String("provider", "", "")
	f.String("backend", "", "")
	f.String("runner-image", "", "")
	f.Bool("dind", false, "")
	f.String("shared-volume", "", "")
	values := map[string]string{
		"url": "https://github.com/test-org", "name": "test-runners",
		"token": "fake-token", "max-runners": "1", providerFlag: "docker",
		"runner-image": "test-image", "dind": "false", "shared-volume": "",
	}
	if output != "" {
		values["output"] = output
	}
	for name, value := range values {
		if err := f.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := runInit(c, nil); err != nil {
		t.Fatal(err)
	}
}

func assertGeneratedConfigValid(t *testing.T, output string) {
	t.Helper()
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
	if cfg.Concurrent != sets[0].MaxRunners {
		t.Fatalf("generated concurrent = %d, want single scale-set max %d", cfg.Concurrent, sets[0].MaxRunners)
	}
}

func TestInitDefaultsToUserConfigPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes /etc/runner/config.toml")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Chdir(t.TempDir())

	runInitWithOutput(t, "provider", "")

	path := filepath.Join(home, ".config", "runner", "config.toml")
	assertGeneratedConfigValid(t, path)
	assertFileMode(t, path, 0o600)
	assertFileMode(t, filepath.Dir(path), 0o700)
}

func TestInitRewritesExistingConfigPrivately(t *testing.T) {
	output := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(output, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := reader
	t.Cleanup(func() { reader = saved })
	reader = bufio.NewReader(strings.NewReader("y\n")) // confirm the overwrite prompt

	runInitWithOutput(t, "provider", output)

	assertFileMode(t, output, 0o600)
	assertGeneratedConfigValid(t, output)
}

func TestInitShadowWarning(t *testing.T) {
	t.Chdir(t.TempDir())
	central := filepath.Join(t.TempDir(), "config.toml")
	if got := initShadowWarning(central); got != "" {
		t.Fatalf("no local config but warning %q", got)
	}
	if err := os.WriteFile("config.toml", []byte("name = 'local'"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := initShadowWarning(central); !strings.Contains(got, central) || !strings.Contains(got, "config.toml") {
		t.Fatalf("shadow warning = %q", got)
	}
	if got := initShadowWarning("config.toml"); got != "" {
		t.Fatalf("writing the local file itself warned: %q", got)
	}
}
