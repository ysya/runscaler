package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	osuser "os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/ysya/runscaler/internal/layout"
)

func buildMaintenanceArgs(target *installationTarget, operation []string, noExpand bool) ([]string, error) {
	if target == nil || target.Scope != scopeSystem || !validServicePrincipal(target.RuntimeUser) || !validServicePrincipal(target.RuntimeGroup) || target.RuntimeUser == "root" || target.RuntimeUser == "0" || target.RuntimeGroup == "root" || target.RuntimeGroup == "0" || !filepath.IsAbs(target.BinaryPath) || !filepath.IsAbs(target.ConfigPath) {
		return nil, errors.New("cannot reproduce the selected system service identity")
	}
	var logDirs []string
	for _, path := range uniqueStrings(target.ReadWritePaths) {
		if _, ok := layout.SystemLogsDirectory(filepath.Join(path, "runner.log")); ok {
			logDirs = append(logDirs, path)
		}
	}
	if len(logDirs) == 0 {
		logDirs = []string{"/var/log/runner"}
	}
	args := []string{"systemd-run", "--wait", "--pipe", "--collect", "--property=Type=exec", "--property=RuntimeMaxSec=60", "--property=TimeoutStopSec=5", "--property=User=" + target.RuntimeUser, "--property=Group=" + target.RuntimeGroup, "--property=NoNewPrivileges=true", "--property=ProtectSystem=strict", "--property=ReadWritePaths=/tmp /var/lib/runner " + strings.Join(logDirs, " ")}
	if noExpand {
		args = append(args, "--expand-environment=no")
	}
	if len(target.SupplementaryGroups) > 0 {
		for _, group := range target.SupplementaryGroups {
			if !validServicePrincipal(group) || group == "root" || group == "0" {
				return nil, errors.New("unsafe service supplementary group")
			}
		}
		args = append(args, "--property=SupplementaryGroups="+strings.Join(target.SupplementaryGroups, " "))
	}
	working := target.WorkingDirectory
	if working == "" {
		working = serviceStateDir
	}
	if !filepath.IsAbs(working) {
		return nil, errors.New("service working directory is not absolute")
	}
	args = append(args, "--property=WorkingDirectory="+working)
	allowed := []string{"HOME", "RUNNER_SERVICE_SCOPE", "RUNNER_SERVICE_VERSION", "RUNNER_SERVICE_STOP_TIMEOUT", "XDG_CONFIG_HOME", "XDG_STATE_HOME"}
	for key := range target.Environment {
		found := false
		for _, safe := range allowed {
			if key == safe {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("service has custom or private inline environment; move it to a protected EnvironmentFile before reproducing maintenance")
		}
	}
	for _, key := range allowed {
		value, ok := target.Environment[key]
		if key == "HOME" && !ok {
			value, ok = serviceStateDir, true
		}
		if key == "RUNNER_SERVICE_SCOPE" {
			value, ok = "system", true
		}
		if ok {
			if strings.ContainsAny(value, "\x00\r\n") {
				return nil, errors.New("invalid service environment metadata")
			}
			args = append(args, "--setenv="+key+"="+value)
		}
	}
	args = append(args, "--setenv=RUNNER_MAINTENANCE_CONTEXT=1")
	for _, path := range target.EnvironmentFiles {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
			return nil, errors.New("invalid service environment file")
		}
		args = append(args, "--property=EnvironmentFile="+path)
	}
	command := append([]string{target.BinaryPath}, operation...)
	command = append(command, "--config", target.ConfigPath, "--scope", "system")
	if !noExpand {
		for _, arg := range command {
			if strings.Contains(arg, "$") {
				return nil, errors.New("this systemd-run lacks literal argument support; refusing dollar expansion")
			}
		}
	}
	args = append(args, "--")
	return append(args, command...), nil
}

