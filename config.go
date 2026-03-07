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

// Config holds all configuration for runscaler.
type Config struct {
	RegistrationURL string   `mapstructure:"url"`
	ScaleSetName    string   `mapstructure:"name"`
	Token           string   `mapstructure:"token"`
	MaxRunners      int      `mapstructure:"max-runners"`
	MinRunners      int      `mapstructure:"min-runners"`
	Labels          []string `mapstructure:"labels"`
	RunnerGroup     string   `mapstructure:"runner-group"`
	RunnerImage     string   `mapstructure:"runner-image"`
	DockerSocket    string   `mapstructure:"docker-socket"`
	DinD            bool     `mapstructure:"dind"`
	WorkDirBase     string   `mapstructure:"work-dir"`
	LogLevel        string   `mapstructure:"log-level"`
	LogFormat       string   `mapstructure:"log-format"`
}

// Validate checks required fields and logical constraints.
func (c *Config) Validate() error {
	if c.RegistrationURL == "" {
		return fmt.Errorf("registration URL (--url) is required")
	}
	if _, err := url.ParseRequestURI(c.RegistrationURL); err != nil {
		return fmt.Errorf("invalid registration URL: %w", err)
	}
	if c.ScaleSetName == "" {
		return fmt.Errorf("scale set name (--name) is required")
	}
	if c.Token == "" {
		return fmt.Errorf("token (--token or config file) is required")
	}
	if c.MinRunners < 0 {
		return fmt.Errorf("min-runners must be >= 0")
	}
	if c.MaxRunners < 1 {
		return fmt.Errorf("max-runners must be >= 1")
	}
	if c.MinRunners > c.MaxRunners {
		return fmt.Errorf("min-runners (%d) must be <= max-runners (%d)", c.MinRunners, c.MaxRunners)
	}
	return nil
}

// ScalesetClient creates a scaleset.Client using PAT authentication.
// A custom retryablehttp client is used to override its default logger,
// which otherwise prints unformatted [DEBUG] lines to stderr.
func (c *Config) ScalesetClient(logger *slog.Logger) (*scaleset.Client, error) {
	httpClient := retryablehttp.NewClient()
	httpClient.Logger = nil // suppress noisy "performing request" debug lines

	client, err := scaleset.NewClientWithPersonalAccessToken(
		scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     c.RegistrationURL,
			PersonalAccessToken: c.Token,
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
func (c *Config) BuildLabels() []scaleset.Label {
	labels := c.Labels
	if len(labels) == 0 {
		labels = []string{c.ScaleSetName}
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
