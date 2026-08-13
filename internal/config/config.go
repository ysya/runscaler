package config

import (
	"context"
	"fmt"
	"image/color"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	charmlog "charm.land/log/v2"
	"github.com/actions/scaleset"
	"github.com/hashicorp/go-retryablehttp"
)

// Config holds the complete runner configuration.
//
// Global-only fields (LogLevel, LogFormat, HealthPort, DryRun) are never
// inherited by scale sets. The Defaults field is squashed so its keys
// appear at the TOML top level; it doubles as the single-mode config
// when no [[scaleset]] entries exist.
type Config struct {
	// Global settings (not inherited by scale sets)
	LogLevel      string  `mapstructure:"log-level"`
	LogFormat     string  `mapstructure:"log-format"`
	HealthPort    int     `mapstructure:"health-port"`
	HealthAddress string  `mapstructure:"health-address"`
	DryRun        bool    `mapstructure:"dry-run"`
	LogFile       *string `mapstructure:"log-file"`

	// Default values for scale sets + single-mode fields.
	// Squashed so TOML keys (url, name, backend, etc.) stay at the top level.
	Defaults ScaleSetConfig `mapstructure:",squash"`

	// Disk configures the periodic disk-pressure guard (see DiskConfig).
	// Global like the fields above: one guard watches every scale set's
	// stores host-wide, grouped by filesystem rather than by scale set.
	Disk DiskConfig `mapstructure:"disk"`

	// Multi-scaleset mode: entries as produced by Load, with map-level
	// inheritance from the top-level defaults already applied.
	ScaleSets []ScaleSetConfig `mapstructure:"scaleset"`

	// Warnings carries non-fatal load diagnostics (unknown config keys,
	// single-mode keys mixed with [[scaleset]] entries) surfaced by callers:
	// `runner run` logs them, `runner validate` fails on them.
	Warnings []string `mapstructure:"-"`
}

// ScaleSetConfig holds per-scale-set configuration.
// In multi mode, inheritance from the top-level defaults happens in Load at
// the map level: a key present in a [[scaleset]] entry always wins, even
// when set to a zero value.
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

	// DisableUpdate stops GitHub from updating the runner binary inside the
	// container or VM. Pointer: nil = inherit default (true), matching the
	// image-based model — the runner is refreshed by rebuilding the image, and
	// ephemeral runners skip a download on every job.
	//
	// Set false when the image's bundled runner cannot be refreshed easily
	// (e.g. a 140GB macOS VM image). GitHub retires old runner versions
	// server-side, and a runner that is both outdated and forbidden to update
	// connects, is refused ("Runner version vX is deprecated and cannot
	// receive messages"), then exits — jobs sit queued behind a scale set that
	// still looks healthy. Allowing the update trades a per-job download for
	// self-healing across retirements.
	DisableUpdate *bool `mapstructure:"disable-update"`

	// Docker backend settings
	Docker DockerConfig `mapstructure:"docker"`

	// Tart VM backend settings
	Tart TartConfig `mapstructure:"tart"`
}

