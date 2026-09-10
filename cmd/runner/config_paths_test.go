package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestUserConfigOperations(t *testing.T) {
	for _, location := range []string{"xdg", "default"} {
		t.Run(location, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
			base := filepath.Join(home, "xdg")
			if location == "default" {
				t.Setenv("XDG_CONFIG_HOME", "relative-is-invalid")
				base = filepath.Join(home, ".config")
			}
			path := filepath.Join(base, "runner", "config.toml")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("url = 'https://github.com/test'\nname = 'user-config'\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(t.TempDir())
			viper.Reset()
			t.Cleanup(viper.Reset)
			c := &cobra.Command{}
			c.PersistentFlags().String("config", "", "")
			c.Flags().Bool("user", true, "")
			c.Flags().String("config-path", "", "")
			cfg, err := loadConfig(c)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ResolveScaleSets()[0].ScaleSetName != "user-config" {
				t.Errorf("user config was not loaded: %s", viper.ConfigFileUsed())
			}
			got, err := resolveConfigPath(c)
			if err != nil {
				t.Fatal(err)
			}
			if got != path {
				t.Errorf("service config = %q, want %q", got, path)
			}
		})
	}
}

func TestMigrationDiscoversXDGUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	base := filepath.Join(home, "xdg")
	source := filepath.Join(base, "runscaler", "config.toml")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("name = 'legacy'"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &cobra.Command{}
	c.Flags().String("config", "", "")
	c.Flags().String("backup-dir", "", "")
	paths, err := resolveConfigMigrationPaths(c, true)
	if err != nil {
		t.Fatal(err)
	}
	if paths.Source != source || paths.Target != filepath.Join(base, "runner", "config.toml") || !paths.RemoveSource {
		t.Fatalf("migration paths = %+v", paths)
	}
}

func TestPlatformServiceScopeDefaults(t *testing.T) {
	for _, c := range []*cobra.Command{migrateCmd, serviceInstallCmd, serviceUninstallCmd, serviceStartCmd, serviceStopCmd, serviceRestartCmd, serviceStatusCmd, serviceLogsCmd} {
		user, err := c.Flags().GetBool("user")
		if err != nil {
			t.Fatal(err)
		}
		if user != (runtime.GOOS == "darwin") {
			t.Errorf("%s user default = %v", c.Name(), user)
		}
	}
}

func TestServiceConfigPrecedence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Chdir(t.TempDir())
	if err := os.WriteFile("config.toml", []byte("name = 'local'"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &cobra.Command{}
	c.PersistentFlags().String("config", "", "")
	c.Flags().String("config-path", "", "")
	c.Flags().Bool("user", true, "")
	for _, tc := range []struct{ flag, value, want string }{
		{"", "", "config.toml"},
		{"config", "custom.toml", "custom.toml"},
		{"config-path", "service.toml", "service.toml"},
	} {
		if tc.flag != "" {
			flags := c.Flags()
			if tc.flag == "config" {
				flags = c.PersistentFlags()
			}
			if err := flags.Set(tc.flag, tc.value); err != nil {
				t.Fatal(err)
			}
		}
		got, err := resolveConfigPath(c)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := filepath.Abs(tc.want)
		if got != want {
			t.Fatalf("%s: got %s want %s", tc.flag, got, want)
		}
	}
}

func TestUserMigrationCopiesSystemConfigWithoutRemovingSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	oldLegacy, oldNew := legacyConfigPath, newConfigPath
	legacyConfigPath = filepath.Join(t.TempDir(), "missing.toml")
	newConfigPath = filepath.Join(t.TempDir(), "config.toml")
	t.Cleanup(func() { legacyConfigPath, newConfigPath = oldLegacy, oldNew })
	if err := os.WriteFile(newConfigPath, validLegacyConfig(), 0600); err != nil {
		t.Fatal(err)
	}
	c := &cobra.Command{}
	c.Flags().String("config", "", "")
	c.Flags().String("backup-dir", "", "")
	paths, err := resolveConfigMigrationPaths(c, true)
	if err != nil {
		t.Fatal(err)
	}
	if paths.Source != newConfigPath || paths.RemoveSource {
		t.Fatalf("system source must be copied and preserved: %+v", paths)
	}
	result, err := migrateConfigFile(paths, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || !result.TargetCreated || result.RemoveSource {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(newConfigPath); err != nil {
		t.Fatal(err)
	}
}
