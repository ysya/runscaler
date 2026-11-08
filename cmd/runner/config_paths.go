package main

import (
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/ysya/runscaler/internal/layout"
)

// Use the same CLI convention on Linux and macOS. Relative XDG paths are
// invalid under the XDG specification and must not depend on the working dir.
func userConfigPath() (string, error) {
	base, err := layout.CurrentIdentity().ConfigHome()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(base, "runner", "config.toml"), nil
}

// defaultScope is the scope of a fresh install when none is requested:
// macOS supports only a LaunchAgent, Linux defaults to a system service
// with a dedicated runtime account.
func defaultScope() serviceScope {
	if runtime.GOOS == "darwin" {
		return scopeUser
	}
	return scopeSystem
}

// Local files retain precedence for interactive CLI use. Services persist the
// selected absolute path rather than depending on launchd's working directory.
func configSearchPaths() ([]string, error) {
	current, err := userConfigPath()
	if err != nil {
		return nil, err
	}
	return []string{".", filepath.Dir(current), filepath.Dir(defaultConfigPath)}, nil
}
