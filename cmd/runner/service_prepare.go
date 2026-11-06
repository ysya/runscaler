package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/viper"
	"github.com/ysya/runscaler/internal/config"
)

var readServiceIdentityFile = os.ReadFile
var lookupServiceGroup = osuser.LookupGroup

func parseServiceAccount(data []byte) (serviceAccount, bool, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) == 0 || fields[0] != serviceAccountName {
			continue
		}
		if len(fields) != 7 {
			return serviceAccount{}, false, errors.New("invalid runscaler account record")
		}
		uid, e1 := strconv.Atoi(fields[2])
		gid, e2 := strconv.Atoi(fields[3])
		account := serviceAccount{Name: fields[0], UID: uid, GID: gid, Home: fields[5], Shell: fields[6]}
		if e1 != nil || e2 != nil {
			return account, true, errors.New("invalid service account IDs")
		}
		return account, true, validateServiceAccount(account)
	}
	return serviceAccount{}, false, nil
}

func readServiceAccount() (serviceAccount, bool, error) {
	data, err := readServiceIdentityFile("/etc/passwd")
	if err != nil {
		return serviceAccount{}, false, err
	}
	return parseServiceAccount(data)
}

// Every administrative program is resolved and checked before execution. No
// shell is used, and failed commands never copy arbitrary output into errors.
var serviceAdminCommand = func(ctx context.Context, name string, args ...string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("required system tool %s is unavailable; prepare the service account manually", name)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if err := checkRootOnlyChain(path, lstatFile); err != nil {
		return fmt.Errorf("untrusted system tool %s: %w", name, err)
	}
	if err := exec.CommandContext(ctx, path, args...).Run(); err != nil {
		return fmt.Errorf("system tool %s failed; no existing account was reconfigured", name)
	}
	return nil
}

func ensureServiceAccount(ctx context.Context) (serviceAccount, error) {
	account, exists, err := readServiceAccount()
	if err != nil {
		return account, err
	}
	group, groupErr := lookupServiceGroup(serviceAccountName)
	if groupErr != nil {
		var unknown osuser.UnknownGroupError
		if !errors.As(groupErr, &unknown) {
			return account, errors.New("cannot verify service group")
		}
		if exists {
			return account, errors.New("existing account does not have the dedicated service group")
		}
		if err := serviceAdminCommand(ctx, "groupadd", "--system", serviceAccountName); err != nil {
			return account, err
		}
		group, groupErr = lookupServiceGroup(serviceAccountName)
	}
	if groupErr != nil {
		return account, errors.New("service group was not created")
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil || gid <= 0 {
		return account, errors.New("service group must be non-root")
	}
	groups, err := readServiceIdentityFile("/etc/group")
	if err != nil {
		return account, err
	}
	passwd, err := readServiceIdentityFile("/etc/passwd")
	if err != nil {
		return account, err
	}
	if err := validateDedicatedGroup(groups, passwd, gid); err != nil {
		return account, err
	}
	if exists {
		if account.GID != gid {
			return account, errors.New("existing account primary group does not match the service group")
		}
		return account, nil
	}
	shell := ""
	for _, candidate := range []string{"/usr/sbin/nologin", "/sbin/nologin", "/bin/false"} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			shell = candidate
			break
		}
	}
	if shell == "" {
		return account, errors.New("no non-login shell is installed; prepare the dedicated account manually")
	}
	if err := serviceAdminCommand(ctx, "useradd", "--system", "--gid", serviceAccountName, "--home-dir", serviceStateDir, "--no-create-home", "--shell", shell, "--comment", "runner service", serviceAccountName); err != nil {
		return account, err
	}
	account, exists, err = readServiceAccount()
	if err != nil {
		return account, err
	}
	if !exists {
		return account, errors.New("service account was not created")
	}
	return account, nil
}

func loadServicePreparationConfig(path string) (config.Config, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config.Config{}, nil, err
	}
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return config.Config{}, nil, errors.New("invalid service configuration")
	}
	cfg, err := config.Load(v)
	if err != nil {
		return cfg, nil, errors.New("invalid service configuration")
	}
	if len(cfg.Warnings) > 0 {
		return cfg, nil, errors.New("service configuration has warnings; run runner validate before installation")
	}
	if err := cfg.ValidateGlobal(); err != nil {
		return cfg, nil, err
	}
	return cfg, data, nil
}