func maintenanceCapabilities(ctx context.Context) (bool, error) {
	data, err := installationOutput(ctx, "systemd-run", "--help")
	if err != nil {
		return false, errors.New("systemd-run is unavailable for service-identity maintenance")
	}
	help := string(data)
	for _, flag := range []string{"--wait", "--pipe", "--collect"} {
		if !strings.Contains(help, flag) {
			return false, errors.New("systemd-run lacks the required bounded maintenance features")
		}
	}
	return strings.Contains(help, "--expand-environment"), nil
}

func maintenanceHint(ctx context.Context, target *installationTarget, operation []string) (string, error) {
	noExpand, err := maintenanceCapabilities(ctx)
	if err != nil {
		return "", err
	}
	args, err := buildMaintenanceArgs(target, operation, noExpand)
	if err != nil {
		return "", err
	}
	for i := range args {
		args[i] = shellQuotePath(args[i])
	}
	return "sudo " + strings.Join(args, " "), nil
}

func runtimeIdentityMatches(target *installationTarget) bool {
	if target.RuntimeUser == "" {
		return false
	}
	uid, err := strconv.Atoi(target.RuntimeUser)
	if err != nil {
		user, lookupErr := osuser.Lookup(target.RuntimeUser)
		if lookupErr != nil {
			return false
		}
		uid, err = strconv.Atoi(user.Uid)
	}
	if err != nil || os.Geteuid() != uid {
		return false
	}
	primary, err := strconv.Atoi(target.RuntimeGroup)
	if err != nil {
		group, lookupErr := osuser.LookupGroup(target.RuntimeGroup)
		if lookupErr != nil {
			return false
		}
		primary, err = strconv.Atoi(group.Gid)
	}
	if err != nil || os.Getegid() != primary {
		return false
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	groups = append(groups, os.Getegid())
	for _, name := range target.SupplementaryGroups {
		gid, err := strconv.Atoi(name)
		if err != nil {
			group, lookupErr := osuser.LookupGroup(name)
			if lookupErr != nil {
				return false
			}
			gid, err = strconv.Atoi(group.Gid)
		}
		if err != nil {
			return false
		}
		found := false
		for _, current := range groups {
			if current == gid {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return os.Getenv("RUNNER_MAINTENANCE_CONTEXT") == "1" && os.Getenv("RUNNER_SERVICE_SCOPE") == "system" && os.Getenv("HOME") == target.Environment["HOME"]
}

func requireDoctorIdentity(c *cobra.Command, target *installationTarget) error {
	if target.Scope != scopeSystem || target.RuntimeUser == "root" || target.RuntimeUser == "0" || runtimeIdentityMatches(target) {
		return nil
	}
	operation := []string{"doctor"}
	if fix, _ := c.Flags().GetBool("fix"); fix {
		operation = append(operation, "--fix")
	}
	hint, err := maintenanceHint(commandContext(c), target, operation)
	if err != nil {
		return err
	}
	return fmt.Errorf("diagnose the selected installation with its service identity and groups:\n\n  %s", hint)
}

func verifyPreparedService(opts installOpts) error {
	target := &installationTarget{Scope: scopeSystem, RuntimeUser: opts.runtimeUser, RuntimeGroup: opts.runtimeGroup, SupplementaryGroups: opts.supplementaryGroups, BinaryPath: opts.binaryPath, ConfigPath: opts.configPath, WorkingDirectory: serviceStateDir, Environment: map[string]string{"HOME": serviceStateDir, "RUNNER_SERVICE_SCOPE": "system"}, EnvironmentFiles: opts.environmentFiles}
	target.ReadWritePaths = []string{systemServiceLogDir(opts)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	noExpand, err := maintenanceCapabilities(ctx)
	if err != nil {
		return err
	}
	args, err := buildMaintenanceArgs(target, []string{"doctor"}, noExpand)
	if err != nil {
		return err
	}
	args = slices.Insert(args, slices.Index(args, "--"), "--setenv=RUNNER_SERVICE_PREPARATION=1")
	if err := serviceAdminCommand(ctx, args[0], args[1:]...); err != nil {
		return fmt.Errorf("service-identity provider probe failed; no service definition was changed: %w", err)
	}
	return nil
}
