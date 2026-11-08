package main

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestMaintenanceReproducesUnitGroupsAndScopeWithoutChangingAccounts(t *testing.T) {
	target := &installationTarget{Scope: scopeSystem, RuntimeUser: "runscaler", RuntimeGroup: "runscaler", SupplementaryGroups: []string{"998"}, BinaryPath: "/opt/runner tools/runner", ConfigPath: "/etc/runner/config.toml", WorkingDirectory: "/var/lib/runner", Environment: map[string]string{"HOME": "/var/lib/runner", "RUNNER_SERVICE_SCOPE": "system"}, EnvironmentFiles: []string{"/etc/runner/credentials.env"}}
	args, err := buildMaintenanceArgs(target, []string{"doctor"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--property=User=runscaler", "--property=Group=runscaler", "--property=SupplementaryGroups=998", "--setenv=HOME=/var/lib/runner", "--setenv=RUNNER_SERVICE_SCOPE=system", "--property=WorkingDirectory=/var/lib/runner", "--expand-environment=no", "/opt/runner tools/runner", "doctor", "--config", "/etc/runner/config.toml"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %q in %q", want, args)
		}
	}
	joined := strings.Join(args[:slices.Index(args, "--")], " ")
	for _, forbidden := range []string{"--scope", "StateDirectory=", "LogsDirectory=", "usermod", "groupadd"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unsafe maintenance command %q", joined)
		}
	}
}

// The install probe runs `runner validate` as the prepared service identity:
// it fails when that identity cannot reach the configured Docker socket or
// GitHub, and never fails over orphaned containers the way doctor does.
func TestPreparedServiceProbeRunsValidate(t *testing.T) {
	savedOutput, savedAdmin := installationOutput, serviceAdminCommand
	t.Cleanup(func() { installationOutput = savedOutput; serviceAdminCommand = savedAdmin })
	installationOutput = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("--wait --pipe --collect --expand-environment"), nil
	}
	var probe []string
	serviceAdminCommand = func(_ context.Context, name string, args ...string) error {
		if name != "systemd-run" {
			t.Fatalf("probe ran %s", name)
		}
		probe = args
		return nil
	}
	if err := verifyPreparedService(installOpts{runtimeUser: "runscaler", runtimeGroup: "runscaler", binaryPath: "/usr/local/bin/runner", configPath: "/etc/runner/config.toml"}); err != nil {
		t.Fatal(err)
	}
	split := slices.Index(probe, "--")
	if split < 0 {
		t.Fatalf("probe has no command: %q", probe)
	}
	command := probe[split+1:]
	if !slices.Equal(command, []string{"/usr/local/bin/runner", "validate", "--config", "/etc/runner/config.toml"}) {
		t.Fatalf("probe command = %q, want runner validate on the prepared config", command)
	}
	for _, arg := range probe {
		if strings.Contains(arg, "RUNNER_SERVICE_PREPARATION") {
			t.Fatalf("probe still sets the retired doctor preparation gate: %q", probe)
		}
	}
}

// validate selects no installation, so it can run before the unit exists.
func TestProbeCommandNeedsNoInstalledUnit(t *testing.T) {
	saved := discoverInstallations
	t.Cleanup(func() { discoverInstallations = saved })
	discoverInstallations = func(context.Context) []installationCandidate {
		t.Error("the install probe looked for an installed unit")
		return nil
	}
	if got, err := managedConfigPath(validateCmd, "/etc/runner/config.toml"); err != nil || got != "/etc/runner/config.toml" {
		t.Fatalf("validate config = %q, %v", got, err)
	}
}

func TestDoctorMaintenanceHintKeepsTheSystemScope(t *testing.T) {
	saved := installationOutput
	t.Cleanup(func() { installationOutput = saved })
	installationOutput = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("--wait --pipe --collect --expand-environment"), nil
	}
	c := &cobra.Command{Use: "doctor"}
	c.Flags().Bool("fix", false, "")
	target := &installationTarget{Scope: scopeSystem, RuntimeUser: "runscaler-hint-test", RuntimeGroup: "runscaler-hint-test", BinaryPath: "/usr/local/bin/runner", ConfigPath: "/etc/runner/config.toml"}
	err := requireDoctorIdentity(c, target)
	if err == nil || !strings.Contains(err.Error(), "'doctor' '--scope' 'system' '--config' '/etc/runner/config.toml'") {
		t.Fatalf("doctor hint = %v, want doctor to select the system installation", err)
	}
}

