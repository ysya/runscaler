package main

import (
	"errors"
	"fmt"
	"os"
)

// darwinSystemPlist is the LaunchDaemon definition that releases before the
// LaunchAgent-only policy could install with --user=false.
const darwinSystemPlist = launchdSystemDir + "/" + launchdPlistFile

// refuseDarwinRoot stops root from running or installing runner on
// macOS: Tart keeps images in the invoking user's ~/.tart and Docker Desktop
// runs per user, so a root instance sees neither.
func refuseDarwinRoot(goos string, euid int, action string, exists func(string) bool) error {
	if goos != "darwin" || euid != 0 {
		return nil
	}
	msg := fmt.Sprintf("refusing to %s as root on macOS: run it as the logged-in user and install a LaunchAgent with 'runner service install'", action)
	if exists(darwinSystemPlist) {
		msg += fmt.Sprintf("\n\n  Found system service definition: %s\n  Remove it with 'sudo runner service uninstall --scope system', then install a LaunchAgent as the logged-in user", darwinSystemPlist)
	}
	return errors.New(msg)
}

// refuseDarwinSystemScope stops install, start and restart of a LaunchDaemon:
// macOS supports only a LaunchAgent for the logged-in user. System scope on
// macOS exists to show, stop or remove a daemon an older release installed.
func refuseDarwinSystemScope(goos string, scope serviceScope, action string) error {
	if goos != "darwin" || scope != scopeSystem {
		return nil
	}
	return fmt.Errorf("refusing to %s with --scope system on macOS: only a LaunchAgent for the logged-in user is supported; system scope can only show, stop or remove an old LaunchDaemon", action)
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
