package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestRefuseDarwinRoot(t *testing.T) {
	none := func(string) bool { return false }
	if err := refuseDarwinRoot("linux", 0, "run runner", none); err != nil {
		t.Errorf("linux root refused: %v", err)
	}
	if err := refuseDarwinRoot("darwin", 501, "run runner", none); err != nil {
		t.Errorf("macOS user refused: %v", err)
	}
	err := refuseDarwinRoot("darwin", 0, "run runner", none)
	if err == nil || !strings.Contains(err.Error(), "LaunchAgent") || strings.Contains(err.Error(), "Found system service") {
		t.Errorf("macOS root without daemons = %v", err)
	}
	legacy := "/Library/LaunchDaemons/com.runscaler.agent.plist"
	err = refuseDarwinRoot("darwin", 0, "run runner", func(p string) bool { return p == legacy })
	if err == nil || !strings.Contains(err.Error(), legacy) {
		t.Errorf("macOS root with a legacy daemon = %v, want it named", err)
	}
}

func TestStartManagerSilencesUsageBeforeLoadingConfig(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	c := &cobra.Command{Use: "run"}
	c.Flags().String("config", "", "")
	if err := c.Flags().Set("config", "/nonexistent/runner/config.toml"); err != nil {
		t.Fatal(err)
	}
	if err := startManager(c); err == nil {
		t.Fatal("startManager() with a missing config succeeded")
	}
	if !c.SilenceUsage {
		t.Fatal("a runtime failure would still print the full usage")
	}
}
