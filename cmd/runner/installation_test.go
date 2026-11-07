package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestSelectInstallationUsesInstalledScopeNotRuntimeUser(t *testing.T) {
	system := &installationTarget{Scope: scopeSystem, RuntimeUser: "runscaler", BinaryPath: "/usr/local/bin/runner", ConfigPath: "/etc/runner/config.toml"}
	got, err := selectInstallation([]installationCandidate{{Scope: scopeSystem, Target: system}, {Scope: scopeUser}}, scopeAuto)
	if err != nil || got != system || got.Scope != scopeSystem {
		t.Fatalf("target = %#v, %v; want system installation with non-root runtime", got, err)
	}
}

func TestSelectInstallationRejectsAmbiguousAndUnknownTargets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates []installationCandidate
	}{
		{"duplicate", []installationCandidate{{Scope: scopeSystem, Target: &installationTarget{Scope: scopeSystem}}, {Scope: scopeUser, Target: &installationTarget{Scope: scopeUser}}}},
		{"unknown", []installationCandidate{{Scope: scopeSystem, Err: errors.New("access denied")}, {Scope: scopeUser, Target: &installationTarget{Scope: scopeUser}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := selectInstallation(tc.candidates, scopeAuto); err == nil {
				t.Fatal("ambiguous or unknown scope was silently selected")
			}
		})
	}
}

func TestSelectInstallationExplicitScopeDoesNotFallBack(t *testing.T) {
	user := &installationTarget{Scope: scopeUser, BinaryPath: "/home/ci/.local/bin/runner"}
	got, err := selectInstallation([]installationCandidate{{Scope: scopeSystem, Err: errors.New("no system bus")}, {Scope: scopeUser, Target: user}}, scopeUser)
	if err != nil || got != user {
		t.Fatalf("explicit user target = %#v, %v", got, err)
	}
	got, err = selectInstallation([]installationCandidate{{Scope: scopeSystem}, {Scope: scopeUser, Target: user}}, scopeSystem)
	if err != nil || got != nil {
		t.Fatalf("missing explicit system target = %#v, %v; must not use user target", got, err)
	}
}

func TestRequestedScopePreservesUserFlagAndRejectsConflicts(t *testing.T) {
	c := &cobra.Command{}
	addInstallationFlags(c)
	if err := c.Flags().Set("user", "true"); err != nil {
		t.Fatal(err)
	}
	if got, err := requestedScope(c); err != nil || got != scopeUser {
		t.Fatalf("scope = %q, %v", got, err)
	}
	if err := c.Flags().Set("scope", "system"); err != nil {
		t.Fatal(err)
	}
	if _, err := requestedScope(c); err == nil {
		t.Fatal("contradictory scope flags accepted")
	}
}

func TestTargetFromArgsPreservesQuotedPathsAndRejectsAmbiguousConfig(t *testing.T) {
	got, err := targetFromArgs(scopeSystem, "/opt/runner tools/runner", []string{"/opt/runner tools/runner", "run", "--config", "/etc/runner tools/config.toml"})
	if err != nil || got.ConfigPath != "/etc/runner tools/config.toml" {
		t.Fatalf("target = %#v, %v", got, err)
	}
	for _, args := range [][]string{
		{"runner", "run", "--config", "/a", "--config=/b"},
		{"runner", "run"},
		{"runner", "run", "--config", "relative.toml"},
	} {
		if _, err := targetFromArgs(scopeSystem, "/usr/local/bin/runner", args); err == nil {
			t.Fatalf("unsafe argv accepted: %q", args)
		}
	}
}

