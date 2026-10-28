package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

func TestRunAppFlagsReachResolvedConfiguration(t *testing.T) {
	flags := pflag.NewFlagSet("app", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	registerGitHubAppFlags(flags)
	v := viper.New()
	if err := flags.Parse([]string{"--github-app-client-id=Iv1.synthetic", "--github-app-installation-id=123", "--github-app-private-key-env=APP_KEY"}); err != nil {
		t.Fatal(err)
	}
	bindGitHubAppFlags(v, flags)
	cfg, err := config.Load(v)
	if err != nil {
		t.Fatal(err)
	}
	ss := cfg.ResolveScaleSets()[0]
	if ss.GitHubApp.ClientID != "Iv1.synthetic" || ss.GitHubApp.InstallationID != 123 || ss.GitHubApp.PrivateKeyEnv != "APP_KEY" || ss.Token != "" {
		t.Fatal("run flags did not select the configured App credentials")
	}
}

func TestLoadConfigCombinesAppFlagsWithConfigFile(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
url = "https://github.com/example-org"
name = "builders"
github-app.client-id = "Iv1.config"
github-app.installation-id = 123
github-app.private-key-env = "APP_KEY"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &cobra.Command{}
	c.Flags().String("config", path, "")
	registerGitHubAppFlags(c.Flags())
	if err := c.Flags().Set("github-app-client-id", "Iv1.flag"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	ss := cfg.ResolveScaleSets()[0]
	if ss.GitHubApp == nil || ss.GitHubApp.ClientID != "Iv1.flag" || ss.GitHubApp.InstallationID != 123 || ss.GitHubApp.PrivateKeyEnv != "APP_KEY" {
		t.Fatal("App CLI override lost other config file credential references")
	}
}

func TestUnusedAppFlagsDoNotWarnOrSelectAuthForMultipleScaleSets(t *testing.T) {
	flags := pflag.NewFlagSet("app", pflag.ContinueOnError)
	registerGitHubAppFlags(flags)
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(strings.NewReader(`
[[scaleset]]
url = "https://github.com/example-org"
name = "builders"
token = "synthetic-pat"
`)); err != nil {
		t.Fatal(err)
	}
	bindGitHubAppFlags(v, flags)
	cfg, err := config.Load(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("unused App flags produced warnings: %v", cfg.Warnings)
	}
	ss := cfg.ResolveScaleSets()[0]
	if ss.GitHubApp != nil || ss.Token != "synthetic-pat" {
		t.Fatal("unused App flags changed PAT configuration")
	}
}
