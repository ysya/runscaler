package config

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/ysya/runscaler/internal/bytesize"
)

// Effective retention settings. Every default for a sweep interval, TTL,
// max-age or disk threshold is applied here and nowhere else, so the
// periodic sweepers, the disk guard and `runner cache` cannot disagree about
// what an unset value means.

// positiveOr returns d, or def when d is unset or not positive.
func positiveOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// unsetOr returns d, or def when d is unset (0). A negative d is kept: for
// the settings that use this, it switches one portion of a sweep off.
func unsetOr(d, def time.Duration) time.Duration {
	if d != 0 {
		return d
	}
	return def
}

// DockerPruneTTL is the age past which the prune sweep removes stopped
// containers and dangling images. A negative value is kept: it disables that
// portion while build-cache retention still runs.
func (ss *ScaleSetConfig) DockerPruneTTL() time.Duration {
	return unsetOr(ss.Docker.PruneTTL, DefaultDockerPruneTTL)
}

// DockerPruneInterval is how often the prune sweep runs.
func (ss *ScaleSetConfig) DockerPruneInterval() time.Duration {
	return positiveOr(ss.Docker.PruneInterval, DefaultDockerPruneInterval)
}

// DockerBuildCacheMaxAge is the idle age past which build cache is pruned. A
// negative value is kept: it disables the age-based prune while a
// build-cache budget still applies.
func (ss *ScaleSetConfig) DockerBuildCacheMaxAge() time.Duration {
	return unsetOr(ss.Docker.BuildCacheMaxAge, DefaultDockerBuildCacheMaxAge)
}

// DockerBuildxCleanupTTL is the age past which an orphaned buildx builder is
// removed. Unlike the prune TTL, a negative value has no meaning of its own.
func (ss *ScaleSetConfig) DockerBuildxCleanupTTL() time.Duration {
	return positiveOr(ss.Docker.BuildxCleanupTTL, DefaultBuildxCleanupTTL)
}

// DockerBuildxCleanupInterval is how often the buildx cleanup sweep runs.
func (ss *ScaleSetConfig) DockerBuildxCleanupInterval() time.Duration {
	return positiveOr(ss.Docker.BuildxCleanupInterval, DefaultBuildxCleanupInterval)
}

// DockerSharedVolumeCleanupInterval is how often the shared-volume TTL sweep
// runs. The max-age itself has no default: unset means no retention policy.
func (ss *ScaleSetConfig) DockerSharedVolumeCleanupInterval() time.Duration {
	return positiveOr(ss.Docker.SharedVolumeCleanupInterval, DefaultSharedVolumeCleanupInterval)
}

// TartCacheMaxAge is the idle age past which `tart prune` evicts a cached
// image layer.
func (ss *ScaleSetConfig) TartCacheMaxAge() time.Duration {
	return positiveOr(ss.Tart.CacheMaxAge, DefaultTartCacheMaxAge)
}

// TartCacheCleanupInterval is how often the Tart cache prune runs.
func (ss *ScaleSetConfig) TartCacheCleanupInterval() time.Duration {
	return positiveOr(ss.Tart.CacheCleanupInterval, DefaultTartCacheCleanupInterval)
}

// DiskGuardSettings are the [disk] settings with every default applied and
// both thresholds parsed: exactly what the disk guard runs with.
type DiskGuardSettings struct {
	// MinFree and TargetFree are the thresholds as configured, or their
	// defaults, kept as text for logs.
	MinFree, TargetFree string
	MinFreeThreshold    bytesize.Threshold
	TargetFreeThreshold bytesize.Threshold
	Interval            time.Duration
	MaxTier             int
}

// Resolve applies the [disk] defaults, parses both thresholds and checks that
// they make sense together: MinFree strictly below TargetFree, and MaxTier a
// real tier (1-4). Both matter more than they look: MinFree >= TargetFree
// makes every sweep both trigger immediately and never satisfy its own
// target, and MaxTier outside 1-4 (in particular the zero value) silently
// disables the tier ladder — see diskguard.Guard.sweepFilesystem's
// target/free subtraction, which assumes the ladder ran at least one tier.
//
// A zero-value DiskConfig (the operator never touched [disk]) resolves to the
// defaults: they are substituted before parsing, never compared as raw zero
// values.
func (dc DiskConfig) Resolve() (DiskGuardSettings, error) {
	s := DiskGuardSettings{
		MinFree:    cmp.Or(dc.MinFree, DefaultDiskMinFree),
		TargetFree: cmp.Or(dc.TargetFree, DefaultDiskTargetFree),
		Interval:   positiveOr(dc.Interval, DefaultDiskGuardInterval),
		MaxTier:    cmp.Or(dc.MaxTier, DefaultDiskMaxTier),
	}
	var err error
	if s.MinFreeThreshold, err = bytesize.ParseThreshold(s.MinFree); err != nil {
		return DiskGuardSettings{}, fmt.Errorf("min-free: %w", err)
	}
	if s.TargetFreeThreshold, err = bytesize.ParseThreshold(s.TargetFree); err != nil {
		return DiskGuardSettings{}, fmt.Errorf("target-free: %w", err)
	}

	// bytesize.Threshold does not record whether it came from a percentage
	// or an absolute size, and a literal "0%" would look like an absolute
	// zero-byte threshold. Reading the "%" suffix off the strings is exact.
	minIsPercent := strings.HasSuffix(s.MinFree, "%")
	targetIsPercent := strings.HasSuffix(s.TargetFree, "%")
	if minIsPercent != targetIsPercent {
		return DiskGuardSettings{}, fmt.Errorf("min-free and target-free must both be percentages or both be sizes to compare")
	}
	if minIsPercent {
		if s.MinFreeThreshold.Percent >= s.TargetFreeThreshold.Percent {
			return DiskGuardSettings{}, fmt.Errorf("min-free must be less than target-free")
		}
	} else if s.MinFreeThreshold.Bytes >= s.TargetFreeThreshold.Bytes {
		return DiskGuardSettings{}, fmt.Errorf("min-free must be less than target-free")
	}

	if s.MaxTier < 1 || s.MaxTier > 4 {
		return DiskGuardSettings{}, fmt.Errorf("max-tier must be between 1 and 4")
	}
	return s, nil
}