func TestParseSystemdPropertiesUsesStructuredArguments(t *testing.T) {
	data := []byte(`{"ExecStart":{"type":"a(sasbttttuii)","data":[["/opt/runner tools/runner",["/opt/runner tools/runner","run","--config","/etc/runner tools/config.toml"],false,0,0,0,0,0,0,0]]},"User":{"type":"s","data":"runscaler"},"Group":{"type":"s","data":"runscaler"},"Environment":{"type":"as","data":["RUNNER_SERVICE_SCOPE=system","HOME=/var/lib/runner"]}}`)
	got, err := parseSystemdProperties(scopeSystem, data)
	if err != nil || got.RuntimeUser != "runscaler" || got.ConfigPath != "/etc/runner tools/config.toml" {
		t.Fatalf("target = %#v, %v", got, err)
	}
	if got.Environment["HOME"] != "/var/lib/runner" {
		t.Fatalf("environment = %#v", got.Environment)
	}
}

func TestSystemdRawArgumentsRespectExpansionFlags(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		flags           []string
		fail            bool
	}{
		{"template literal", "/etc/runner/$$NAME.toml", "/etc/runner/$NAME.toml", nil, false},
		{"multiple literals", "/etc/runner/$$$$NAME.toml", "/etc/runner/$$NAME.toml", nil, false},
		{"explicit no expansion", "/etc/runner/$$NAME.toml", "/etc/runner/$$NAME.toml", []string{"no-env-expand"}, false},
		{"unknown variable", "/etc/runner/${NAME}.toml", "", nil, true},
		{"privilege prefix", "/etc/runner/config.toml", "", []string{"privileged"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := []string{"/usr/local/bin/runner", "run", "--config", tc.raw}
			props := map[string]any{
				"ExecStart":   map[string]any{"type": "a(sasbttttuii)", "data": [][]any{{argv[0], argv, false, 0, 0, 0, 0, 0, 0, 0}}},
				"ExecStartEx": map[string]any{"type": "a(sasasttttuii)", "data": [][]any{{argv[0], argv, tc.flags, 0, 0, 0, 0, 0, 0, 0}}},
			}
			data, err := json.Marshal(props)
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseSystemdProperties(scopeSystem, data)
			if tc.fail {
				if err == nil {
					t.Fatalf("ambiguous/privileged raw argv accepted: %+v", got)
				}
				return
			}
			if err != nil || got.ConfigPath != tc.want {
				t.Fatalf("config=%+v err=%v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestParseLaunchdTargetDecodesXMLAndRejectsInvalidArguments(t *testing.T) {
	data := []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>Label</key><string>io.github.ysya.runner</string><key>ProgramArguments</key><array><string>/Users/ci/runner tools/runner</string><string>run</string><string>--config</string><string>/Users/ci/config &amp; keys/config.toml</string></array><key>EnvironmentVariables</key><dict><key>XDG_STATE_HOME</key><string>/Users/ci/state</string></dict></dict></plist>`)
	got, err := parseLaunchdTarget(scopeUser, data)
	if err != nil || got.ConfigPath != "/Users/ci/config & keys/config.toml" || got.Environment["XDG_STATE_HOME"] != "/Users/ci/state" {
		t.Fatalf("target = %#v, %v", got, err)
	}
	if _, err := parseLaunchdTarget(scopeUser, []byte(strings.ReplaceAll(string(data), "<string>run</string>", "<string>update</string>"))); err == nil {
		t.Fatal("non-manager launch target accepted")
	}
}

func TestManagedConfigUsesInstalledConfigInsteadOfCurrentDirectory(t *testing.T) {
	saved := discoverInstallations
	t.Cleanup(func() { discoverInstallations = saved; viper.Reset() })
	dir := t.TempDir()
	installed := filepath.Join(dir, "installed.toml")
	if err := os.WriteFile(installed, []byte("name='installed-target'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	t.Chdir(local)
	if err := os.WriteFile("config.toml", []byte("name='wrong-local-config'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	discoverInstallations = func(context.Context) []installationCandidate {
		return []installationCandidate{{Scope: scopeUser, Target: &installationTarget{Scope: scopeUser, BinaryPath: "/home/ci/.local/bin/runner", ConfigPath: installed, Environment: map[string]string{"XDG_STATE_HOME": "/installed/state"}}}}
	}
	c := &cobra.Command{Use: "status"}
	c.Flags().String("config", "", "")
	addInstallationFlags(c)
	viper.Reset()
	cfg, err := loadConfig(c)
	if err != nil || cfg.ResolveScaleSets()[0].ScaleSetName != "installed-target" {
		t.Fatalf("managed config chose another installation: %v, %v", cfg.ResolveScaleSets(), err)
	}
	id := commandIdentity(c)
	if id.ServiceScope != "user" || id.XDGStateHome != "/installed/state" {
		t.Fatalf("managed identity=%+v", id)
	}
}

func TestManagedConfigRejectsMutationAgainstDifferentConfig(t *testing.T) {
	saved := discoverInstallations
	t.Cleanup(func() { discoverInstallations = saved; viper.Reset() })
	discoverInstallations = func(context.Context) []installationCandidate {
		return []installationCandidate{{Scope: scopeUser, Target: &installationTarget{Scope: scopeUser, BinaryPath: "/home/ci/.local/bin/runner", ConfigPath: "/installed/config.toml"}}}
	}
	c := &cobra.Command{Use: "update"}
	c.Flags().String("config", "", "")
	addInstallationFlags(c)
	if err := c.Flags().Set("config", "/different/config.toml"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(c); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("config conflict not reported: %v", err)
	}
}

func TestServiceScopeSelectionRejectsOppositeInstallation(t *testing.T) {
	saved := discoverInstallations
	t.Cleanup(func() { discoverInstallations = saved })
	discoverInstallations = func(context.Context) []installationCandidate {
		return []installationCandidate{{Scope: scopeSystem, Target: &installationTarget{Scope: scopeSystem, RuntimeUser: "runscaler", ConfigPath: "/etc/runner/config.toml"}}, {Scope: scopeUser}}
	}
	c := &cobra.Command{Use: "install"}
	addInstallationFlags(c)
	if err := c.Flags().Set("user", "true"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := serviceCommandInstallation(c, true); err == nil {
		t.Fatal("second installation allowed in opposite scope")
	}
	c = &cobra.Command{Use: "restart"}
	addInstallationFlags(c)
	user, target, err := serviceCommandInstallation(c, false)
	if err != nil || user || target.RuntimeUser != "runscaler" {
		t.Fatalf("system/non-root target misclassified: user=%v target=%+v err=%v", user, target, err)
	}
}

func TestExplicitMissingInstallationDoesNotFallBackToStandalone(t *testing.T) {
	saved := discoverInstallations
	t.Cleanup(func() { discoverInstallations = saved })
	discoverInstallations = func(context.Context) []installationCandidate { return nil }
	c := &cobra.Command{Use: "status"}
	addInstallationFlags(c)
	if err := c.Flags().Set("scope", "system"); err != nil {
		t.Fatal(err)
	}
	if _, err := commandInstallation(c); err == nil {
		t.Fatal("explicit missing system target fell back to local configuration")
	}
}

func TestFreshSystemPreparationDoctorDoesNotRequireAnInstalledUnit(t *testing.T) {
	t.Setenv("RUNNER_SERVICE_PREPARATION", "1")
	t.Setenv("RUNNER_MAINTENANCE_CONTEXT", "1")
	t.Setenv("RUNNER_SERVICE_SCOPE", "system")
	saved := discoverInstallations
	t.Cleanup(func() { discoverInstallations = saved })
	discoverInstallations = func(context.Context) []installationCandidate {
		t.Error("pre-unit probe attempted installed-unit discovery")
		return nil
	}
	c := &cobra.Command{Use: "doctor"}
	c.Flags().Bool("fix", false, "")
	addInstallationFlags(c)
	if err := c.Flags().Set("scope", "system"); err != nil {
		t.Fatal(err)
	}
	got, err := managedConfigPath(c, "/etc/runner/config.toml")
	if err != nil || got != "/etc/runner/config.toml" {
		t.Fatalf("fresh preparation config=%q, err=%v", got, err)
	}
	if err := c.Flags().Set("fix", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := managedConfigPath(c, "/etc/runner/config.toml"); err == nil {
		t.Fatal("preparation context allowed cleanup")
	}
}
