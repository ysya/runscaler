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
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	charmlog "charm.land/log/v2"
	"github.com/actions/scaleset"
	"github.com/hashicorp/go-retryablehttp"

	"github.com/ysya/runscaler/internal/bytesize"
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
	//
	// This is the short form. See Cache for the long form, which adds an
	// optional retention budget; ParseCacheVolumes merges the two.
	CacheVolumes []string `mapstructure:"cache-volumes"`

	// Cache is the long form of CacheVolumes — [[docker.cache]] tables
	// instead of "name:/path" strings — for entries that need a retention
	// budget. An entry here whose Name also appears in CacheVolumes replaces
	// it entirely (see ParseCacheVolumes); the two are never merged field by
	// field.
	Cache []CacheVolumeSpec `mapstructure:"cache"`

	// SharedVolumeMaxAge deletes files in shared-volume older than this
	// duration. Accepts Go duration strings, e.g. "168h". It must exceed
	// the longest workflow run on this host: the volume holds handoff data
	// a later job in the same run still reads.
	//
	// 0 (default) is not "cleanup off, everything else unchanged" — it is
	// the only retention policy this volume has. runner stopped removing
	// the volume at exit (that broke runs in flight across a restart), and
	// the disk guard cannot reclaim un-expired scratch data at any tier, so
	// with no max-age nothing ever frees it. The volume is still measured
	// and listed by `runner cache`, and startup warns about the missing
	// setting.
	SharedVolumeMaxAge time.Duration `mapstructure:"shared-volume-max-age"`
	// SharedVolumeTTL is this field's pre-2026-08-14 name.
	//
	// Deprecated: superseded by SharedVolumeMaxAge; config loading never
	// populates this field. Load's applyAliases step (load.go) always moves
	// a configured shared-volume-ttl value onto shared-volume-max-age and
	// deletes the old key from the settings map before any decode runs, so
	// a live mapstructure tag here would never fire in the working case —
	// and if applyAliases ever regressed, a live tag would silently swallow
	// a legacy config's value into this field (which nothing reads) instead
	// of surfacing the regression as the loud "unknown config key" warning
	// it should. mapstructure:"-" makes that the only possible outcome.
	// Kept only so Go code that still constructs
	// DockerConfig{SharedVolumeTTL: …} directly (not through config
	// loading) keeps compiling; read SharedVolumeMaxAge instead.
	SharedVolumeTTL time.Duration `mapstructure:"-"`
	// SharedVolumeCleanupInterval is how often the cleanup sweep runs while
	// runner is up. Ignored when SharedVolumeMaxAge is 0. Defaults to
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

// CacheVolumeSpec is one [[docker.cache]] long-form cache-volume entry: like
// a CacheVolumes short-form "name:/path" string, plus an optional retention
// budget. See ParseCacheVolumes for how it merges with the short form and
// what each field validates to.
type CacheVolumeSpec struct {
	Name     string `mapstructure:"name"`
	Path     string `mapstructure:"path"`
	Budget   string `mapstructure:"budget"`
	OnExceed string `mapstructure:"on-exceed"`
}

// CacheVolumeMount is one resolved cache-volumes entry — merged from the
// short and long forms by ParseCacheVolumes — naming a Docker volume, the
// absolute container path it mounts at, and its optional retention budget.
// BudgetBytes 0 means no budget is configured (every short-form entry, and
// a long-form entry whose Budget is ""); OnExceed only matters when
// BudgetBytes != 0.
type CacheVolumeMount struct {
	Volume      string
	Path        string
	BudgetBytes uint64
	OnExceed    string
}

// volumeNameRe matches Docker's volume-name pattern.
var volumeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// validateVolumeName reports whether name matches Docker's volume-name
// pattern.
func validateVolumeName(name string) error {
	if !volumeNameRe.MatchString(name) {
		return fmt.Errorf("invalid volume name %q", name)
	}
	return nil
}

