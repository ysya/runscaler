package main

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// statusCommand builds `runner status` with configFile as the config viper
// read and target as the selected installation, either of which may be empty.
func statusCommand(t *testing.T, configFile string, target *installationTarget) *cobra.Command {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	if configFile != "" {
		viper.SetConfigFile(configFile)
	}
	c := &cobra.Command{Use: "status"}
	c.Flags().Int("health-port", 8080, "")
	ctx := context.Background()
	if target != nil {
		ctx = context.WithValue(ctx, installationContextKey{}, target)
	}
	c.SetContext(ctx)
	return c
}

func TestStatusUnreachableServicePointsAtTheService(t *testing.T) {
	target := &installationTarget{Scope: scopeUser, BinaryPath: "/home/ci/.local/bin/runner", ConfigPath: "/home/ci/config.toml"}
	err := statusUnreachable(statusCommand(t, "/home/ci/config.toml", target), "127.0.0.1", 9090)
	for _, want := range []string{
		"cannot reach runner's health endpoint at 127.0.0.1:9090 (port from /home/ci/config.toml)",
		"The user service may be stopped or still starting",
		"/home/ci/.local/bin/runner service status --scope user",
		"/home/ci/.local/bin/runner service logs --scope user",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), " run --config") {
		t.Fatalf("an installed service was told to start a foreground runner:\n%s", err)
	}
}

func TestStatusUnreachableWithoutServiceSuggestsStartingRunner(t *testing.T) {
	err := statusUnreachable(statusCommand(t, "", nil), "127.0.0.1", 8080)
	for _, want := range []string{"at 127.0.0.1:8080 (default port; no config file was found)", "Is runner running?", " run --config config.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), "service status") || strings.Count(err.Error(), "--health-port") > 0 {
		t.Fatalf("hint is not for a foreground runner:\n%s", err)
	}

	err = statusUnreachable(statusCommand(t, "/srv/runner/config.toml", nil), "127.0.0.1", 8080)
	if !strings.Contains(err.Error(), "(port from /srv/runner/config.toml)") || !strings.Contains(err.Error(), " run --config /srv/runner/config.toml") {
		t.Fatalf("config-backed hint:\n%s", err)
	}
}

func TestStatusUnreachableNamesAnExplicitPort(t *testing.T) {
	c := statusCommand(t, "/srv/runner/config.toml", nil)
	if err := c.Flags().Set("health-port", "9100"); err != nil {
		t.Fatal(err)
	}
	if err := statusUnreachable(c, "127.0.0.1", 9100); !strings.Contains(err.Error(), "at 127.0.0.1:9100 (port from --health-port)") {
		t.Fatalf("explicit port hint:\n%s", err)
	}
}
