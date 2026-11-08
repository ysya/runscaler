package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Discovery and the service managers must agree on every path a definition
// can live at, or a unit discovery manages becomes one uninstall cannot find.
func TestDefinitionPathsMatchWhatTheManagersWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	system := []string{"/etc/systemd/system/runner.service", "/usr/lib/systemd/system/runner.service", "/lib/systemd/system/runner.service"}
	if got := systemdDefinitionPaths(scopeSystem); !slices.Equal(got, system) {
		t.Errorf("system unit paths = %q, want %q", got, system)
	}
	user := []string{filepath.Join(home, ".config", "systemd", "user", "runner.service")}
	if got := systemdDefinitionPaths(scopeUser); !slices.Equal(got, user) {
		t.Errorf("user unit paths = %q, want %q", got, user)
	}
	for _, scope := range []serviceScope{scopeSystem, scopeUser} {
		if systemdDefinitionPaths(scope)[0] != systemdUnitPath(scope) {
			t.Errorf("%s: the highest-precedence path is not the one install writes", scope)
		}
	}
	if got, want := launchdPlistPath(scopeUser), filepath.Join(home, "Library", "LaunchAgents", "io.github.ysya.runner.plist"); got != want {
		t.Errorf("user plist = %q, want %q", got, want)
	}
	if got, want := launchdPlistPath(scopeSystem), "/Library/LaunchDaemons/io.github.ysya.runner.plist"; got != want {
		t.Errorf("system plist = %q, want %q", got, want)
	}
}

// A unit a package installed is managed, but runner never deletes it: the
// refusal must come before any systemctl call or file removal.
func TestUninstallRefusesADefinitionRunnerDidNotWrite(t *testing.T) {
	const packaged = "/usr/lib/systemd/system/runner.service"
	err := (&systemdManager{}).uninstall(scopeSystem, packaged)
	if err == nil || !strings.Contains(err.Error(), packaged) || !strings.Contains(err.Error(), "did not write") {
		t.Fatalf("uninstalling a package unit = %v", err)
	}
	if err := (&launchdManager{}).uninstall(scopeUser, "/opt/elsewhere/io.github.ysya.runner.plist"); err == nil {
		t.Fatal("uninstalling a plist runner did not write succeeded")
	}
	if err := ownedDefinition("", "/etc/systemd/system/runner.service"); err != nil {
		t.Errorf("an unknown definition = %v, want the managed path assumed", err)
	}
	if err := ownedDefinition("/etc/systemd/system/runner.service", "/etc/systemd/system/runner.service"); err != nil {
		t.Errorf("runner's own unit = %v", err)
	}
}

func TestPackageUnitIsReportedOnlyForSystemScope(t *testing.T) {
	present := func(paths ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(paths, p) }
	}
	if got := packageUnit(scopeSystem, present("/lib/systemd/system/runner.service")); got != "/lib/systemd/system/runner.service" {
		t.Errorf("package unit under /lib = %q", got)
	}
	if got := packageUnit(scopeSystem, present("/usr/lib/systemd/system/runner.service", "/lib/systemd/system/runner.service")); got != "/usr/lib/systemd/system/runner.service" {
		t.Errorf("both locations = %q, want the one systemd loads first", got)
	}
	if got := packageUnit(scopeSystem, present()); got != "" {
		t.Errorf("no package unit = %q", got)
	}
	if got := packageUnit(scopeUser, present("/usr/lib/systemd/system/runner.service")); got != "" {
		t.Errorf("user scope = %q; package units are system units", got)
	}
}