// DockerConfig holds Docker-specific backend settings.
type DockerConfig struct {
	Socket       string `mapstructure:"socket"`
	DinD         *bool  `mapstructure:"dind"` // pointer: nil = inherit default (true)
	SharedVolume string `mapstructure:"shared-volume"`
	Memory       int    `mapstructure:"memory"`     // Memory limit in MB (0 = unlimited)
	CPU          int    `mapstructure:"cpu"`        // CPU cores (0 = unlimited)
	PidsLimit    int64  `mapstructure:"pids-limit"` // Max pids (processes + threads) per container (0 = unlimited)
	Platform     string `mapstructure:"platform"`   // e.g. "linux/amd64" to force architecture

	// Network is the name of a pre-existing Docker network to attach runner
	// containers to ("" = the daemon's default bridge). Lets operators isolate
	// runners from the rest of the host network, e.g.
	// `docker network create --opt com.docker.network.bridge.enable_icc=false runners`.
	// Not validated here — the daemon fails container creation with a clear
	// error if the network does not exist.
	Network string `mapstructure:"network"`

	// SharedVolumeName is the named Docker volume backing the shared-volume
	// mount ("" = DefaultSharedVolumeName). Different scalesets — or two
	// runner processes on one host — can use isolated volumes by picking
	// distinct names.
	SharedVolumeName string `mapstructure:"shared-volume-name"`

	// CacheVolumes are named volumes mounted into every runner container at
	// tool-default cache paths, so workflows hit warm caches with zero
	// workflow changes. Entry format "volume-name:/absolute/container/path",
	// e.g. "gradle-cache:/home/runner/.gradle". Cache volumes are persistent
	// caches: they are deliberately never removed at exit and never swept by
	// any TTL. Same name across scalesets = shared cache, different = isolated.
	CacheVolumes []string `mapstructure:"cache-volumes"`

	// SharedVolumeTTL deletes files in shared-volume older than this duration.
	// 0 (default) disables TTL cleanup. Accepts Go duration strings, e.g. "168h".
	SharedVolumeTTL time.Duration `mapstructure:"shared-volume-ttl"`
	// SharedVolumeCleanupInterval is how often the TTL sweep runs while
	// runner is up. Ignored when SharedVolumeTTL is 0. Defaults to
	// DefaultSharedVolumeCleanupInterval when unset.
	SharedVolumeCleanupInterval time.Duration `mapstructure:"shared-volume-cleanup-interval"`

	// BuildxCleanup enables automatic removal of orphaned buildx BuildKit
	// builder containers and their state volumes. Pointer: nil = inherit the
	// safe default (false). Enable only on a daemon dedicated to runners.
	BuildxCleanup *bool `mapstructure:"buildx-cleanup"`
	// BuildxCleanupTTL removes buildx builders older than this duration.
	// Defaults to DefaultBuildxCleanupTTL when unset. Set generously above
	// your longest build so in-progress builds are never disrupted.
	BuildxCleanupTTL time.Duration `mapstructure:"buildx-cleanup-ttl"`
	// BuildxCleanupInterval is how often the buildx cleanup sweep runs while
	// runner is up. Defaults to DefaultBuildxCleanupInterval when unset.
	BuildxCleanupInterval time.Duration `mapstructure:"buildx-cleanup-interval"`

	// Prune enables the periodic runtime prune of the shared Docker daemon:
	// stopped containers and dangling images older than PruneTTL, plus
	// age/budget-based build cache retention. Pointer: nil = inherit the safe
	// default (false). Like buildx-cleanup, this assumes the daemon is dedicated to
	// runners: stopped containers and dangling images older than the TTL are
	// treated as garbage regardless of what created them.
	Prune *bool `mapstructure:"prune"`
	// PruneInterval is how often the runtime prune sweep runs while runner
	// is up. Defaults to DefaultDockerPruneInterval when unset.
	PruneInterval time.Duration `mapstructure:"prune-interval"`
	// PruneTTL is the age threshold of the sweep: dangling images and stopped
	// containers older than this are removed. Defaults to DefaultDockerPruneTTL
	// when unset (0); <= 0 (i.e. negative) disables the image/container
	// portion while keeping build cache retention.
	PruneTTL time.Duration `mapstructure:"prune-ttl"`
	// BuildCacheMaxAge prunes daemon build cache entries not used within this
	// window. Defaults to DefaultDockerBuildCacheMaxAge when unset (0);
	// <= 0 (i.e. negative) disables the age-based cache prune.
	BuildCacheMaxAge time.Duration `mapstructure:"build-cache-max-age"`
	// BuildCacheBudgetGB is an optional hard cap on the daemon build cache
	// (in GB) — least-recently-used entries are evicted down to the cap on
	// each sweep. 0 (default) means no cap (age-based pruning still runs).
	BuildCacheBudgetGB int `mapstructure:"build-cache-budget"`
}

