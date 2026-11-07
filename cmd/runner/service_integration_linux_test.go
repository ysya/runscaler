//go:build linux && systemd_integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/ysya/runscaler/internal/versioncheck"
)

// These tests intentionally exercise real systemd, account preparation and
// permissions. The tag alone never authorizes them on a developer host.
func TestSystemdInstallationFixture(t *testing.T) {
	if os.Getenv("RUNNER_SYSTEMD_FIXTURE") != "1" {
		t.Skip("isolated fixture opt-in is required")
	}
	if os.Geteuid() != 0 || os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("fixture requires an isolated root GitHub Actions runner")
	}
	mode := os.Getenv("RUNNER_FIXTURE_MODE")
	if mode != "system" && mode != "root" && mode != "user" {
		t.Fatal("invalid fixture mode")
	}
	cli := os.Getenv("RUNNER_FIXTURE_BINARY")
	if err := checkRootOnlyChain(cli, lstatFile); err != nil {
		t.Fatal(err)
	}
	owner, err := osuser.Lookup(os.Getenv("RUNNER_FIXTURE_OWNER"))
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(owner.Uid)
	if err != nil || uid == 0 {
		t.Fatal("fixture owner must be non-root")
	}
	gid, err := strconv.Atoi(owner.Gid)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/etc/runner", serviceStateDir, "/var/log/runner", filepath.Join(systemdSystemDir, systemdUnitFile), filepath.Join(owner.HomeDir, ".config/systemd/user/runner.service")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("refusing pre-existing fixture path %s: %v", path, err)
		}
	}
	if _, exists, err := readServiceAccount(); err != nil || exists {
		t.Fatalf("refusing pre-existing service account: exists=%v err=%v", exists, err)
	}
	if _, err := osuser.LookupGroup(serviceAccountName); err == nil {
		t.Fatal("refusing pre-existing service group")
	}
	base, err := os.MkdirTemp("", "runner-systemd-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(base, "user")
	for _, path := range []string{userDir, filepath.Join(userDir, "config"), filepath.Join(userDir, "config/runner"), filepath.Join(userDir, "state"), filepath.Join(userDir, "state/runner")} {
		if err := os.Mkdir(path, 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	fixtureEnv := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + owner.HomeDir, "XDG_CONFIG_HOME=" + filepath.Join(userDir, "config"), "XDG_STATE_HOME=" + filepath.Join(userDir, "state"), "XDG_RUNTIME_DIR=/run/user/" + owner.Uid, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + owner.Uid + "/bus"}
	groups, err := owner.GroupIds()
	if err != nil {
		t.Fatal(err)
	}
	credential := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	for _, id := range groups {
		n, err := strconv.Atoi(id)
		if err != nil {
			t.Fatal(err)
		}
		credential.Groups = append(credential.Groups, uint32(n))
	}
	run := func(asUser bool, binary string, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, binary, args...)
		if asUser {
			c.Env = fixtureEnv
			c.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
		}
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %q: %v\n%s", binary, args, err, out)
		}
		return out
	}
	state := serviceStateDir
	if mode == "root" {
		state = "/var/log/runner"
	}
	configPath := "/etc/runner/config $literal file.toml"
	helper := filepath.Join(filepath.Dir(cli), "service-fixture")
	if mode == "user" {
		state = filepath.Join(userDir, "state/runner")
		configPath = filepath.Join(userDir, "config/runner/config $literal file.toml")
		helper = filepath.Join(userDir, "service-fixture")
		run(false, "systemctl", "start", "user@"+owner.Uid+".service")
	} else {
		if err := os.Mkdir("/etc/runner", 0750); err != nil {
			t.Fatal(err)
		}
	}
	// Every non-temporary path below was checked absent. Cleanup refuses a
	// changed unit and removes only this fixture's known files/directories.
	t.Cleanup(func() {
		unit := filepath.Join(systemdSystemDir, systemdUnitFile)
		flags := []string{}
		if mode == "user" {
			unit = filepath.Join(owner.HomeDir, ".config/systemd/user/runner.service")
			flags = []string{"--user"}
		}
		if data, err := os.ReadFile(unit); err == nil {
			if !bytes.Contains(data, []byte(helper)) {
				t.Errorf("fixture unit changed; leaving %s intact", unit)
				return
			}
			for _, action := range []string{"stop", "disable"} {
				run(mode == "user", "systemctl", append(flags, action, "runner.service")...)
			}
			if err := os.Remove(unit); err != nil {
				t.Error(err)
			}
			run(mode == "user", "systemctl", append(flags, "daemon-reload")...)
		}
		if mode != "user" {
			for _, path := range []string{configPath, "/etc/runner/service env.conf", filepath.Join(state, "fixture.info"), filepath.Join(state, "fixture.stopped"), "/var/log/runner/fixture.log", helper} {
				_ = os.Remove(path)
			}
			for _, dir := range []string{"/etc/runner", serviceStateDir, "/var/log/runner"} {
				if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
					t.Errorf("fixture directory not empty or removable: %s: %v", dir, err)
				}
			}
		}
	})
	body := "url='https://github.com/example'\nname='systemd-fixture'\ntoken='synthetic-fixture-token'\nhealth-port=0\n"
	if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if mode == "user" {
		if err := os.Chown(configPath, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(helper); !os.IsNotExist(err) {
		t.Fatalf("refusing pre-existing helper: %v", err)
	}
	program := fixtureProgram("0.0.1")
	if err := os.WriteFile(helper, []byte(program), 0755); err != nil {
		t.Fatal(err)
	}
	if mode == "user" {
		if err := os.Chown(helper, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"service", "install", "--config-path", configPath, "--binary-path", helper}
	if mode == "user" {
		args = append(args, "--scope", "user")
	} else {
		if err := os.WriteFile("/etc/runner/service env.conf", []byte("RUNNER_FIXTURE_ENV=literal-environment-file\n"), 0600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--environment-file", "/etc/runner/service env.conf")
	}
	if mode == "root" {
		for _, path := range []string{serviceStateDir, "/var/log/runner"} {
			if err := os.Mkdir(path, 0750); err != nil {
				t.Fatal(err)
			}
		}
		unit, err := renderSystemdUnit(installOpts{binaryPath: helper, configPath: configPath, provider: "docker", environmentFiles: []string{"/etc/runner/service env.conf"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(systemdSystemDir, systemdUnitFile), []byte(unit), 0644); err != nil {
			t.Fatal(err)
		}
		run(false, "systemctl", "daemon-reload")
		args = append(args, "--force")
	}
	run(mode == "user", cli, args...)
	infoPath := filepath.Join(state, "fixture.info")
	deadline := time.Now().Add(15 * time.Second)
	var info string
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(infoPath); err == nil {
			info = string(data)
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if info == "" {
		t.Fatal("fixture service did not publish runtime identity")
	}
	wantUID, wantGID := 0, 0
	if mode == "system" {
		account, exists, err := readServiceAccount()
		if err != nil || !exists {
			t.Fatalf("service account unavailable: %v", err)
		}
		wantUID, wantGID = account.UID, account.GID
	}
	if mode == "user" {
		wantUID, wantGID = uid, gid
	}
	for _, want := range []string{fmt.Sprintf("uid=%d\n", wantUID), fmt.Sprintf("gid=%d\n", wantGID), "config=" + configPath + "\n", "scope=" + map[bool]string{true: "user", false: "system"}[mode == "user"] + "\n"} {
		if !strings.Contains(info, want) {
			t.Fatalf("runtime identity missing %q:\n%s", want, info)
		}
	}
	if mode == "system" {
		for _, want := range []string{"home=/var/lib/runner\n", "cwd=/var/lib/runner\n"} {
			if !strings.Contains(info, want) {
				t.Fatalf("missing runtime property %q: %s", want, info)
			}
		}
		socket, err := os.Stat("/var/run/docker.sock")
		if err != nil {
			t.Fatal(err)
		}
		socketGID := socket.Sys().(*syscall.Stat_t).Gid
		if !strings.Contains(info, " "+strconv.FormatUint(uint64(socketGID), 10)+" ") {
			t.Fatalf("Docker socket group missing: %s", info)
		}
	}
	if mode == "user" && !strings.Contains(info, "home="+owner.HomeDir+"\n") {
		t.Fatal("user HOME changed")
	}
	if mode != "user" && !strings.Contains(info, "env=literal-environment-file\n") {
		unit, _ := os.ReadFile(filepath.Join(systemdSystemDir, systemdUnitFile))
		t.Fatalf("literal EnvironmentFile was not loaded: identity=%q\nunit:\n%s", info, unit)
	}
	// --force must keep the runtime identity and selected config/binary.
	run(mode == "user", cli, append(args, "--force", "--no-start")...)
	run(mode == "user", cli, "service", "stop", "--scope", map[bool]string{true: "user", false: "system"}[mode == "user"])
	if _, err := os.Stat(filepath.Join(state, "fixture.stopped")); err != nil {
		t.Fatal("stop did not deliver the fixture's drain signal")
	}
	if mode == "user" {
		c := exec.Command(os.Args[0], "-test.run=^TestSystemdUserUpdateChild$", "-test.v")
		c.Env = append(fixtureEnv, "RUNNER_SYSTEMD_CHILD=1", "RUNNER_FIXTURE_BINARY="+cli, "RUNNER_FIXTURE_HELPER="+helper, "RUNNER_FIXTURE_CONFIG="+configPath)
		c.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("user update fixture: %v\n%s", err, out)
		}
	} else {
		fixtureUpdate(t, cli, helper, configPath, scopeSystem)
	}
	run(mode == "user", cli, "service", "restart", "--scope", map[bool]string{true: "user", false: "system"}[mode == "user"])
}

func TestSystemdUserUpdateChild(t *testing.T) {
	if os.Getenv("RUNNER_SYSTEMD_CHILD") != "1" {
		t.Skip("parent fixture only")
	}
	if os.Geteuid() == 0 {
		t.Fatal("user fixture did not drop privileges")
	}
	fixtureUpdate(t, os.Getenv("RUNNER_FIXTURE_BINARY"), os.Getenv("RUNNER_FIXTURE_HELPER"), os.Getenv("RUNNER_FIXTURE_CONFIG"), scopeUser)
}

func fixtureUpdate(t *testing.T, cli, helper, configPath string, scope serviceScope) {
	t.Helper()
	savedLatest, savedInstall, savedExe := latestUpdateRelease, installUpdateRelease, updateExecutablePath
	t.Cleanup(func() {
		latestUpdateRelease = savedLatest
		installUpdateRelease = savedInstall
		updateExecutablePath = savedExe
		viper.Reset()
	})
	before, err := os.ReadFile(cli)
	if err != nil {
		t.Fatal(err)
	}
	latestUpdateRelease = func(context.Context) (*versioncheck.ReleaseInfo, error) {
		return &versioncheck.ReleaseInfo{TagName: "v0.0.2"}, nil
	}
	installUpdateRelease = func(_ context.Context, _ string, path string) error {
		if path != helper {
			t.Errorf("updated wrong executable: %s", path)
		}
		return writeFileAtomic(path, []byte(fixtureProgram("0.0.2")), 0755)
	}
	updateExecutablePath = func() (string, error) { return cli, nil }
	c := &cobra.Command{Use: "update"}
	c.SetContext(context.Background())
	c.Flags().Bool("check", false, "")
	c.Flags().String("binary-path", "", "")
	c.Flags().String("config", "", "")
	addInstallationFlags(c)
	if err := c.Flags().Set("scope", string(scope)); err != nil {
		t.Fatal(err)
	}
	if err := runUpdate(c, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(cli)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("invoking CLI was replaced")
	}
	got, err := installedBinaryVersion(context.Background(), helper)
	if err != nil || got != "0.0.2" {
		t.Fatalf("target version=%s, err=%v", got, err)
	}
	target, err := commandInstallation(c)
	if err != nil || target.ConfigPath != configPath {
		t.Fatalf("selected config=%+v err=%v", target, err)
	}
}

func fixtureProgram(version string) string {
	return `#!/bin/sh
set -eu
if [ "$1" = version ]; then printf '{"version":"` + version + `"}\n'; exit 0; fi
operation=$1
shift
config=
while [ "$#" -gt 0 ]; do
  case "$1" in --config) config=$2; shift;; esac
  shift
done
scope=${RUNNER_SERVICE_SCOPE:-system}
state=/var/lib/runner
if [ "$scope" = user ]; then state=$XDG_STATE_HOME/runner; elif [ "$(id -u)" -eq 0 ]; then state=/var/log/runner; fi
test -r "$config"
test -w "$state"
if [ "$scope" = system ]; then printf log-probe > /var/log/runner/fixture.log; fi
if [ "$scope" = system ] && [ "$(id -u)" -ne 0 ]; then test ! -w "$config"; fi
if [ "$operation" = doctor ]; then
  /usr/bin/curl --fail --silent --max-time 5 --unix-socket /var/run/docker.sock http://localhost/_ping >/dev/null
  exit 0
fi
umask 077
printf 'uid=%s\ngid=%s\nhome=%s\ncwd=%s\ngroups= %s \nconfig=%s\nscope=%s\nenv=%s\n' "$(id -u)" "$(id -g)" "${HOME:-}" "$PWD" "$(id -G)" "$config" "$scope" "${RUNNER_FIXTURE_ENV:-}" > "$state/fixture.info.tmp"
mv "$state/fixture.info.tmp" "$state/fixture.info"
trap 'printf stopped > "$state/fixture.stopped"; exit 0' TERM QUIT
while :; do sleep 1 & wait $! || :; done
`
}
