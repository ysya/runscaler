package main

import (
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/ysya/runscaler/internal/layout"
)

// Use the same CLI convention on Linux and macOS. Relative XDG paths are
// invalid under the XDG specification and must not depend on the working dir.
func userConfigPath(app string) (string, error) {
	base, err := layout.CurrentIdentity().ConfigHome()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(base, app, "config.toml"), nil
}

func defaultUserService() bool { return runtime.GOOS == "darwin" }

// Local files retain precedence for interactive CLI use. Services persist the
// selected absolute path rather than depending on launchd's working directory.
func configSearchPaths() ([]string, error) {
	current, err := userConfigPath("runner")
	if err != nil {
		return nil, err
	}
	legacy, err := userConfigPath("runscaler")
	if err != nil {
		return nil, err
	}
	return []string{".", filepath.Dir(current), filepath.Dir(defaultConfigPath), filepath.Dir(legacy), legacyConfigDir}, nil
}
