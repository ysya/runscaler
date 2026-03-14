package config

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/actions/scaleset"
	charmlog "github.com/charmbracelet/log"
	"github.com/charmbracelet/lipgloss"
	"github.com/hashicorp/go-retryablehttp"
)

// Config holds the complete runscaler configuration.
//
// Global-only fields (LogLevel, LogFormat, HealthPort, DryRun) are never
// inherited by scale sets. The Defaults field is squashed so its keys
// appear at the TOML top level; it doubles as the single-mode config
// when no [[scaleset]] entries exist.
type Config struct {
	// Global settings (not inherited by scale sets)
	LogLevel   string `mapstructure:"log-level"`
	LogFormat  string `mapstructure:"log-format"`
	HealthPort int    `mapstructure:"health-port"`
	DryRun     bool   `mapstructure:"dry-run"`

	// Default values for scale sets + single-mode fields.
	// Squashed so TOML keys (url, name, backend, etc.) stay at the top level.
	Defaults ScaleSetConfig `mapstructure:",squash"`

	// Multi-scaleset mode: each entry inherits from Defaults.
	ScaleSets []ScaleSetConfig `mapstructure:"scaleset"`
}

// ScaleSetConfig holds per-scale-set configuration.
// In multi-mode, fields left at their zero value inherit from Config.Defaults.
type ScaleSetConfig struct {
	RegistrationURL string   `mapstructure:"url"`
	ScaleSetName    string   `mapstructure:"name"`
	Token           string   `mapstructure:"token"`
	MaxRunners      int      `mapstructure:"max-runners"`
	MinRunners      int      `mapstructure:"min-runners"`
	Labels          []string `mapstructure:"labels"`
	RunnerGroup     string   `mapstructure:"runner-group"`
	RunnerImage     string   `mapstructure:"runner-image"`
	Backend         string   `mapstructure:"backend"`

	// Docker backend settings
	Docker DockerConfig `mapstructure:"docker"`

	// Tart VM backend settings
	Tart TartConfig `mapstructure:"tart"`
}

// DockerConfig holds Docker-specific backend settings.
type DockerConfig struct {
	Socket       string `mapstructure:"socket"`
	DinD         *bool  `mapstructure:"dind"`          // pointer: nil = inherit default (true)
	SharedVolume string `mapstructure:"shared-volume"`
	Memory       int    `mapstructure:"memory"`        // Memory limit in MB (0 = unlimited)
	CPU          int    `mapstructure:"cpu"`            // CPU cores (0 = unlimited)
	Platform     string `mapstructure:"platform"`      // e.g. "linux/amd64" to force architecture
}

// TartConfig holds Tart VM-specific backend settings.
type TartConfig struct {
	RunnerDir string `mapstructure:"runner-dir"` // Runner binary path in VM
	CPU       int    `mapstructure:"cpu"`         // Number of CPU cores (0 = use image default)
	Memory    int    `mapstructure:"memory"`      // Memory in MB (0 = use image default)
	PoolSize  int    `mapstructure:"pool-size"`   // Pre-warmed VM count (0 = disabled)
}

// ResolveScaleSets returns the resolved list of scale set configs.
// If [[scaleset]] entries exist, each inherits unset fields from Defaults.
// Otherwise, Defaults itself is returned as a single-element slice.
func (c *Config) ResolveScaleSets() []ScaleSetConfig {
	if len(c.ScaleSets) > 0 {
		for i := range c.ScaleSets {
			mergeDefaults(&c.ScaleSets[i], &c.Defaults)
		}
		return c.ScaleSets
	}

	// Single scale set mode: use Defaults directly.
	ss := c.Defaults
	ss.applyDefaults()
	ss.resolveEnvToken()
	return []ScaleSetConfig{ss}
}

// mergeDefaults fills zero-valued fields in dst from defaults.
// Identity fields (URL, Name, Token, Labels) are never inherited.
// MinRunners is not inherited because 0 is a valid explicit value.
func mergeDefaults(dst *ScaleSetConfig, defaults *ScaleSetConfig) {
	// Common
	if dst.RunnerImage == "" {
		dst.RunnerImage = defaults.RunnerImage
	}
	if dst.RunnerGroup == "" {
		dst.RunnerGroup = defaults.RunnerGroup
	}
	if dst.MaxRunners == 0 {
		dst.MaxRunners = defaults.MaxRunners
	}
	if dst.Backend == "" {
		dst.Backend = defaults.Backend
	}

	// Docker
	if dst.Docker.Socket == "" {
		dst.Docker.Socket = defaults.Docker.Socket
	}
	if dst.Docker.DinD == nil {
		dst.Docker.DinD = defaults.Docker.DinD
	}
	if dst.Docker.SharedVolume == "" {
		dst.Docker.SharedVolume = defaults.Docker.SharedVolume
	}
	if dst.Docker.Memory == 0 {
		dst.Docker.Memory = defaults.Docker.Memory
	}
	if dst.Docker.CPU == 0 {
		dst.Docker.CPU = defaults.Docker.CPU
	}
	if dst.Docker.Platform == "" {
		dst.Docker.Platform = defaults.Docker.Platform
	}

	// Tart
	if dst.Tart.RunnerDir == "" {
		dst.Tart.RunnerDir = defaults.Tart.RunnerDir
	}
	if dst.Tart.CPU == 0 {
		dst.Tart.CPU = defaults.Tart.CPU
	}
	if dst.Tart.Memory == 0 {
		dst.Tart.Memory = defaults.Tart.Memory
	}
	if dst.Tart.PoolSize == 0 {
		dst.Tart.PoolSize = defaults.Tart.PoolSize
	}

	dst.applyDefaults()
	dst.resolveEnvToken()
}

