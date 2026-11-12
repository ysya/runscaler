package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

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

// configMissing reports that no config file was found and nothing else,
// such as `runner run` flags, configured a scale set: a command that checks
// the configuration would only check defaults.
func configMissing(cfg config.Config) bool {
	if viper.ConfigFileUsed() != "" {
		return false
	}
	for _, ss := range cfg.ResolveScaleSets() {
		if ss.RegistrationURL != "" {
			return false
		}
	}
	return true
}

// searchedConfigFiles lists, in search order, the files runner looks for
// when no config is given, or nil when they cannot be resolved.
func searchedConfigFiles() []string {
	dirs, err := configSearchPaths()
	if err != nil {
		return nil
	}
	files := make([]string, len(dirs))
	for i, dir := range dirs {
		files[i] = filepath.Join(dir, "config.toml")
		if dir == "." {
			files[i] = "./config.toml"
		}
	}
	return files
}

// missingConfigError explains that no config file was found and how to give
// runner one.
const configBuilderURL = "https://runscaler.ysya.dev/configure/"

func missingConfigError() error {
	msg := "no config file found"
	if files := searchedConfigFiles(); len(files) > 0 {
		msg += " (searched " + strings.Join(files, ", ") + ")"
	}
	return errors.New(msg + "\n\n  Build config.toml: " + configBuilderURL + "\n  Or pass its path: --config PATH")
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
