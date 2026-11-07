package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSystemUnitDedicatedAccountDoesNotRecursivelyChangeDirectories(t *testing.T) {
	unit, err := renderSystemdUnit(installOpts{binaryPath: "/usr/local/bin/runner", configPath: "/etc/runner/config.toml", provider: "docker", runtimeUser: "runscaler", runtimeGroup: "runscaler", supplementaryGroups: []string{"docker"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"User=runscaler", "Group=runscaler", "SupplementaryGroups=docker", "WorkingDirectory=/var/lib/runner", `Environment="HOME=/var/lib/runner"`, `Environment="RUNNER_SERVICE_SCOPE=system"`} {
		if !strings.Contains(unit, want) {
			t.Errorf("missing %q in:\n%s", want, unit)
		}
	}
	for _, forbidden := range []string{"StateDirectory=", "LogsDirectory=", "PrivateTmp="} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("unexpected automatic directory handling %q", forbidden)
		}
	}
	if got := unitValues(unit, "ReadWritePaths"); !slices.Equal(got, []string{"/tmp", "/var/lib/runner", "/var/log/runner"}) {
		t.Fatalf("writable paths = %q", got)
	}
}

func TestSystemUnitUserModeKeepsCurrentUser(t *testing.T) {
	unit, err := renderSystemdUnit(installOpts{user: true, binaryPath: "/home/ci/.local/bin/runner", configPath: "/home/ci/.config/runner/config.toml"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unit, "User=runscaler") || strings.Contains(unit, "/var/lib/runner") {
		t.Fatalf("user service acquired system identity:\n%s", unit)
	}
}

func TestServiceAccountRejectsPersonalOrRootAccount(t *testing.T) {
	for _, account := range []serviceAccount{
		{Name: "runscaler", UID: 0, GID: 123, Home: "/var/lib/runner", Shell: "/usr/sbin/nologin"},
		{Name: "runscaler", UID: 123, GID: 123, Home: "/home/runscaler", Shell: "/bin/bash"},
		{Name: "runscaler", UID: 123, GID: 0, Home: "/var/lib/runner", Shell: "/usr/sbin/nologin"},
	} {
		if err := validateServiceAccount(account); err == nil {
			t.Fatalf("incompatible account accepted: %+v", account)
		}
	}
	if err := validateServiceAccount(serviceAccount{Name: "runscaler", UID: 123, GID: 123, Home: "/var/lib/runner", Shell: "/usr/sbin/nologin"}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceTreeAuditRefusesUnownedOrLinkedChildren(t *testing.T) {
	for _, kind := range []string{"regular", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "private-backup")
			if kind == "regular" {
				if err := os.WriteFile(path, []byte("sensitive"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink("/etc", path); err != nil {
					t.Fatal(err)
				}
			}
			if err := auditServiceTree(root, uint32(os.Geteuid()+100), uint32(os.Getegid()+100)); err == nil {
				t.Fatal("tree with excluded ownership or symlink accepted")
			}
			data, err := os.Lstat(path)
			if err != nil || data == nil {
				t.Fatalf("audit mutated data: %v", err)
			}
		})
	}
}

func TestEmptyForeignServiceDirectoryIsNotTakenOver(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires a directory owned by a non-root user")
	}
	if err := auditServiceTree(t.TempDir(), uint32(os.Geteuid()+100), uint32(os.Getegid()+100)); err == nil {
		t.Fatal("empty personal directory accepted for service ownership")
	}
}

func TestPrivateServiceFilesMustBeRegularAndUnlinked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("token='synthetic'"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateServiceFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(dir, "shared")); err != nil {
		t.Fatal(err)
	}
	if err := validateServiceFile(path); err == nil {
		t.Fatal("hard-linked credentials accepted for permission changes")
	}
	if err := validateServiceFile(dir); err == nil {
		t.Fatal("directory accepted as credential file")
	}
}

func TestServiceGroupRejectsPersonalMembersAndSharedIDs(t *testing.T) {
	passwd := []byte("runscaler:x:123:456:runner:/var/lib/runner:/usr/sbin/nologin\n")
	if err := validateDedicatedGroup([]byte("runscaler:x:456:runscaler\n"), passwd, 456); err != nil {
		t.Fatal(err)
	}
	if err := validateDedicatedGroup([]byte("runscaler:x:456:personal\n"), passwd, 456); err == nil {
		t.Fatal("personal group member accepted")
	}
	if err := validateDedicatedGroup([]byte("runscaler:x:456:\n"), append(passwd, []byte("personal:x:1000:456::/home/personal:/bin/sh\n")...), 456); err == nil {
		t.Fatal("shared primary group accepted")
	}
}

func TestSocketGroupGrantRejectsRootAndOtherUsersRootlessSocket(t *testing.T) {
	account := serviceAccount{Name: "runscaler", UID: 123, GID: 123, Home: "/var/lib/runner", Shell: "/usr/sbin/nologin"}
	if _, err := socketSupplementaryGroup(account, 0, 0, 0660); err == nil {
		t.Fatal("root group granted")
	}
	if _, err := socketSupplementaryGroup(account, 1000, 1000, 0660); err == nil {
		t.Fatal("another user's rootless socket accepted")
	}
	group, err := socketSupplementaryGroup(account, 0, 998, 0660)
	if err != nil || group != "998" {
		t.Fatalf("socket group = %q, %v", group, err)
	}
	group, err = socketSupplementaryGroup(account, 123, 123, 0600)
	if err != nil || group != "" {
		t.Fatalf("owned rootless socket = %q, %v", group, err)
	}
}