// Fresh installation only. Existing root services never enter this path.
// All directory audits precede account or file mutations. An interrupted fresh
// preparation may leave the dedicated account and owned directories for retry;
// it never takes over a populated foreign tree or recursively changes owners.
var prepareSystemInstallation = func(opts *installOpts) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("system service preparation requires root on Linux")
	}
	if opts.runtimeUser != serviceAccountName || opts.runtimeGroup != serviceAccountName {
		return errors.New("custom service identities require manual preparation; existing service was not changed")
	}
	if err := checkRootOnlyChain(opts.configPath, lstatFile); err != nil {
		return fmt.Errorf("system config must be root-owned and protected: %w", err)
	}
	cfg, raw, err := loadServicePreparationConfig(opts.configPath)
	if err != nil {
		return err
	}
	refs, err := credentialEnvironmentRefs(raw)
	if err != nil {
		return err
	}
	if len(refs) > 0 && len(opts.environmentFiles) == 0 {
		return errors.New("service credentials reference environment variables; provide --environment-file instead of relying on the installing shell")
	}
	tables, err := serviceAuthTables(raw)
	if err != nil {
		return err
	}
	for _, table := range tables {
		token, _ := table["token"].(string)
		_, app := table["github-app"]
		if strings.TrimSpace(token) == "" && !app {
			return errors.New("service credentials must be explicit in config or a referenced environment file; shell token fallback is not persisted")
		}
	}
	for _, path := range opts.environmentFiles {
		if err := validateServiceFile(path); err != nil {
			return err
		}
		if err := checkRootOnlyChain(path, lstatFile); err != nil {
			return fmt.Errorf("environment file is not protected: %w", err)
		}
	}
	var files = []string{opts.configPath}
	for _, ss := range cfg.ResolveScaleSets() {
		// Validation is offline. Unresolved env credentials are checked later
		// by the transient probe, never imported from the installing shell.
		if ss.Token == "" && ss.GitHubApp == nil {
			ss.Token = "service-environment-preflight"
		}
		if err := ss.Validate(); err != nil {
			return err
		}
		if ss.IsTart() {
			return errors.New("tart system installation is unsupported; use macOS user mode")
		}
		if ss.GitHubApp != nil && ss.GitHubApp.PrivateKeyFile != "" {
			if err := checkRootOnlyChain(ss.GitHubApp.PrivateKeyFile, lstatFile); err != nil {
				return fmt.Errorf("app key must be a protected system file: %w", err)
			}
			if _, err := ss.NewScalesetClient(slog.Default()); err != nil {
				return fmt.Errorf("invalid App credentials: %w", err)
			}
			files = append(files, ss.GitHubApp.PrivateKeyFile)
		}
	}
	for _, path := range files {
		if err := validateServiceFile(path); err != nil {
			return err
		}
	}
	account, exists, err := readServiceAccount()
	if err != nil {
		return err
	}
	uid, gid := uint32(^uint32(0)), uint32(^uint32(0))
	if exists {
		uid, gid = uint32(account.UID), uint32(account.GID)
	}
	dirs := []string{serviceStateDir, systemServiceLogDir(*opts)}
	for _, path := range dirs {
		parent, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("prepare the protected parent directory for %s first", path)
		}
		if err := checkRootOnlyChain(parent, lstatFile); err != nil {
			return err
		}
		if err := auditServiceTree(path, uid, gid); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	account, err = ensureServiceAccount(ctx)
	if err != nil {
		return err
	}
	var groups []string
	for _, ss := range cfg.ResolveScaleSets() {
		info, err := os.Stat(ss.Docker.Socket)
		if err != nil {
			return fmt.Errorf("cannot inspect configured Docker socket %s: %w", ss.Docker.Socket, err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSocket == 0 {
			return errors.New("configured Docker endpoint is not a Unix socket")
		}
		group, err := socketSupplementaryGroup(account, uint32(st.Uid), uint32(st.Gid), info.Mode())
		if err != nil {
			return err
		}
		if group != "" {
			groups = append(groups, group)
		}
	}
	// The config directory is managed only at the canonical location. Outside
	// it, administrators must already have made parent traversal possible.
	if filepath.Dir(opts.configPath) == "/etc/runner" {
		if err := os.Chown("/etc/runner", 0, account.GID); err != nil {
			return err
		}
		if err := os.Chmod("/etc/runner", 0750); err != nil {
			return err
		}
	}
	for _, path := range files {
		if err := os.Chown(path, 0, account.GID); err != nil {
			return err
		}
		if err := os.Chmod(path, 0640); err != nil {
			return err
		}
	}
	for _, path := range dirs {
		if err := prepareOwnedDirectory(path, account.UID, account.GID, os.Chown); err != nil {
			return err
		}
	}
	opts.supplementaryGroups = uniqueStrings(groups)
	if len(groups) > 0 {
		fmt.Println("  Docker daemon access is granted through unit supplementary groups; rootful Docker access remains root-equivalent.")
	}
	if !opts.noStart {
		if err := verifyPreparedService(*opts); err != nil {
			return err
		}
	}
	return nil
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
