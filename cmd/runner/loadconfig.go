package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

// loadConfig reads the configuration from the config file (if any) and
// builds a Config from all sources (flag > config file > default) via
// config.Load, which also collects non-fatal diagnostics into cfg.Warnings.
// Every command that reads the config goes through this helper.
func loadConfig(cmd *cobra.Command) (config.Config, error) {
	explicit, _ := cmd.Flags().GetString("config")
	configPath, err := managedConfigPath(cmd, explicit)
	if err != nil {
		return config.Config{}, err
	}
	return loadConfigAtPath(cmd, configPath)
}

func loadConfigAtPath(cmd *cobra.Command, configPath string) (config.Config, error) {
	// Only explicitly passed App flags select App mode. Binding their zero
	// defaults would manufacture an empty App config for existing PAT users.
	bindGitHubAppFlags(viper.GetViper(), cmd.Flags())
	if configFile := configPath; configFile != "" {
		viper.SetConfigFile(configFile)
		if err := viper.ReadInConfig(); err != nil {
			return config.Config{}, fmt.Errorf("failed to read config file: %w", err)
		}
	} else {
		viper.SetConfigName("config")
		viper.SetConfigType("toml")
		paths, err := configSearchPaths()
		if err != nil {
			return config.Config{}, err
		}
		for _, path := range paths {
			viper.AddConfigPath(path)
		}
		if err := viper.ReadInConfig(); err != nil {
			// A missing config is fine (single-mode via flags); anything else
			// (parse error, permission denied) must surface.
			var notFound viper.ConfigFileNotFoundError
			if !errors.As(err, &notFound) {
				return config.Config{}, fmt.Errorf("failed to read config file: %w", err)
			}
		}

	}

	if providerFlag := cmd.Flags().Lookup("provider"); providerFlag != nil && providerFlag.Changed {
		viper.Set("provider", providerFlag.Value.String())
	}

	cfg, err := config.Load(viper.GetViper())
	if err != nil {
		return config.Config{}, fmt.Errorf("failed to parse configuration: %w", err)
	}
	return cfg, nil
}
