package main

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestServiceCommandNamesScopeAndNeedsSudoOnlyForSystem(t *testing.T) {
	if got, want := serviceCommand("/usr/local/bin/runner", scopeSystem, "restart"), "sudo /usr/local/bin/runner service restart --scope system"; got != want {
		t.Errorf("system = %q, want %q", got, want)
	}
	if got, want := serviceCommand("/Users/ada/my runner/runner", scopeUser, "status"), "'/Users/ada/my runner/runner' service status --scope user"; got != want {
		t.Errorf("user = %q, want %q", got, want)
	}
}

func TestServiceInstallCommandCarriesEveryInstallInput(t *testing.T) {
	got := serviceInstallCommand("/usr/local/bin/runner", scopeSystem, "/etc/my runner/config.toml", true, true, []string{"/etc/runner/credentials.env"})
	want := "sudo /usr/local/bin/runner service install --scope system --force --no-start --config-path '/etc/my runner/config.toml' --binary-path /usr/local/bin/runner --environment-file /etc/runner/credentials.env"
	if got != want {
		t.Errorf("system = %q\nwant %q", got, want)
	}
	got = serviceInstallCommand("/Users/ada/.local/bin/runner", scopeUser, "", true, false, nil)
	want = "/Users/ada/.local/bin/runner service install --scope user --force --binary-path /Users/ada/.local/bin/runner"
	if got != want {
		t.Errorf("user = %q\nwant %q", got, want)
	}
}

func TestInstallationFlagsAreRegisteredOnceWithAutoScope(t *testing.T) {
	for _, c := range []*cobra.Command{serviceInstallCmd, serviceUninstallCmd, serviceStartCmd, serviceStopCmd, serviceRestartCmd, serviceStatusCmd, serviceLogsCmd, statusCmd, logsCmd, updateCmd, doctorCmd} {
		scope := c.Flags().Lookup("scope")
		user := c.Flags().Lookup("user")
		if scope == nil || user == nil {
			t.Fatalf("%s lacks the installation flags", c.CommandPath())
		}
		if scope.DefValue != "auto" || user.DefValue != "false" {
			t.Errorf("%s defaults: scope=%q user=%q; want auto/false", c.CommandPath(), scope.DefValue, user.DefValue)
		}
		if !strings.Contains(user.Usage, "--scope user") {
			t.Errorf("%s --user help does not present it as an alias: %q", c.CommandPath(), user.Usage)
		}
	}
	want := scopeSystem
	if runtime.GOOS == "darwin" {
		want = scopeUser
	}
	if got := defaultScope(); got != want {
		t.Errorf("defaultScope() = %q, want %q", got, want)
	}
}

func TestServiceCommandInstallationReturnsTheResolvedScopeWithoutTouchingFlags(t *testing.T) {
	saved := discoverInstallations
	t.Cleanup(func() { discoverInstallations = saved })
	discoverInstallations = func(context.Context) []installationCandidate {
		return []installationCandidate{{Scope: scopeSystem}, {Scope: scopeUser, Target: &installationTarget{Scope: scopeUser, BinaryPath: "/home/ci/.local/bin/runner", ConfigPath: "/home/ci/.config/runner/config.toml"}}}
	}
	c := &cobra.Command{Use: "stop"}
	addInstallationFlags(c)
	scope, target, err := serviceCommandInstallation(c, false)
	if err != nil || scope != scopeUser || target == nil {
		t.Fatalf("auto selection = %q, %+v, %v", scope, target, err)
	}
	if got := c.Flags().Lookup("user").Value.String(); got != "false" {
		t.Fatalf("resolver wrote the scope back into --user (%s)", got)
	}
}

func TestRefuseDarwinSystemScopeOnlyBlocksMutations(t *testing.T) {
	if err := refuseDarwinSystemScope("darwin", scopeSystem, "install the runner service"); err == nil || !strings.Contains(err.Error(), "LaunchAgent") {
		t.Errorf("darwin system install = %v", err)
	}
	for _, tc := range []struct {
		goos  string
		scope serviceScope
	}{{"darwin", scopeUser}, {"linux", scopeSystem}} {
		if err := refuseDarwinSystemScope(tc.goos, tc.scope, "start the runner service"); err != nil {
			t.Errorf("%s %s = %v, want nil", tc.goos, tc.scope, err)
		}
	}
}

func TestCheckCommandPrivilegesHintForwardsFlagsAndOffersUserScope(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	c := &cobra.Command{Use: "install"}
	c.Flags().String("config-path", "", "")
	c.Flags().String("binary-path", "", "")
	c.Flags().String("environment-file", "", "")
	c.Flags().Bool("force", false, "")
	c.Flags().Bool("no-start", false, "")
	for name, value := range map[string]string{"config-path": "/etc/my runner/config.toml", "force": "true"} {
		if err := c.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	err := checkCommandPrivileges(c, scopeSystem)
	if err == nil {
		t.Fatal("system scope as a user passed")
	}
	for _, want := range []string{"sudo ", " service install --scope system --config-path '/etc/my runner/config.toml' --force", "service install --scope user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("hint lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "--user") {
		t.Errorf("hint still uses the --user spelling:\n%v", err)
	}
	if err := checkCommandPrivileges(c, scopeUser); err != nil {
		t.Errorf("user scope as a user = %v", err)
	}
}

func TestStatusPrivilegeErrorNamesTheBinaryAndScope(t *testing.T) {
	err := statusPrivilegeError("darwin", scopeSystem, 501, "/opt/my runner/runner")
	if err == nil || !strings.Contains(err.Error(), "sudo '/opt/my runner/runner' service status --scope system") {
		t.Errorf("macOS system status as a user = %v", err)
	}
	for _, tc := range []struct {
		goos  string
		scope serviceScope
		euid  int
	}{{"darwin", scopeUser, 501}, {"darwin", scopeSystem, 0}, {"linux", scopeSystem, 1000}} {
		if err := statusPrivilegeError(tc.goos, tc.scope, tc.euid, "runner"); err != nil {
			t.Errorf("statusPrivilegeError(%+v) = %v, want nil", tc, err)
		}
	}
}

func TestLaunchdSystemTargetRunsAsRootByDefault(t *testing.T) {
	data := []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>Label</key><string>io.github.ysya.runner</string><key>ProgramArguments</key><array><string>/usr/local/bin/runner</string><string>run</string><string>--config</string><string>/etc/runner/config.toml</string></array></dict></plist>`)
	got, err := parseLaunchdTarget(scopeSystem, data)
	if err != nil || got.RuntimeUser != "root" {
		t.Fatalf("system daemon identity = %+v, %v; want root", got, err)
	}
	got, err = parseLaunchdTarget(scopeUser, data)
	if err != nil || got.RuntimeUser != "" {
		t.Fatalf("user agent identity = %+v, %v; want the current user", got, err)
	}
}

func TestExplicitConfigConflict(t *testing.T) {
	target := &installationTarget{Scope: scopeUser, ConfigPath: "/installed/config.toml"}
	if err := explicitConfigConflict("", target); err != nil {
		t.Errorf("no explicit config = %v", err)
	}
	if err := explicitConfigConflict("/installed/../installed/config.toml", target); err != nil {
		t.Errorf("same file spelled differently = %v", err)
	}
	if err := explicitConfigConflict("/different/config.toml", target); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("different file = %v", err)
	}
	if err := explicitConfigConflict("/different/config.toml", nil); err != nil {
		t.Errorf("no installation = %v", err)
	}
}