func TestMaintenanceIdentityRequiresTheUnitPrimaryGroup(t *testing.T) {
	t.Setenv("RUNNER_MAINTENANCE_CONTEXT", "1")
	t.Setenv("RUNNER_SERVICE_SCOPE", "system")
	t.Setenv("HOME", "/var/lib/runner")
	target := &installationTarget{RuntimeUser: strconv.Itoa(os.Geteuid()), RuntimeGroup: strconv.Itoa(os.Getegid() + 100), Environment: map[string]string{"HOME": "/var/lib/runner"}}
	if runtimeIdentityMatches(target) {
		t.Fatal("incorrect primary group accepted")
	}
}

func TestMaintenanceNeverCopiesPrivateEnvironmentIntoCommandLine(t *testing.T) {
	target := &installationTarget{Scope: scopeSystem, RuntimeUser: "runscaler", RuntimeGroup: "runscaler", BinaryPath: "/usr/local/bin/runner", ConfigPath: "/etc/runner/config.toml", Environment: map[string]string{"RUNNER_TOKEN": "test-private-value"}}
	args, err := buildMaintenanceArgs(target, []string{"doctor"}, true)
	if err == nil || strings.Contains(err.Error(), "test-private-value") || strings.Contains(strings.Join(args, " "), "test-private-value") {
		t.Fatalf("private environment escaped: %q, %v", args, err)
	}
}

func TestMaintenanceEnvironmentFileIsALiteralDBusProperty(t *testing.T) {
	path := "/etc/runner/credentials % literal.env"
	target := &installationTarget{Scope: scopeSystem, RuntimeUser: "runscaler", RuntimeGroup: "runscaler", BinaryPath: "/usr/local/bin/runner", ConfigPath: "/etc/runner/config.toml", EnvironmentFiles: []string{path}}
	args, err := buildMaintenanceArgs(target, []string{"doctor"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--property=EnvironmentFile="+path) {
		t.Fatalf("DBus path was unit-file escaped: %q", args)
	}
}

func TestMaintenanceRefusesRootPrimaryGroup(t *testing.T) {
	target := &installationTarget{Scope: scopeSystem, RuntimeUser: "runscaler", RuntimeGroup: "0", BinaryPath: "/usr/local/bin/runner", ConfigPath: "/etc/runner/config.toml"}
	if _, err := buildMaintenanceArgs(target, []string{"doctor"}, true); err == nil {
		t.Fatal("root primary group granted")
	}
}

func TestMaintenanceRefusesUnsafeExpansionOnOlderSystemd(t *testing.T) {
	target := &installationTarget{Scope: scopeSystem, RuntimeUser: "runscaler", RuntimeGroup: "runscaler", BinaryPath: "/usr/local/bin/runner", ConfigPath: "/etc/runner/$NAME.toml"}
	if _, err := buildMaintenanceArgs(target, []string{"doctor"}, false); err == nil {
		t.Fatal("uncontrolled systemd expansion accepted")
	}
	args, err := buildMaintenanceArgs(target, []string{"doctor"}, true)
	if err != nil || !slices.Contains(args, "/etc/runner/$NAME.toml") {
		t.Fatalf("literal path changed: %q, %v", args, err)
	}
}

func TestMaintenancePreservesCustomSystemLogAccess(t *testing.T) {
	target := &installationTarget{Scope: scopeSystem, RuntimeUser: "runscaler", RuntimeGroup: "runscaler", BinaryPath: "/usr/local/bin/runner", ConfigPath: "/etc/runner/config.toml", ReadWritePaths: []string{"/tmp", "/var/lib/runner", "/var/log/custom-runner", "/etc"}}
	args, err := buildMaintenanceArgs(target, []string{"doctor"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--property=ReadWritePaths=/tmp /var/lib/runner /var/log/custom-runner") {
		t.Fatalf("custom log directory lost: %q", args)
	}
	if strings.Contains(strings.Join(args, " "), "/var/log/runner") {
		t.Fatal("custom-log probe requires an unprepared default log directory")
	}
	if strings.Contains(strings.Join(args, " "), " /etc ") {
		t.Fatal("arbitrary writable path copied")
	}
}
