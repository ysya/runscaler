package main

import (
	"context"
	"fmt"
	"io"
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
	Long: `Copy config from /etc/runscaler to /etc/runner (the legacy file is
removed only after the service migration succeeds), reinstall the service under
the new name, and clean up the old docker volume. Idempotent — safe to run
multiple times; a fresh install has nothing to migrate.`,
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

	copied, err := migrateConfig()
	if err != nil {
		return fmt.Errorf("config migration failed: %w", err)
	}
	if copied {
		fmt.Printf("  ✓ Copied config %s → %s\n", legacyConfigPath, newConfigPath)
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

	// Service migration succeeded (or there was none) — now safe to remove the
	// legacy config, but only if the new config is in place. Stat-based so a
	// re-run also cleans it up.
	if _, lerr := os.Stat(legacyConfigPath); lerr == nil {
		if _, nerr := os.Stat(newConfigPath); nerr == nil {
			if err := os.Remove(legacyConfigPath); err == nil {
				fmt.Printf("  ✓ Removed legacy config %s\n", legacyConfigPath)
				did = true
			}
		}
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

// copyFile copies src to dst. dst must not already exist (O_EXCL).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// migrateConfig copies the legacy config to the new path, KEEPING the legacy
// file so an old service still works if a later step fails. runMigrate removes
// the legacy config only after the service migration succeeds. Returns whether
// it copied. No-op if legacy absent or target already exists.
func migrateConfig() (bool, error) {
	if _, err := os.Stat(legacyConfigPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if _, err := os.Stat(newConfigPath); err == nil {
		warnLegacy("legacy config %s exists but %s is already present — leaving both, not overwriting", legacyConfigPath, newConfigPath)
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(newConfigPath), 0o755); err != nil {
		return false, err
	}
	if err := copyFile(legacyConfigPath, newConfigPath); err != nil {
		return false, err
	}
	return true, nil
}

// migrateService migrates the service atomically: install the new service
// (without starting) BEFORE removing the legacy one, so a failure leaves the
// OLD service intact and the migration re-runnable. Returns whether it acted.
func migrateService(user bool) (bool, error) {
	if !legacyServiceInstalled(user) {
		// If the new service is installed but a previous migration's start step
		// failed, ensure it's running (idempotent). Otherwise nothing to migrate.
		if newServiceInstalled(user) {
			if mgr, err := newServiceManager(); err == nil {
				_ = mgr.start(user)
			}
		}
		return false, nil
	}
	mgr, err := newServiceManager()
	if err != nil {
		return false, err
	}
	// 1. Install the new service (not started) if not already present. On
	//    failure the legacy service is untouched — host keeps running, re-runnable.
	if !newServiceInstalled(user) {
		binaryPath, err := os.Executable()
		if err != nil {
			return false, fmt.Errorf("cannot detect binary path: %w", err)
		}
		if resolved, rerr := filepath.EvalSymlinks(binaryPath); rerr == nil {
			binaryPath = resolved
		}
		if err := mgr.install(installOpts{
			user:       user,
			configPath: newConfigPath,
			binaryPath: binaryPath,
			backend:    detectBackend(newConfigPath),
			noStart:    true,
		}); err != nil {
			return false, err
		}
	}
	// 2. Remove the legacy service now that the new one is in place.
	switch runtime.GOOS {
	case "linux":
		if err := removeSystemdUnit(user, legacySystemdUnit, legacyServiceName); err != nil {
			return true, err
		}
	case "darwin":
		if err := removeLaunchdPlist(legacyLaunchdPlistPath(user)); err != nil {
			return true, err
		}
	default:
		return true, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	// 3. Start the new service.
	if err := mgr.start(user); err != nil {
		return true, fmt.Errorf("new service installed but failed to start (run `runner service start` to retry): %w", err)
	}
	return true, nil
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
