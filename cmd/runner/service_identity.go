package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/pelletier/go-toml/v2"
)

const serviceAccountName = "runscaler"
const serviceStateDir = "/var/lib/runner"

type serviceAccount struct {
	Name  string
	UID   int
	GID   int
	Home  string
	Shell string
}

func validServicePrincipal(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	return value != "." && value != ".."
}

func validateServiceAccount(account serviceAccount) error {
	if account.Name != serviceAccountName || account.UID <= 0 || account.GID <= 0 || account.Home != serviceStateDir || (filepath.Base(account.Shell) != "nologin" && account.Shell != "/bin/false") {
		return errors.New("existing runscaler account is incompatible; refusing to take over a personal or privileged account")
	}
	return nil
}

func validateServiceFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink != 1 {
		return fmt.Errorf("service credential %s must be a regular file without hard links", path)
	}
	return nil
}

func validateDedicatedGroup(groups, passwd []byte, gid int) error {
	found := false
	for _, line := range strings.Split(string(groups), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 4 {
			continue
		}
		id, err := strconv.Atoi(fields[2])
		if err != nil || id != gid {
			continue
		}
		if fields[0] != serviceAccountName {
			return errors.New("service group ID is shared with another group")
		}
		found = true
		for _, member := range strings.Split(fields[3], ",") {
			if member != "" && member != serviceAccountName {
				return errors.New("service group has personal members; refusing to expose credentials")
			}
		}
	}
	if !found {
		return errors.New("dedicated service group is missing")
	}
	for _, line := range strings.Split(string(passwd), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 7 {
			continue
		}
		id, err := strconv.Atoi(fields[3])
		if err == nil && id == gid && fields[0] != serviceAccountName {
			return errors.New("service primary group is shared with another account")
		}
	}
	return nil
}

func applyInstalledIdentity(opts *installOpts, target *installationTarget) {
	if target == nil || target.Scope != scopeSystem {
		return
	}
	if target.RuntimeUser != "" && target.RuntimeUser != "root" && target.RuntimeUser != "0" {
		opts.runtimeUser = target.RuntimeUser
		opts.runtimeGroup = target.RuntimeGroup
		opts.supplementaryGroups = append([]string(nil), target.SupplementaryGroups...)
	}
	opts.environmentFiles = append([]string(nil), target.EnvironmentFiles...)
}

// auditServiceTree is deliberately read-only. Only an empty root-owned
// directory or an entirely service-owned tree is eligible for preparation.
// Linked, special or unowned children block preparation.
func auditServiceTree(root string, uid, gid uint32) error {
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("service directory %s is not a real directory", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (uint32(st.Uid) != 0 && (uint32(st.Uid) != uid || uint32(st.Gid) != gid)) || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("service directory %s has incompatible ownership or permissions", root)
		}
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (!info.IsDir() && !info.Mode().IsRegular()) || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && st.Nlink != 1) || uint32(st.Uid) != uid || uint32(st.Gid) != gid || info.Mode().Perm()&0o002 != 0 {
			return fmt.Errorf("service directory contains an unapproved entry at %s; existing data was not changed", path)
		}
		return nil
	})
}

// Rootful daemon access is an explicit unit-only grant. Never grant GID 0,
// change socket permissions, or adopt another user's rootless endpoint.
func socketSupplementaryGroup(account serviceAccount, owner, gid uint32, mode os.FileMode) (string, error) {
	if owner == uint32(account.UID) {
		if mode&0o600 == 0o600 {
			return "", nil
		}
		return "", errors.New("service account cannot read/write its Docker socket")
	}
	if owner != 0 {
		return "", errors.New("docker socket belongs to another user; use that user's --user installation or prepare a daemon for the service account")
	}
	if gid == 0 {
		return "", errors.New("refusing to grant the root group for Docker access")
	}
	if mode&0o060 != 0o060 {
		return "", errors.New("docker socket does not grant group read/write access")
	}
	if gid == uint32(account.GID) {
		return "", nil
	}
	return strconv.FormatUint(uint64(gid), 10), nil
}

func systemServiceLogDir(opts installOpts) string {
	dir, _ := systemdLogsDirectory(opts.logFile)
	return filepath.Join("/var/log", dir)
}

func serviceAuthTables(data []byte) ([]map[string]any, error) {
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, errors.New("invalid service configuration")
	}
	if sets, ok := raw["scaleset"].([]any); ok && len(sets) > 0 {
		var tables []map[string]any
		for _, set := range sets {
			table, ok := set.(map[string]any)
			if !ok {
				return nil, errors.New("invalid scale set configuration")
			}
			tables = append(tables, table)
		}
		return tables, nil
	}
	return []map[string]any{raw}, nil
}

func credentialEnvironmentRefs(data []byte) ([]string, error) {
	tables, err := serviceAuthTables(data)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, table := range tables {
		if token, ok := table["token"].(string); ok {
			if name, ok := strings.CutPrefix(token, "env:"); ok {
				refs = append(refs, name)
			}
		}
		if app, ok := table["github-app"].(map[string]any); ok {
			if name, ok := app["private-key-env"].(string); ok && name != "" {
				refs = append(refs, name)
			}
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs), nil
}

func prepareOwnedDirectory(path string, uid, gid int, chown func(string, int, int) error) error {
	if !filepath.IsAbs(path) {
		return errors.New("service directory must be absolute")
	}
	if err := auditServiceTree(path, uint32(uid), uint32(gid)); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0750); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0750)
}
