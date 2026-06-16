package main

import "testing"

func TestRootHasNoRunE(t *testing.T) {
	// Bare `runner` must print help, not start scaling. cobra prints usage
	// for a command with neither Run nor RunE.
	if cmd.RunE != nil || cmd.Run != nil {
		t.Error("root command must not have Run/RunE so bare `runner` prints help")
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
