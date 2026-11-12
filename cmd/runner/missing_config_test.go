package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

func TestConfigMissingOnlyWithoutAFileOrAScaleSet(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	if !configMissing(config.Config{}) {
		t.Fatal("an empty config without a file was not reported missing")
	}
	flags := viper.New()
	flags.Set("url", "https://github.com/example")
	flags.Set("name", "flags")
	flags.Set("token", "synthetic")
	cfg, err := config.Load(flags)
	if err != nil {
		t.Fatal(err)
	}
	if configMissing(cfg) {
		t.Fatal("a scale set configured without a file was reported missing")
	}
	viper.SetConfigFile("/srv/runner/config.toml")
	if configMissing(config.Config{}) {
		t.Fatal("a config file was reported missing")
	}
}

// withoutConfigFiles makes every place runner looks for config.toml empty,
// except /etc/runner, which a test cannot redirect.
func withoutConfigFiles(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(defaultConfigPath); err == nil {
		t.Skipf("%s exists on this host", defaultConfigPath)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Chdir(home)
	viper.Reset()
	t.Cleanup(viper.Reset)
	return home
}

func TestValidateWithoutAConfigFileSaysWhereItLooked(t *testing.T) {
	home := withoutConfigFiles(t)
	c := &cobra.Command{Use: "validate"}
	c.Flags().String("config", "", "")
	c.SetContext(context.Background())

	var err error
	captureStdout(t, func() { err = runValidate(c, nil) })
	if err == nil {
		t.Fatal("validate passed without any config")
	}
	for _, want := range []string{"no config file found", "./config.toml", filepath.Join(home, "xdg", "runner", "config.toml"), defaultConfigPath, "https://ysya.github.io/runscaler/#configure", "--config PATH"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q:\n%s", want, err)
		}
	}
}

func TestDoctorSkipsGitHubWithoutAConfigFile(t *testing.T) {
	withoutConfigFiles(t)

	var err error
	out := captureStdout(t, func() { err = doctorGitHubCheck(context.Background(), &config.Config{}) })
	if err != nil {
		t.Fatalf("a missing config failed the GitHub check instead of skipping it: %v", err)
	}
	for _, want := range []string{"GitHub API: no config file found (skipping)", "./config.toml", "https://ysya.github.io/runscaler/#configure"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}
