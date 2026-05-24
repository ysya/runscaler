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

	// DefaultTartCacheCleanupInterval is the period between `tart prune` sweeps
	// when CacheSpaceBudgetGB > 0 and no explicit interval is set.
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
