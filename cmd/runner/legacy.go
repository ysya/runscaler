package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Legacy identifiers from before the runscaler→runner rename, kept for upgrade
// compatibility. Centralized here so the compat layer is easy to maintain and
// to remove in a future release. Declared as vars (not consts) so tests can
// point detection at temp paths.
var (
	legacyConfigDir    = "/etc/runscaler"
	legacyConfigPath   = "/etc/runscaler/config.toml"
	legacySystemdUnit  = "runscaler.service"
	legacyServiceName  = "runscaler"
	legacyLaunchdPlist = "com.runscaler.agent.plist"
	legacySharedVolume = "runscaler-shared"
	legacyTokenEnv     = "RUNSCALER_TOKEN"
)

// legacyConfigExists reports whether a config remains at the old location.
func legacyConfigExists() bool {
	_, err := os.Stat(legacyConfigPath)
	return err == nil
}

// legacyLaunchdPlistPath returns the legacy launchd plist path for the level.
func legacyLaunchdPlistPath(user bool) string {
	if user {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "LaunchAgents", legacyLaunchdPlist)
	}
	return filepath.Join(launchdSystemDir, legacyLaunchdPlist)
}

// legacyServiceInstalled reports whether a legacy service is present at the
// given level (current OS only).
func legacyServiceInstalled(user bool) bool {
	return serviceFilePresent(user, legacySystemdUnit, legacyLaunchdPlist)
}

// warnLegacy prints a one-line deprecation notice to stderr.
func warnLegacy(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "  ⚠ "+format+"\n", args...)
}