func TestRuntimeSelectionPreservesExistingRootAndCustomIdentity(t *testing.T) {
	for _, name := range []string{"root", "custom-runner", "runscaler"} {
		opts := installOpts{}
		applyInstalledIdentity(&opts, &installationTarget{Scope: scopeSystem, RuntimeUser: name, RuntimeGroup: name})
		if name == "root" {
			if opts.runtimeUser != "" {
				t.Fatal("existing root runtime changed")
			}
		} else if opts.runtimeUser != name {
			t.Fatalf("identity changed from %s to %s", name, opts.runtimeUser)
		}
	}
}

func TestLegacyRootRegenerationAlsoProtectsEnvironmentFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.env")
	if err := os.WriteFile(path, []byte("RUNNER_TOKEN=synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	m := &identityInstallManager{}
	err := installService(m, installOpts{binaryPath: "/usr/local/bin/runner", configPath: "/etc/runner/config.toml", environmentFiles: []string{path}}, func(_ bool, path string) (string, error) { return path, nil })
	if err == nil || m.installed != nil {
		t.Fatalf("unprotected environment installed in root unit: err=%v installed=%v", err, m.installed != nil)
	}
}

func TestServiceCredentialReferencesUseTOMLNotLineMatching(t *testing.T) {
	body := []byte("token = 'env:ORG_TOKEN'\n[github-app]\nprivate-key-env = 'APP_KEY'\n")
	got, err := credentialEnvironmentRefs(body)
	if err != nil || !reflect.DeepEqual(got, []string{"APP_KEY", "ORG_TOKEN"}) {
		t.Fatalf("references = %q, %v", got, err)
	}
	body = []byte("# token='env:IGNORED'\n[[scaleset]]\ntoken = 'env:A_TOKEN'\n[[scaleset]]\ngithub-app.private-key-env='B_KEY'\n")
	got, err = credentialEnvironmentRefs(body)
	if err != nil || !reflect.DeepEqual(got, []string{"A_TOKEN", "B_KEY"}) {
		t.Fatalf("multi references = %q, %v", got, err)
	}
}

func TestPrepareOwnedDirectoryChangesOnlyTheSelectedDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	var changed []string
	err := prepareOwnedDirectory(path, os.Geteuid(), os.Getegid(), func(path string, uid, gid int) error { changed = append(changed, path); return nil })
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0750 || !reflect.DeepEqual(changed, []string{path}) {
		t.Fatalf("prepared directory = %v, %q, %v", info, changed, err)
	}
}

func TestPrepareOwnedDirectoryRefusesPopulatedForeignTreeBeforeMutation(t *testing.T) {
	path := t.TempDir()
	file := filepath.Join(path, "unapproved")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	changed := false
	err := prepareOwnedDirectory(path, os.Geteuid()+100, os.Getegid()+100, func(string, int, int) error { changed = true; return nil })
	if err == nil || changed {
		t.Fatalf("unsafe preparation = %v, changed=%v", err, changed)
	}
	data, _ := os.ReadFile(file)
	if string(data) != "keep" {
		t.Fatal("unapproved file changed")
	}
}

type identityInstallManager struct {
	serviceManager
	installed *installOpts
}

func (m *identityInstallManager) install(opts installOpts) error { m.installed = &opts; return nil }

func TestSystemInstallationRunsPreflightBeforeAnyServiceWrite(t *testing.T) {
	saved := prepareSystemInstallation
	t.Cleanup(func() { prepareSystemInstallation = saved })
	prepareSystemInstallation = func(*installOpts) error { return errors.New("account conflicts") }
	m := &identityInstallManager{}
	err := installService(m, installOpts{runtimeUser: "runscaler", runtimeGroup: "runscaler", binaryPath: "/usr/local/bin/runner", configPath: "/etc/runner/config.toml"}, func(_ bool, p string) (string, error) { return p, nil })
	if err == nil || m.installed != nil {
		t.Fatalf("service written despite failed identity preparation: %v", err)
	}
	prepareSystemInstallation = func(opts *installOpts) error { opts.supplementaryGroups = []string{"998"}; return nil }
	if err := installService(m, installOpts{runtimeUser: "runscaler", runtimeGroup: "runscaler", binaryPath: "/usr/local/bin/runner", configPath: "/etc/runner/config.toml"}, func(_ bool, p string) (string, error) { return p, nil }); err != nil {
		t.Fatal(err)
	}
	if m.installed == nil || !reflect.DeepEqual(m.installed.supplementaryGroups, []string{"998"}) {
		t.Fatalf("preflight identity lost: %+v", m.installed)
	}
}

func TestParseDedicatedAccountUsesActualPasswdIdentity(t *testing.T) {
	account, found, err := parseServiceAccount([]byte("other:x:101:101:Other:/home/other:/bin/bash\nrunscaler:x:991:992:runner service:/var/lib/runner:/usr/sbin/nologin\n"))
	if err != nil || !found || account.UID != 991 || account.GID != 992 || validateServiceAccount(account) != nil {
		t.Fatalf("account=%+v found=%v err=%v", account, found, err)
	}
	if _, _, err := parseServiceAccount([]byte("runscaler:x:0:0:root:/var/lib/runner:/usr/sbin/nologin\n")); err == nil {
		t.Fatal("root account accepted")
	}
}
