package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

// loadConfig reads the configuration from the config file (if any) and
// unmarshals all sources (flag > config file > default) into a Config.
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
		viper.AddConfigPath("/etc/runscaler")
		viper.ReadInConfig() // ignore error — default paths are optional
	}

	var cfg config.Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return config.Config{}, fmt.Errorf("failed to parse configuration: %w", err)
	}
	return cfg, nil
}
