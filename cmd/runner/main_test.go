package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRootBareInvocationDoesNotStart(t *testing.T) {
	called := false
	orig := startScaling
	startScaling = func(c *cobra.Command) error { called = true; return nil }
	defer func() { startScaling = orig }()
	defer func() {
		if f := cmd.PersistentFlags().Lookup("config"); f != nil {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	}()

	cmd.SetArgs([]string{})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	defer cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if called {
		t.Error("bare `runner` must print help, not start scaling")
	}
}

func TestRootConfigInvocationStartsViaDropIn(t *testing.T) {
	called := false
	orig := startScaling
	startScaling = func(c *cobra.Command) error { called = true; return nil }
	defer func() { startScaling = orig }()
	defer func() {
		if f := cmd.PersistentFlags().Lookup("config"); f != nil {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	}()

	cmd.SetArgs([]string{"--config", "/nonexistent/x.toml"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	defer cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !called {
		t.Error("`runner --config X` must start via drop-in compat")
	}
}

func TestRunSubcommandRegistered(t *testing.T) {
	found := false
	for _, c := range cmd.Commands() {
		if c.Name() == "run" {
			found = true
			break
		}
	}
	if !found {
		t.Error("`run` subcommand must be registered on root")
	}
}

func TestRunOwnsStartFlags(t *testing.T) {
	for _, name := range []string{"url", "name", "token", "max-runners", "backend", "health-port", "dry-run"} {
		if runCommand.Flags().Lookup(name) == nil {
			t.Errorf("`run` must own the --%s start flag", name)
		}
	}
}

func TestRootNoLongerOwnsStartFlags(t *testing.T) {
	if cmd.Flags().Lookup("url") != nil {
		t.Error("root must not own --url anymore; it moved to `run`")
	}
}

func TestConfigStaysPersistentOnRoot(t *testing.T) {
	if cmd.PersistentFlags().Lookup("config") == nil {
		t.Error("--config must remain a persistent flag on root")
	}
}
