package config

import "time"

// Default values for configuration fields.
// All flag definitions, config templates, and fallback logic
// should reference these constants instead of hardcoding values.
const (
	DefaultBackend       = "docker"
	DefaultMaxRunners    = 10
	DefaultRunnerImage   = "ghcr.io/actions/actions-runner:latest"
	DefaultRunnerGroup   = "default"
	DefaultDockerSocket  = "/var/run/docker.sock"
	DefaultDinD          = true
	DefaultTartRunnerDir = "/Users/admin/actions-runner"
	DefaultLogLevel      = "info"
	DefaultLogFormat     = "text"
	DefaultHealthPort    = 8080
	DefaultSystemName    = "dockerscaleset"

	// DefaultSharedVolumeCleanupInterval is the period between shared-volume
	// TTL sweeps when SharedVolumeTTL > 0 and no explicit interval is set.
	DefaultSharedVolumeCleanupInterval = 6 * time.Hour

	// DefaultTartCacheCleanup enables Tart OCI/IPSW cache cleanup by default.
	// It acts as a safety net: every `tart pull` accumulates layers under
	// $TART_HOME/cache/, and rolling tags (e.g. :latest) combined with 50–80GB
	// macOS images can fill the disk within a few updates. The sweep only
	// touches caches, never local VMs, so it is safe to leave on.
	DefaultTartCacheCleanup = true

	// DefaultTartCacheMaxAge removes cache entries not accessed within this
	// window. Old layers left behind by an image update stop being accessed and
	// age out naturally. Generous so an image still in active use is never
	// evicted. `tart prune` granularity is whole days; sub-day values floor
	// to 1 day.
	DefaultTartCacheMaxAge = 7 * 24 * time.Hour

	// DefaultTartCacheCleanupInterval is the period between `tart prune` sweeps
	// when cache cleanup is enabled and no explicit interval is set.
	DefaultTartCacheCleanupInterval = 24 * time.Hour

	// DefaultBuildxCleanup enables orphaned buildx builder cleanup by default.
	// It acts as a safety net: `docker buildx create` builders (e.g. from
	// docker/setup-buildx-action) leak on persistent hosts sharing one daemon,
	// each keeping a multi-GB state volume, and most users never notice.
	DefaultBuildxCleanup = true

	// DefaultBuildxCleanupTTL removes buildx builders older than this. It is
	// deliberately generous — well beyond any realistic build — so a sweep
	// never disrupts an in-progress build.
	DefaultBuildxCleanupTTL = 24 * time.Hour

	// DefaultBuildxCleanupInterval is the period between buildx cleanup sweeps.
	DefaultBuildxCleanupInterval = 6 * time.Hour
)
