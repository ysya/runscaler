package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// darwinSystemPlists are LaunchDaemon definitions older releases installed.
var darwinSystemPlists = []string{
	launchdSystemDir + "/" + launchdPlistFile,
	launchdSystemDir + "/" + legacyLaunchdPlist,
}

// refuseDarwinRoot stops root from running, installing or migrating runner on
// macOS: Tart keeps images in the invoking user's ~/.tart and Docker Desktop
// runs per user, so a root instance sees neither.
func refuseDarwinRoot(goos string, euid int, action string, exists func(string) bool) error {
	if goos != "darwin" || euid != 0 {
		return nil
	}
	msg := fmt.Sprintf("refusing to %s as root on macOS: run it as the logged-in user and install a LaunchAgent with 'runner service install'", action)
	var found []string
	for _, plist := range darwinSystemPlists {
		if exists(plist) {
			found = append(found, plist)
		}
	}
	if len(found) > 0 {
		msg += fmt.Sprintf("\n\n  Found system service definitions: %s\n  Convert them as described in the README section \"macOS LaunchDaemon to LaunchAgent\"", strings.Join(found, ", "))
	}
	return errors.New(msg)
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