// validateContainerPath reports whether path is a clean absolute path other
// than "/" — the shape every cache-volume mount target must have.
func validateContainerPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return fmt.Errorf("container path %q must be absolute", path)
	}
	return nil
}

// parseCacheVolumeBudget parses a [[docker.cache]] entry's Budget string
// into bytes. "" means unconfigured (0, no error) — the short form's only
// option. A percentage (e.g. "10%") is rejected: unlike [disk]'s
// thresholds — which internal/diskguard resolves against a fresh statfs on
// every sweep — there is no live filesystem to resolve a percentage against
// at config-parse time, so letting bytesize.ParseThreshold's percentage
// branch through would silently produce a permanently-zero budget (0% of an
// unknown total).
func parseCacheVolumeBudget(budget string) (uint64, error) {
	if budget == "" {
		return 0, nil
	}
	if strings.HasSuffix(budget, "%") {
		return 0, fmt.Errorf("budget %q must be an absolute size like \"20GB\" (percentages are not supported for cache volumes)", budget)
	}
	th, err := bytesize.ParseThreshold(budget)
	if err != nil {
		return 0, fmt.Errorf("invalid budget %q: want a size like \"20GB\"", budget)
	}
	return th.Bytes, nil
}

// ParseCacheVolumes parses and merges CacheVolumes (short form) and Cache
// (long form) into one list of mounts, ordered by first appearance across
// both. An entry present in both forms is resolved entirely from its
// long-form entry — path, budget, and on-exceed all come from there, and
// the short-form entry of the same name is discarded rather than partially
// merged — matching docker-compose's own short/long merge convention (see
// section E of docs/superpowers/specs/2026-08-13-cache-architecture-design.md).
//
// Errors: an empty volume name or path, a non-absolute/non-clean container
// path, a volume name outside Docker's volume-name pattern, the same volume
// name repeated within one form (CacheVolumes or Cache — but not across the
// two; see above), two entries (from either form) resolving to the same
// container path, a long-form Budget that fails to parse or names a
// percentage (see parseCacheVolumeBudget), or a long-form OnExceed other
// than "", "warn", "wipe".
func (dc DockerConfig) ParseCacheVolumes() ([]CacheVolumeMount, error) {
	order := make([]string, 0, len(dc.CacheVolumes)+len(dc.Cache))
	byName := make(map[string]CacheVolumeMount, len(dc.CacheVolumes)+len(dc.Cache))

	seenShort := make(map[string]bool, len(dc.CacheVolumes))
	for _, entry := range dc.CacheVolumes {
		name, path, ok := strings.Cut(entry, ":")
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("cache-volumes entry %q must be \"volume-name:/absolute/container/path\"", entry)
		}
		if err := validateVolumeName(name); err != nil {
			return nil, fmt.Errorf("cache-volumes entry %q: %w", entry, err)
		}
		if err := validateContainerPath(path); err != nil {
			return nil, fmt.Errorf("cache-volumes entry %q: %w", entry, err)
		}
		// A repeated name within this same list is rejected rather than
		// silently resolving to whichever entry is seen last: two different
		// paths under one volume name almost always means a copy-pasted
		// entry whose name the operator forgot to change, not a deliberate
		// request to mount one named volume at two paths. Cross-form
		// repeats (a long-form entry naming a short-form one) are the
		// opposite — a deliberate override — and stay unrestricted below.
		if seenShort[name] {
			return nil, fmt.Errorf("cache-volumes contains duplicate volume name %q", name)
		}
		seenShort[name] = true
		order = append(order, name)
		byName[name] = CacheVolumeMount{Volume: name, Path: path}
	}

	seenLong := make(map[string]bool, len(dc.Cache))
	for _, spec := range dc.Cache {
		if spec.Name == "" {
			return nil, fmt.Errorf("[[docker.cache]] entry has an empty name")
		}
		if err := validateVolumeName(spec.Name); err != nil {
			return nil, fmt.Errorf("[[docker.cache]] entry %q: %w", spec.Name, err)
		}
		if err := validateContainerPath(spec.Path); err != nil {
			return nil, fmt.Errorf("[[docker.cache]] entry %q: %w", spec.Name, err)
		}
		budgetBytes, err := parseCacheVolumeBudget(spec.Budget)
		if err != nil {
			return nil, fmt.Errorf("[[docker.cache]] entry %q: %w", spec.Name, err)
		}
		switch spec.OnExceed {
		case "", "warn", "wipe":
		default:
			return nil, fmt.Errorf("[[docker.cache]] entry %q: on-exceed must be \"warn\" or \"wipe\", got %q", spec.Name, spec.OnExceed)
		}
		// Same rationale as seenShort above, scoped to this form only.
		if seenLong[spec.Name] {
			return nil, fmt.Errorf("[[docker.cache]] contains duplicate volume name %q", spec.Name)
		}
		seenLong[spec.Name] = true

		if _, exists := byName[spec.Name]; !exists {
			order = append(order, spec.Name)
		}
		byName[spec.Name] = CacheVolumeMount{
			Volume: spec.Name, Path: spec.Path, BudgetBytes: budgetBytes, OnExceed: spec.OnExceed,
		}
	}

	if len(order) == 0 {
		return nil, nil
	}

	mounts := make([]CacheVolumeMount, 0, len(order))
	pathOwner := make(map[string]string, len(order))
	for _, name := range order {
		m := byName[name]
		if owner, dup := pathOwner[m.Path]; dup {
			return nil, fmt.Errorf("cache volumes %q and %q both mount container path %q", owner, name, m.Path)
		}
		pathOwner[m.Path] = name
		mounts = append(mounts, m)
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
	// CacheBudgetGB is an optional hard cap on the OCI/IPSW cache size (in
	// GB), enforced via `tart prune --space-budget` on top of the age-based
	// sweep — least-recently-used entries are removed until the total fits.
	// 0 (default) means no size cap (age-based cleanup still runs).
	CacheBudgetGB int `mapstructure:"cache-budget"`
	// CacheSpaceBudgetGB is this field's pre-2026-08-14 name.
	//
	// Deprecated: superseded by CacheBudgetGB, for the same reason and in
	// the same way DockerConfig.SharedVolumeTTL is deprecated in favor of
	// SharedVolumeMaxAge — see that field's doc comment. mapstructure:"-"
	// because Load's applyAliases step (load.go) always moves a configured
	// cache-space-budget value onto CacheBudgetGB before decoding.
	CacheSpaceBudgetGB int `mapstructure:"-"`
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

	min, err := bytesize.ParseThreshold(minFree)
	if err != nil {
		return fmt.Errorf("min-free: %w", err)
	}
	target, err := bytesize.ParseThreshold(targetFree)
	if err != nil {
		return fmt.Errorf("target-free: %w", err)
	}

	// bytesize.Threshold doesn't itself record whether it came from a
	// percentage or an absolute size (ParseThreshold sets exactly one of
	// Percent/Bytes — see its doc comment), and BytesOf's own "Bytes != 0
	// means absolute" heuristic isn't precise enough here: it would treat a
	// literal "0%" as indistinguishable from an absolute zero-byte
	// threshold, a distinction BytesOf's caller (the guard, which always
	// has a real totalBytes to scale a percentage against) never needs to
	// make but this comparison does. Reading the "%" suffix off the
	// original strings instead is exact.
	minIsPercent := strings.HasSuffix(minFree, "%")
	targetIsPercent := strings.HasSuffix(targetFree, "%")
	if minIsPercent != targetIsPercent {
		return fmt.Errorf("min-free and target-free must both be percentages or both be sizes to compare")
	}
	if minIsPercent {
		if min.Percent >= target.Percent {
			return fmt.Errorf("min-free must be less than target-free")
		}
	} else if min.Bytes >= target.Bytes {
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
