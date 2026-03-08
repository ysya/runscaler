package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/actions/scaleset"
	charmlog "github.com/charmbracelet/log"
	"github.com/hashicorp/go-retryablehttp"
)

// Config holds global configuration for runscaler.
type Config struct {
	// Global settings
	DockerSocket string `mapstructure:"docker-socket"`
	DinD         bool   `mapstructure:"dind"`
	SharedVolume string `mapstructure:"shared-volume"`
	LogLevel     string `mapstructure:"log-level"`
	LogFormat    string `mapstructure:"log-format"`

	// Scale sets (multi-org support)
	ScaleSets []ScaleSetConfig `mapstructure:"scaleset"`

	// Legacy top-level fields for single scale set / CLI mode
	RegistrationURL string   `mapstructure:"url"`
	ScaleSetName    string   `mapstructure:"name"`
	Token           string   `mapstructure:"token"`
	MaxRunners      int      `mapstructure:"max-runners"`
	MinRunners      int      `mapstructure:"min-runners"`
	Labels          []string `mapstructure:"labels"`
	RunnerGroup     string   `mapstructure:"runner-group"`
	RunnerImage     string   `mapstructure:"runner-image"`
}

// ScaleSetConfig holds per-scale-set configuration.
// Fields left at their zero value inherit from the global Config.
type ScaleSetConfig struct {
	RegistrationURL string   `mapstructure:"url"`
	ScaleSetName    string   `mapstructure:"name"`
	Token           string   `mapstructure:"token"`
	MaxRunners      int      `mapstructure:"max-runners"`
	MinRunners      int      `mapstructure:"min-runners"`
	Labels          []string `mapstructure:"labels"`
	RunnerGroup     string   `mapstructure:"runner-group"`
	RunnerImage     string   `mapstructure:"runner-image"`
	SharedVolume    string   `mapstructure:"shared-volume"`
	DockerSocket    string   `mapstructure:"docker-socket"`
	DinD            *bool    `mapstructure:"dind"` // pointer to distinguish "not set" from "false"
}

// ResolveScaleSets returns the list of scale set configs to run.
// If [[scaleset]] entries exist, use them. Otherwise fall back to
// top-level fields (single scale set / CLI mode).
func (c *Config) ResolveScaleSets() []ScaleSetConfig {
	if len(c.ScaleSets) > 0 {
		// Apply defaults from global config to each scale set
		for i := range c.ScaleSets {
			ss := &c.ScaleSets[i]
			if ss.RunnerImage == "" {
				ss.RunnerImage = c.RunnerImage
			}
			if ss.RunnerGroup == "" {
				ss.RunnerGroup = c.RunnerGroup
			}
			if ss.MaxRunners == 0 {
				ss.MaxRunners = c.MaxRunners
			}
			if ss.SharedVolume == "" {
				ss.SharedVolume = c.SharedVolume
			}
			if ss.DockerSocket == "" {
				ss.DockerSocket = c.DockerSocket
			}
			if ss.DinD == nil {
				ss.DinD = &c.DinD
			}
			ss.resolveEnvToken()
		}
		return c.ScaleSets
	}

	// Legacy single scale set mode
	ss := ScaleSetConfig{
		RegistrationURL: c.RegistrationURL,
		ScaleSetName:    c.ScaleSetName,
		Token:           c.Token,
		MaxRunners:      c.MaxRunners,
		MinRunners:      c.MinRunners,
		Labels:          c.Labels,
		RunnerGroup:     c.RunnerGroup,
		RunnerImage:     c.RunnerImage,
		SharedVolume:    c.SharedVolume,
		DockerSocket:    c.DockerSocket,
		DinD:            &c.DinD,
	}
	ss.resolveEnvToken()
	return []ScaleSetConfig{ss}
}

// IsDinD returns whether Docker-in-Docker is enabled for this scale set.
func (ss *ScaleSetConfig) IsDinD() bool {
	if ss.DinD != nil {
		return *ss.DinD
	}
	return true // default
}