// applyDefaults fills in backend-specific defaults that depend on
// the backend selection (e.g. TartRunnerDir when backend is "tart").
func (ss *ScaleSetConfig) applyDefaults() {
	if ss.Backend == "" {
		ss.Backend = DefaultBackend
	}
	if ss.Backend == "tart" && ss.Tart.RunnerDir == "" {
		ss.Tart.RunnerDir = DefaultTartRunnerDir
	}
}

// IsDinD returns whether Docker-in-Docker is enabled for this scale set.
func (ss *ScaleSetConfig) IsDinD() bool {
	if ss.Docker.DinD != nil {
		return *ss.Docker.DinD
	}
	return DefaultDinD
}

// IsTart returns whether this scale set uses the Tart VM backend.
func (ss *ScaleSetConfig) IsTart() bool {
	return ss.Backend == "tart"
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

	switch ss.Backend {
	case DefaultBackend:
		// Docker backend: no additional validation
	case "tart":
		if ss.RunnerImage == "" {
			return fmt.Errorf("runner-image is required when backend is \"tart\"")
		}
	default:
		return fmt.Errorf("unsupported backend %q (must be %q or \"tart\")", ss.Backend, DefaultBackend)
	}

	return nil
}

// --- Standalone utility functions (not struct methods) ---

// parseLogLevel converts a log level string to a charmlog.Level.
func parseLogLevel(level string) charmlog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return charmlog.DebugLevel
	case "warn":
		return charmlog.WarnLevel
	case "error":
		return charmlog.ErrorLevel
	default:
		return charmlog.InfoLevel
	}
}

// NewLogger creates a structured logger with the given level and format,
// and sets it as the process-wide default.
func NewLogger(level, format string) *slog.Logger {
	opts := charmlog.Options{
		ReportTimestamp: true,
		TimeFormat:      time.DateTime,
		Level:           parseLogLevel(level),
	}

	if strings.ToLower(format) == "json" {
		opts.Formatter = charmlog.JSONFormatter
	}

	handler := charmlog.NewWithOptions(os.Stdout, opts)
	logger := slog.New(&demoteHandler{inner: handler, demote: demoteMessages})
	slog.SetDefault(logger)
	return logger
}

// scaleSetColors is a 5-color palette for distinguishing scale sets in logs.
// Red is excluded to avoid confusion with error output (same rationale as stern).
// Black/white excluded for readability on dark/light terminal backgrounds.
var scaleSetColors = []lipgloss.Color{
	lipgloss.Color("6"), // cyan
	lipgloss.Color("3"), // yellow
	lipgloss.Color("2"), // green
	lipgloss.Color("5"), // magenta
	lipgloss.Color("4"), // blue
}

// NewScaleSetLogger creates a logger with a colored prefix for the given scale set.
// The color is determined by the index, cycling through the palette.
func NewScaleSetLogger(level, format string, name string, index int) *slog.Logger {
	opts := charmlog.Options{
		ReportTimestamp: true,
		TimeFormat:      time.DateTime,
		Level:           parseLogLevel(level),
		Prefix:          name,
	}

	if strings.ToLower(format) == "json" {
		opts.Formatter = charmlog.JSONFormatter
	}

	handler := charmlog.NewWithOptions(os.Stdout, opts)

	// Apply color only for text format (not JSON)
	if strings.ToLower(format) != "json" {
		styles := charmlog.DefaultStyles()
		color := scaleSetColors[index%len(scaleSetColors)]
		styles.Prefix = lipgloss.NewStyle().Foreground(color).Bold(true)
		handler.SetStyles(styles)
	}

	return slog.New(&demoteHandler{inner: handler, demote: demoteMessages})
}

// demoteMessages lists log messages from upstream libraries that are too noisy
// at Info level. These are demoted to Debug so they only appear with log-level=debug.
var demoteMessages = map[string]bool{
	"Getting next message": true, // actions/scaleset listener polling loop
}

// demoteHandler wraps a slog.Handler and downgrades specific messages from Info to Debug.
type demoteHandler struct {
	inner  slog.Handler
	demote map[string]bool
}

func (h *demoteHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *demoteHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelInfo && h.demote[r.Message] {
		r.Level = slog.LevelDebug
		if !h.inner.Enabled(ctx, slog.LevelDebug) {
			return nil
		}
	}
	return h.inner.Handle(ctx, r)
}

func (h *demoteHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &demoteHandler{inner: h.inner.WithAttrs(attrs), demote: h.demote}
}

func (h *demoteHandler) WithGroup(name string) slog.Handler {
	return &demoteHandler{inner: h.inner.WithGroup(name), demote: h.demote}
}

// NewScalesetClient creates a scaleset.Client using PAT authentication.
func NewScalesetClient(registrationURL, token string, logger *slog.Logger) (*scaleset.Client, error) {
	httpClient := retryablehttp.NewClient()
	httpClient.Logger = nil // suppress noisy "performing request" debug lines

	client, err := scaleset.NewClientWithPersonalAccessToken(
		scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     registrationURL,
			PersonalAccessToken: token,
		},
		scaleset.WithLogger(logger),
		scaleset.WithRetryableHTTPClint(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create scaleset client: %w", err)
	}
	return client, nil
}

// BuildLabels converts string labels to scaleset.Label slice.
// If no labels are provided, uses the scale set name as default.
func BuildLabels(name string, labels []string) []scaleset.Label {
	if len(labels) == 0 {
		labels = []string{name}
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

// NewSystemInfo returns metadata for the scaleset client user agent.
func NewSystemInfo(scaleSetID int, version string) scaleset.SystemInfo {
	return scaleset.SystemInfo{
		System:     DefaultSystemName,
		Version:    version,
		ScaleSetID: scaleSetID,
	}
}
