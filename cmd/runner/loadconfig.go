package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

// loadConfig reads the configuration from the config file (if any) and
// builds a Config from all sources (flag > config file > default) via
// config.Load, which also collects non-fatal diagnostics into cfg.Warnings.
// Both the root command and validate subcommand use this helper.
func loadConfig(cmd *cobra.Command) (config.Config, error) {
	if configFile, _ := cmd.Flags().GetString("config"); configFile != "" {
		viper.SetConfigFile(configFile)
		if err := viper.ReadInConfig(); err != nil {
			return config.Config{}, fmt.Errorf("failed to read config file: %w", err)
		}
	} else {
		viper.SetConfigName("config")
		viper.SetConfigType("toml")
		viper.AddConfigPath(".")
		viper.AddConfigPath("/etc/runner")
		viper.AddConfigPath(legacyConfigDir) // legacy /etc/runscaler — deprecated
		if err := viper.ReadInConfig(); err != nil {
			// A missing config is fine (single-mode via flags); anything else
			// (parse error, permission denied) must surface.
			var notFound viper.ConfigFileNotFoundError
			if !errors.As(err, &notFound) {
				return config.Config{}, fmt.Errorf("failed to read config file: %w", err)
			}
		}

		if used := viper.ConfigFileUsed(); used != "" && filepath.Dir(used) == filepath.Clean(legacyConfigDir) {
			warnLegacy("config loaded from legacy path %s — run 'runner migrate' or move it to /etc/runner", used)
		}
	}

	// Provider flags are copied only when explicitly set. Binding both the
	// canonical and legacy names to Viper would make --provider's default
	// "docker" look explicit and override an existing backend = "tart"
	// config before the alias layer can preserve it.
	providerFlag := cmd.Flags().Lookup("provider")
	backendFlag := cmd.Flags().Lookup("backend")
	providerChanged := providerFlag != nil && providerFlag.Changed
	backendChanged := backendFlag != nil && backendFlag.Changed
	if providerChanged && backendChanged {
		return config.Config{}, fmt.Errorf("--provider and deprecated --backend cannot be used together")
	}
	if providerChanged {
		viper.Set("provider", providerFlag.Value.String())
	}
	if backendChanged {
		viper.Set("backend", backendFlag.Value.String())
	}

	cfg, err := config.Load(viper.GetViper())
	if err != nil {
		return config.Config{}, fmt.Errorf("failed to parse configuration: %w", err)
	}
	return cfg, nil
}

// resolveLogFilePath applies the documented config-relative default. A nil
// LogFile means no explicit setting; a pointer to "" explicitly disables it.
func resolveLogFilePath(cfg config.Config) (string, bool) {
	if cfg.LogFile != nil {
		return *cfg.LogFile, *cfg.LogFile != ""
	}
	if used := viper.ConfigFileUsed(); used != "" {
		return filepath.Join(filepath.Dir(used), config.DefaultLogFileName), true
	}
	return config.DefaultLogFileName, true
}