// CacheVolumeMount is one parsed cache-volumes entry: a named Docker volume
// and the absolute container path it is mounted at.
type CacheVolumeMount struct {
	Volume string
	Path   string
}

// volumeNameRe matches Docker's volume-name pattern.
var volumeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ParseCacheVolumes parses the "volume-name:/absolute/container/path" entries
// in CacheVolumes. The entry is split on the first colon; an entry with an
// empty volume name or path, a non-absolute path, or a volume name outside
// Docker's volume-name pattern is an error.
func (dc DockerConfig) ParseCacheVolumes() ([]CacheVolumeMount, error) {
	if len(dc.CacheVolumes) == 0 {
		return nil, nil
	}
	mounts := make([]CacheVolumeMount, 0, len(dc.CacheVolumes))
	paths := make(map[string]bool)
	for _, entry := range dc.CacheVolumes {
		name, path, ok := strings.Cut(entry, ":")
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("cache-volumes entry %q must be \"volume-name:/absolute/container/path\"", entry)
		}
		if !volumeNameRe.MatchString(name) {
			return nil, fmt.Errorf("cache-volumes entry %q: invalid volume name %q", entry, name)
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return nil, fmt.Errorf("cache-volumes entry %q: container path %q must be absolute", entry, path)
		}
		if paths[path] {
			return nil, fmt.Errorf("cache-volumes contains duplicate container path %q", path)
		}
		paths[path] = true
		mounts = append(mounts, CacheVolumeMount{Volume: name, Path: path})
	}
	return mounts, nil
}

// TartConfig holds Tart VM-specific backend settings.
type TartConfig struct {
	Home      string `mapstructure:"home"`       // TART_HOME for tart CLI ("" = default ~/.tart)
	RunnerDir string `mapstructure:"runner-dir"` // Runner binary path in VM
	CPU       int    `mapstructure:"cpu"`        // Number of CPU cores (0 = use image default)
	Memory    int    `mapstructure:"memory"`     // Memory in MB (0 = use image default)
	PoolSize  int    `mapstructure:"pool-size"`  // Pre-warmed VM count (0 = disabled)

	// CacheCleanup enables periodic `tart prune` of the OCI/IPSW caches under
	// TART_HOME. Pointer: nil = inherit default (true). Local VMs are never
	// touched. Disable (false) only if you manage the cache yourself.
	CacheCleanup *bool `mapstructure:"cache-cleanup"`
	// CacheMaxAge removes cache entries not accessed within this window via
	// `tart prune --older-than`. Defaults to DefaultTartCacheMaxAge when unset.
	// Granularity is whole days; sub-day values floor to 1 day.
	CacheMaxAge time.Duration `mapstructure:"cache-max-age"`
	// CacheSpaceBudgetGB is an optional hard cap on the OCI/IPSW cache size
	// (in GB), enforced via `tart prune --space-budget` on top of the age-based
	// sweep — least-recently-used entries are removed until the total fits.
	// 0 (default) means no size cap (age-based cleanup still runs).
	CacheSpaceBudgetGB int `mapstructure:"cache-space-budget"`
	// CacheCleanupInterval is how often the prune sweep runs while runner
	// is up. Ignored when cache cleanup is disabled. Defaults to
	// DefaultTartCacheCleanupInterval when unset.
	CacheCleanupInterval time.Duration `mapstructure:"cache-cleanup-interval"`
}

// ResolveScaleSets returns the resolved list of scale set configs.
// [[scaleset]] entries arrive from Load with inheritance already applied at
// the map level (a key present in an entry wins, even when zero), so multi
// mode only applies backend-dependent defaults and token resolution here.
// Without [[scaleset]] entries, Defaults itself is the single scale set.
func (c *Config) ResolveScaleSets() []ScaleSetConfig {
	if len(c.ScaleSets) > 0 {
		for i := range c.ScaleSets {
			c.ScaleSets[i].applyDefaults()
			c.ScaleSets[i].resolveEnvToken()
		}
		return c.ScaleSets
	}

	// Single scale set mode: use Defaults directly.
	ss := c.Defaults
	ss.applyDefaults()
	ss.resolveEnvToken()
	return []ScaleSetConfig{ss}
}

