package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	dockerclient "github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

// newConfigPath is the destination for migrated config (var for testability).
var newConfigPath = defaultConfigPath // "/etc/runner/config.toml"

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate an existing runscaler install to runner",
	Long: `Move config from /etc/runscaler to /etc/runner, reinstall the service
under the new name, and clean up the old docker volume. Idempotent — safe to
run multiple times; a fresh install has nothing to migrate.`,
	RunE: runMigrate,
}

func init() {
	migrateCmd.Flags().Bool("user", false, "Migrate user-level service")
	cmd.AddCommand(migrateCmd)
}

func runMigrate(c *cobra.Command, _ []string) error {
	user, _ := c.Flags().GetBool("user")
	if err := checkPrivileges(user); err != nil {
		return err
	}

	did := false

	moved, err := migrateConfig()
	if err != nil {
		return fmt.Errorf("config migration failed: %w", err)
	}
	if moved {
		fmt.Printf("  ✓ Moved config %s → %s\n", legacyConfigPath, newConfigPath)
		did = true
	}

	migratedSvc, err := migrateService(user)
	if err != nil {
		return fmt.Errorf("service migration failed: %w", err)
	}
	if migratedSvc {
		fmt.Printf("  ✓ Reinstalled service under the new name\n")
		did = true
	}

	if cleaned := migrateVolume(c.Context()); cleaned {
		fmt.Printf("  ✓ Removed legacy docker volume %s\n", legacySharedVolume)
		did = true
	}

	if os.Getenv(legacyTokenEnv) != "" {
		warnLegacy("the %s env var is deprecated — rename it to RUNNER_TOKEN", legacyTokenEnv)
	}

	if !did {
		fmt.Println("  ✓ Nothing to migrate")
	}
	return nil
}

// migrateConfig moves the legacy config to the new path. Returns whether it
// moved anything. No-op if the legacy file is absent or the target exists.
func migrateConfig() (bool, error) {
	if _, err := os.Stat(legacyConfigPath); err != nil {
		return false, nil // nothing at the old path
	}
	if _, err := os.Stat(newConfigPath); err == nil {
		warnLegacy("legacy config %s exists but %s is already present — leaving both, not overwriting", legacyConfigPath, newConfigPath)
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(newConfigPath), 0o755); err != nil {
		return false, err
	}
	if err := os.Rename(legacyConfigPath, newConfigPath); err != nil {
		return false, err
	}
	return true, nil
}

// migrateService removes a legacy-named service and installs the new one.
// Returns whether it did anything.
func migrateService(user bool) (bool, error) {
	if !legacyServiceInstalled(user) {
		return false, nil
	}
	switch runtime.GOOS {
	case "linux":
		if err := removeSystemdUnit(user, legacySystemdUnit, legacyServiceName); err != nil {
			return false, err
		}
	case "darwin":
		if err := removeLaunchdPlist(legacyLaunchdPlistPath(user)); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	mgr, err := newServiceManager()
	if err != nil {
		return false, err
	}
	binaryPath, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("cannot detect binary path: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(binaryPath); rerr == nil {
		binaryPath = resolved
	}
	configPath := newConfigPath
	// noStart is left false: migrate removes the old service and activates the
	// new one in a single step.
	return true, mgr.install(installOpts{
		user:       user,
		configPath: configPath,
		binaryPath: binaryPath,
		backend:    detectBackend(configPath),
	})
}

// migrateVolume removes the legacy shared docker volume. Best-effort; returns
// whether it removed anything.
func migrateVolume(ctx context.Context) bool {
	client, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return false
	}
	defer client.Close()
	if _, err := client.VolumeInspect(ctx, legacySharedVolume); err != nil {
		return false
	}
	rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.VolumeRemove(rmCtx, legacySharedVolume, true); err != nil {
		warnLegacy("found legacy volume %s but could not remove it: %v", legacySharedVolume, err)
		return false
	}
	return true
}