// resolveEnvToken resolves the token value from environment variables.
// Supports two patterns:
//   - token = "env:VARIABLE_NAME" — reads from the named env var
//   - Empty token with RUNSCALER_TOKEN env var set — uses that as fallback
func (ss *ScaleSetConfig) resolveEnvToken() {
	if strings.HasPrefix(ss.Token, "env:") {
		envName := strings.TrimPrefix(ss.Token, "env:")
		ss.Token = os.Getenv(envName)
		return
	}
	if ss.Token == "" {
		if v := os.Getenv("RUNSCALER_TOKEN"); v != "" {
			ss.Token = v
		}
	}
}

// Validate checks required fields and logical constraints for a scale set.
func (ss *ScaleSetConfig) Validate() error {
	if ss.RegistrationURL == "" {
		return fmt.Errorf("registration URL (url) is required")
	}
	if _, err := url.ParseRequestURI(ss.RegistrationURL); err != nil {
		return fmt.Errorf("invalid registration URL: %w", err)
	}
	if ss.ScaleSetName == "" {
		return fmt.Errorf("scale set name (name) is required")
	}
	if ss.Token == "" {
		return fmt.Errorf("token is required")
	}
	if ss.MinRunners < 0 {
		return fmt.Errorf("min-runners must be >= 0")
	}
	if ss.MaxRunners < 1 {
		return fmt.Errorf("max-runners must be >= 1")
	}
	if ss.MinRunners > ss.MaxRunners {
		return fmt.Errorf("min-runners (%d) must be <= max-runners (%d)", ss.MinRunners, ss.MaxRunners)
	}
	return nil
}

// ScalesetClient creates a scaleset.Client using PAT authentication.
// A custom retryablehttp client is used to override its default logger,
// which otherwise prints unformatted [DEBUG] lines to stderr.
func (ss *ScaleSetConfig) ScalesetClient(logger *slog.Logger) (*scaleset.Client, error) {
	httpClient := retryablehttp.NewClient()
	httpClient.Logger = nil // suppress noisy "performing request" debug lines

	client, err := scaleset.NewClientWithPersonalAccessToken(
		scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     ss.RegistrationURL,
			PersonalAccessToken: ss.Token,
		},
		scaleset.WithLogger(logger),
		scaleset.WithRetryableHTTPClint(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create scaleset client: %w", err)
	}
	return client, nil
}

// Logger creates a structured logger using charmbracelet/log and sets it
// as the process-wide default. This unifies all logging output:
// slog calls, Go standard log calls, and any library using either.
func (c *Config) Logger() *slog.Logger {
	var level charmlog.Level
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		level = charmlog.DebugLevel
	case "warn":
		level = charmlog.WarnLevel
	case "error":
		level = charmlog.ErrorLevel
	default:
		level = charmlog.InfoLevel
	}

	opts := charmlog.Options{
		ReportTimestamp: true,
		TimeFormat:      time.DateTime,
		Level:           level,
	}

	if strings.ToLower(c.LogFormat) == "json" {
		opts.Formatter = charmlog.JSONFormatter
	}

	logger := slog.New(charmlog.NewWithOptions(os.Stdout, opts))

	// Set as process-wide default: unifies slog.Info(), log.Println(), etc.
	slog.SetDefault(logger)

	return logger
}

// BuildLabels converts string labels to scaleset.Label slice.
// If no labels are provided, uses the scale set name as default.
func (ss *ScaleSetConfig) BuildLabels() []scaleset.Label {
	labels := ss.Labels
	if len(labels) == 0 {
		labels = []string{ss.ScaleSetName}
	}

	result := make([]scaleset.Label, len(labels))
	for i, l := range labels {
		result[i] = scaleset.Label{
			Name: l,
			Type: "User",
		}
	}
	return result
}

// systemInfo returns metadata for the scaleset client user agent.
func systemInfo(scaleSetID int) scaleset.SystemInfo {
	return scaleset.SystemInfo{
		System:     "dockerscaleset",
		Version:    "0.1.0",
		ScaleSetID: scaleSetID,
	}
}