// applyDefaults fills in backend-specific defaults that depend on
// the backend selection (e.g. TartRunnerDir when backend is "tart").
func (ss *ScaleSetConfig) applyDefaults() {
	if ss.Backend == "" {
		ss.Backend = DefaultBackend
	}
	if ss.Backend == DefaultBackend && ss.Docker.Socket == "" {
		ss.Docker.Socket = DefaultDockerSocket
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

// SharedVolumeName returns the named Docker volume backing the shared-volume
// mount, falling back to DefaultSharedVolumeName when unset.
func (ss *ScaleSetConfig) SharedVolumeName() string {
	if ss.Docker.SharedVolumeName != "" {
		return ss.Docker.SharedVolumeName
	}
	return DefaultSharedVolumeName
}

// IsTart returns whether this scale set uses the Tart VM backend.
func (ss *ScaleSetConfig) IsTart() bool {
	return ss.Backend == "tart"
}

// IsBuildxCleanupEnabled reports whether orphaned buildx builder cleanup is
// enabled (default false unless explicitly enabled).
func (ss *ScaleSetConfig) IsBuildxCleanupEnabled() bool {
	if ss.Docker.BuildxCleanup != nil {
		return *ss.Docker.BuildxCleanup
	}
	return DefaultBuildxCleanup
}

// IsUpdateDisabled reports whether GitHub is barred from updating the runner
// binary inside the container or VM (default true unless explicitly enabled).
func (ss *ScaleSetConfig) IsUpdateDisabled() bool {
	if ss.DisableUpdate != nil {
		return *ss.DisableUpdate
	}
	return DefaultDisableUpdate
}

// IsDockerPruneEnabled reports whether the periodic Docker runtime prune is
// enabled (default false unless explicitly enabled).
func (ss *ScaleSetConfig) IsDockerPruneEnabled() bool {
	if ss.Docker.Prune != nil {
		return *ss.Docker.Prune
	}
	return DefaultDockerPrune
}

// IsTartCacheCleanupEnabled reports whether Tart OCI/IPSW cache cleanup is
// enabled (default true unless explicitly disabled).
func (ss *ScaleSetConfig) IsTartCacheCleanupEnabled() bool {
	if ss.Tart.CacheCleanup != nil {
		return *ss.Tart.CacheCleanup
	}
	return DefaultTartCacheCleanup
}

// resolveEnvToken resolves the token value from environment variables.
// Supports two patterns:
//   - token = "env:VARIABLE_NAME" — reads from the named env var
//   - Empty token with RUNNER_TOKEN env var set — uses that as fallback
//     (the legacy RUNSCALER_TOKEN is still honored during the transition)
func (ss *ScaleSetConfig) resolveEnvToken() {
	if strings.HasPrefix(ss.Token, "env:") {
		envName := strings.TrimPrefix(ss.Token, "env:")
		ss.Token = os.Getenv(envName)
		return
	}
	if ss.Token == "" {
		if v := os.Getenv("RUNNER_TOKEN"); v != "" {
			ss.Token = v
		} else if v := os.Getenv("RUNSCALER_TOKEN"); v != "" {
			// Deprecated: kept so existing deployments keep working during the
			// runscaler→runner transition. Remove in a future release.
			ss.Token = v
		}
	}
}

// Validate checks required fields and logical constraints for a scale set.
func (ss *ScaleSetConfig) Validate() error {
	if ss.RegistrationURL == "" {
		return fmt.Errorf("registration URL (url) is required")
	}
	parsedURL, err := url.ParseRequestURI(ss.RegistrationURL)
	if err != nil {
		return fmt.Errorf("invalid registration URL: %w", err)
	}
	if parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil {
		return fmt.Errorf("registration URL must be an HTTPS URL without embedded credentials")
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
	if ss.RunnerImage == "" {
		return fmt.Errorf("runner-image is required")
	}

	switch ss.Backend {
	case DefaultBackend:
		if ss.Docker.Platform != "" {
			parts := strings.Split(ss.Docker.Platform, "/")
			if (len(parts) != 2 && len(parts) != 3) || parts[0] == "" || parts[1] == "" || (len(parts) == 3 && parts[2] == "") {
				return fmt.Errorf("docker platform must be os/arch or os/arch/variant")
			}
		}
		mounts, err := ss.Docker.ParseCacheVolumes()
		if err != nil {
			return err
		}
		if ss.Docker.Memory < 0 || ss.Docker.CPU < 0 || ss.Docker.PidsLimit < 0 || ss.Docker.BuildCacheBudgetGB < 0 {
			return fmt.Errorf("docker resource limits and cache budget must be >= 0")
		}
		if ss.Docker.SharedVolumeName != "" && !volumeNameRe.MatchString(ss.Docker.SharedVolumeName) {
			return fmt.Errorf("invalid shared-volume-name %q", ss.Docker.SharedVolumeName)
		}
		if ss.Docker.SharedVolume != "" {
			if !filepath.IsAbs(ss.Docker.SharedVolume) || filepath.Clean(ss.Docker.SharedVolume) != ss.Docker.SharedVolume || ss.Docker.SharedVolume == "/" {
				return fmt.Errorf("shared-volume must be a clean absolute container path other than /")
			}
			for _, mount := range mounts {
				if mount.Path == ss.Docker.SharedVolume {
					return fmt.Errorf("shared-volume conflicts with cache-volumes path %q", mount.Path)
				}
			}
		}
	case "tart":
		if ss.MaxRunners > 2 {
			return fmt.Errorf("max-runners must be <= 2 for the Tart backend (Apple host limit)")
		}
		if ss.Tart.PoolSize < 0 || ss.Tart.PoolSize > ss.MaxRunners {
			return fmt.Errorf("tart pool-size must be between 0 and max-runners")
		}
	default:
		return fmt.Errorf("unsupported backend %q (must be %q or \"tart\")", ss.Backend, DefaultBackend)
	}

	return nil
}

// ValidateGlobal checks process-wide settings that are not part of any scale
// set and therefore would otherwise bypass ScaleSetConfig.Validate.
func (c *Config) ValidateGlobal() error {
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log-level must be debug, info, warn, or error")
	}
	switch strings.ToLower(c.LogFormat) {
	case "text", "json":
	default:
		return fmt.Errorf("log-format must be text or json")
	}
	if c.HealthPort < 0 || c.HealthPort > 65535 {
		return fmt.Errorf("health-port must be between 0 and 65535")
	}
	if err := c.Disk.Validate(); err != nil {
		return fmt.Errorf("disk: %w", err)
	}
	return nil
}

// DiskConfig configures the periodic disk-pressure guard (internal/diskguard):
// the free-space thresholds that trigger and bound reclamation, how often it
// sweeps, and the highest cachestore.Tier it may reach. See package
// diskguard's Config and Guard for what these drive at runtime.
type DiskConfig struct {
	// Guard enables the periodic disk guard. Pointer: nil = inherit default
	// (true) — see DefaultDiskGuard's doc comment for why leaving it on is
	// safe even though it deletes data.
	Guard *bool `mapstructure:"guard"`
	// MinFree is the free-space threshold that triggers reclamation: a
	// percentage like "10%" (of the filesystem's total capacity) or an
	// absolute size like "20GB". Defaults to DefaultDiskMinFree when unset.
	MinFree string `mapstructure:"min-free"`
	// TargetFree is the free-space level reclamation aims to restore before
	// stopping; must be strictly greater than MinFree (see Validate).
	// Defaults to DefaultDiskTargetFree when unset.
	TargetFree string `mapstructure:"target-free"`
	// Interval is the period between guard sweeps. <= 0 (unset) resolves to
	// DefaultDiskGuardInterval at the call site, matching every other sweep
	// interval in this package (e.g. PruneInterval).
	Interval time.Duration `mapstructure:"interval"`
	// MaxTier is the highest cachestore.Tier the guard may reclaim at while
	// chasing TargetFree; must be between 1 and 4 (see Validate). 0 (unset)
	// resolves to DefaultDiskMaxTier.
	MaxTier int `mapstructure:"max-tier"`
}

// IsDiskGuardEnabled reports whether the periodic disk guard is enabled
// (default true unless explicitly disabled).
func (c *Config) IsDiskGuardEnabled() bool {
	if c.Disk.Guard != nil {
		return *c.Disk.Guard
	}
	return DefaultDiskGuard
}

// Validate checks that the disk guard's thresholds are parseable and that
// MinFree is strictly less than TargetFree, and that MaxTier is a real tier
// (1-4). Both matter more than they look: MinFree >= TargetFree makes every
// sweep both trigger immediately and never satisfy its own target, and
// MaxTier outside 1-4 (in particular the zero value) silently disables the
// tier ladder — see diskguard.Guard.sweepFilesystem's target/free
// subtraction, which assumes the ladder ran at least one tier.
//
// A zero-value DiskConfig (the operator never touched [disk]) must pass:
// defaults are substituted before parsing, not compared as raw zero values.
func (dc DiskConfig) Validate() error {
	minFree := dc.MinFree
	if minFree == "" {
		minFree = DefaultDiskMinFree
	}
	targetFree := dc.TargetFree
	if targetFree == "" {
		targetFree = DefaultDiskTargetFree
	}

	min, err := parseDiskThreshold(minFree)
	if err != nil {
		return fmt.Errorf("min-free: %w", err)
	}
	target, err := parseDiskThreshold(targetFree)
	if err != nil {
		return fmt.Errorf("target-free: %w", err)
	}
	less, comparable := min.lessThan(target)
	if !comparable {
		return fmt.Errorf("min-free and target-free must both be percentages or both be sizes to compare")
	}
	if !less {
		return fmt.Errorf("min-free must be less than target-free")
	}

	maxTier := dc.MaxTier
	if maxTier == 0 {
		maxTier = DefaultDiskMaxTier
	}
	if maxTier < 1 || maxTier > 4 {
		return fmt.Errorf("max-tier must be between 1 and 4")
	}

	return nil
}

// diskThreshold is a parsed [disk] free-space threshold: either a
// percentage (0-100) of total filesystem capacity or an absolute byte
// count — the same two shapes diskguard.Threshold models.
//
// This is a deliberate duplication of diskguard.ParseThreshold's parsing,
// not a shared type: package diskguard transitively imports this package
// (diskguard -> cachestore -> backend -> config, all confirmed via `go list
// -deps`), so importing diskguard from here would be a compile-time import
// cycle. cachestore/volume.go's shellQuote sets the same precedent — a
// small helper duplicated across this exact kind of package boundary rather
// than exported for one caller.
type diskThreshold struct {
	percent   float64
	bytes     uint64
	isPercent bool
}

// lessThan reports whether dt < other, and whether the two are even
// comparable (both percentages or both absolute sizes — a percentage of one
// filesystem's capacity and an absolute byte count are not comparable
// without knowing that filesystem's size, which Validate does not have).
func (dt diskThreshold) lessThan(other diskThreshold) (less, comparable bool) {
	if dt.isPercent != other.isPercent {
		return false, false
	}
	if dt.isPercent {
		return dt.percent < other.percent, true
	}
	return dt.bytes < other.bytes, true
}

// diskByteUnits mirrors diskguard.Threshold's byte-unit table exactly
// (longest-suffix-first so "GB" is tried before the bare "B" suffix it
// would otherwise also match).
var diskByteUnits = []struct {
	suffix     string
	multiplier uint64
}{
	{"TB", 1024 * 1024 * 1024 * 1024},
	{"GB", 1024 * 1024 * 1024},
	{"MB", 1024 * 1024},
	{"KB", 1024},
	{"B", 1},
}

// parseDiskThreshold parses s the same way diskguard.ParseThreshold does: a
// percentage like "10%" (0-100 inclusive) or an absolute size like "20GB"
// (binary units, case-insensitive: B/KB/MB/GB/TB).
func parseDiskThreshold(s string) (diskThreshold, error) {
	if rest, ok := strings.CutSuffix(s, "%"); ok {
		pct, err := strconv.ParseFloat(rest, 64)
		// Negation of the in-range condition, not `pct < 0 || pct > 100`:
		// every ordered comparison with NaN is false, so the direct form
		// would let ParseFloat's accepted "NaN"/"Inf" spellings silently
		// pass through as a threshold instead of failing validation —
		// mirrors diskguard.ParseThreshold's identical guard exactly.
		if err != nil || !(pct >= 0 && pct <= 100) {
			return diskThreshold{}, invalidDiskThresholdError(s)
		}
		return diskThreshold{percent: pct, isPercent: true}, nil
	}

	upper := strings.ToUpper(s)
	for _, u := range diskByteUnits {
		numPart, ok := strings.CutSuffix(upper, u.suffix)
		if !ok || numPart == "" {
			continue
		}
		n, err := strconv.ParseUint(numPart, 10, 64)
		if err != nil {
			continue
		}
		return diskThreshold{bytes: n * u.multiplier}, nil
	}

	return diskThreshold{}, invalidDiskThresholdError(s)
}

// invalidDiskThresholdError reports the legal formats so a misconfigured
// value is actionable without reading source.
func invalidDiskThresholdError(s string) error {
	return fmt.Errorf("invalid threshold %q: want a percentage like \"10%%\" or a size like \"20GB\"", s)
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
	return NewLoggerWithWriter(level, format, nil)
}

// NewLoggerWithWriter creates a process logger that tees to file when one is
// supplied. The writer may be shared safely with scale-set loggers.
func NewLoggerWithWriter(level, format string, file io.Writer) *slog.Logger {
	opts := charmlog.Options{
		ReportTimestamp: true,
		TimeFormat:      time.DateTime,
		Level:           parseLogLevel(level),
	}

	if strings.ToLower(format) == "json" {
		opts.Formatter = charmlog.JSONFormatter
	}

	handler := charmlog.NewWithOptions(outputWriter(file), opts)
	logger := slog.New(&demoteHandler{inner: handler, demote: demoteMessages})
	slog.SetDefault(logger)
	return logger
}

// scaleSetColors is a 5-color palette for distinguishing scale sets in logs.
// Red is excluded to avoid confusion with error output (same rationale as stern).
// Black/white excluded for readability on dark/light terminal backgrounds.
var scaleSetColors = []color.Color{
	lipgloss.Color("6"), // cyan
	lipgloss.Color("3"), // yellow
	lipgloss.Color("2"), // green
	lipgloss.Color("5"), // magenta
	lipgloss.Color("4"), // blue
}

// NewScaleSetLogger creates a logger with a colored prefix for the given scale set.
// The color is determined by the index, cycling through the palette.
func NewScaleSetLogger(level, format string, name string, index int) *slog.Logger {
	return NewScaleSetLoggerWithWriter(level, format, name, index, nil)
}

func NewScaleSetLoggerWithWriter(level, format string, name string, index int, file io.Writer) *slog.Logger {
	opts := charmlog.Options{
		ReportTimestamp: true,
		TimeFormat:      time.DateTime,
		Level:           parseLogLevel(level),
		Prefix:          name,
	}

	if strings.ToLower(format) == "json" {
		opts.Formatter = charmlog.JSONFormatter
	}

	handler := charmlog.NewWithOptions(outputWriter(file), opts)

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
