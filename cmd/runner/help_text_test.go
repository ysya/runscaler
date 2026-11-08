package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestUserAliasIsHiddenFromHelp(t *testing.T) {
	for _, c := range []*cobra.Command{serviceInstallCmd, serviceRestartCmd, statusCmd, updateCmd, doctorCmd} {
		if flag := c.Flags().Lookup("user"); flag == nil || !flag.Hidden {
			t.Errorf("%s shows the --user alias in its help", c.CommandPath())
		}
		var out bytes.Buffer
		c.SetOut(&out)
		if err := c.Help(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "--user ") {
			t.Errorf("%s help lists --user:\n%s", c.CommandPath(), out.String())
		}
		c.SetOut(nil)
	}
}

func TestHelpTextNamesNoInternalFunctions(t *testing.T) {
	if strings.Contains(cacheCmd.Long, "Measure()") {
		t.Errorf("cache help names an internal function:\n%s", cacheCmd.Long)
	}
}

func TestServiceInstallHelpExplainsScopesWithExamples(t *testing.T) {
	for _, want := range []string{"--scope user", "LaunchAgent", "sudo"} {
		if !strings.Contains(serviceInstallCmd.Long, want) {
			t.Errorf("install help lacks %q:\n%s", want, serviceInstallCmd.Long)
		}
	}
	for _, want := range []string{"sudo runner service install", "runner service install --scope user"} {
		if !strings.Contains(serviceInstallCmd.Example, want) {
			t.Errorf("install examples lack %q:\n%s", want, serviceInstallCmd.Example)
		}
	}
}

func TestMissingServiceSaysHowToInstallOne(t *testing.T) {
	withInstallations(t, []installationCandidate{{Scope: scopeSystem}, {Scope: scopeUser}})
	c := installationCommand(t, "status")
	_, _, err := serviceCommandInstallation(c, false)
	if err == nil || !strings.Contains(err.Error(), "service not installed") {
		t.Fatalf("err = %v; want service not installed", err)
	}
	if want := serviceCommand(hintBinary(c), defaultScope(), "install"); !strings.Contains(err.Error(), want) {
		t.Fatalf("error lacks the install command %q:\n%s", want, err)
	}
	if defaultScope() == scopeSystem && !strings.Contains(err.Error(), serviceCommand(hintBinary(c), scopeUser, "install")) {
		t.Fatalf("error lacks the user install command:\n%s", err)
	}
}

func TestMissingServiceInRequestedScopeNamesTheInstalledOne(t *testing.T) {
	installed := &installationTarget{Scope: scopeSystem, BinaryPath: "/usr/local/bin/runner", ConfigPath: "/etc/runner/config.toml"}
	withInstallations(t, []installationCandidate{{Scope: scopeSystem, Target: installed}, {Scope: scopeUser}})
	c := installationCommand(t, "restart")
	if err := c.Flags().Set("scope", "user"); err != nil {
		t.Fatal(err)
	}
	c.SetContext(context.Background())
	_, _, err := serviceCommandInstallation(c, false)
	if err == nil || !strings.Contains(err.Error(), "select it with --scope system") || strings.Contains(err.Error(), "Install it with") {
		t.Fatalf("err = %v; want a pointer to the system installation", err)
	}
}
