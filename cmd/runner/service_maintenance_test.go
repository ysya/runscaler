package main

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
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

func TestPreparedServiceProbeMarksThePreUnitContext(t *testing.T) {
	savedOutput, savedAdmin := installationOutput, serviceAdminCommand
	t.Cleanup(func() { installationOutput = savedOutput; serviceAdminCommand = savedAdmin })
	installationOutput = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("--wait --pipe --collect --expand-environment"), nil
	}
	called := false
	serviceAdminCommand = func(_ context.Context, name string, args ...string) error {
		called = true
		if name != "systemd-run" || !slices.Contains(args, "--setenv=RUNNER_SERVICE_PREPARATION=1") {
			t.Fatalf("probe context missing: %s %q", name, args)
		}
		return nil
	}
	if err := verifyPreparedService(installOpts{runtimeUser: "runscaler", runtimeGroup: "runscaler", binaryPath: "/usr/local/bin/runner", configPath: "/etc/runner/config.toml"}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("service identity probe was not run")
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
