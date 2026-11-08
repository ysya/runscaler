package config

import (
	"testing"
	"time"
)

func TestRetentionAccessorsApplyEachDefault(t *testing.T) {
	var unset ScaleSetConfig
	for name, tc := range map[string]struct{ got, want time.Duration }{
		"docker prune ttl":              {unset.DockerPruneTTL(), DefaultDockerPruneTTL},
		"docker prune interval":         {unset.DockerPruneInterval(), DefaultDockerPruneInterval},
		"docker build-cache max-age":    {unset.DockerBuildCacheMaxAge(), DefaultDockerBuildCacheMaxAge},
		"docker buildx ttl":             {unset.DockerBuildxCleanupTTL(), DefaultBuildxCleanupTTL},
		"docker buildx interval":        {unset.DockerBuildxCleanupInterval(), DefaultBuildxCleanupInterval},
		"docker shared-volume interval": {unset.DockerSharedVolumeCleanupInterval(), DefaultSharedVolumeCleanupInterval},
		"tart cache max-age":            {unset.TartCacheMaxAge(), DefaultTartCacheMaxAge},
		"tart cache interval":           {unset.TartCacheCleanupInterval(), DefaultTartCacheCleanupInterval},
		"docker ready timeout":          {unset.DockerReadyTimeout(), DefaultReadyTimeout},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want the default %v", name, tc.got, tc.want)
		}
	}
}

// A negative prune TTL or build-cache max-age switches that portion of the
// sweep off, so it must survive defaulting; every other negative value is
// meaningless and falls back to the default like an unset one.
func TestRetentionAccessorsKeepOnlyMeaningfulNegatives(t *testing.T) {
	ss := ScaleSetConfig{
		Docker: DockerConfig{PruneTTL: -time.Hour, BuildCacheMaxAge: -time.Hour, PruneInterval: -time.Hour, BuildxCleanupTTL: -time.Hour, BuildxCleanupInterval: -time.Hour, SharedVolumeCleanupInterval: -time.Hour},
		Tart:   TartConfig{CacheMaxAge: -time.Hour, CacheCleanupInterval: -time.Hour},
	}
	for name, tc := range map[string]struct{ got, want time.Duration }{
		"docker prune ttl":              {ss.DockerPruneTTL(), -time.Hour},
		"docker build-cache max-age":    {ss.DockerBuildCacheMaxAge(), -time.Hour},
		"docker prune interval":         {ss.DockerPruneInterval(), DefaultDockerPruneInterval},
		"docker buildx ttl":             {ss.DockerBuildxCleanupTTL(), DefaultBuildxCleanupTTL},
		"docker buildx interval":        {ss.DockerBuildxCleanupInterval(), DefaultBuildxCleanupInterval},
		"docker shared-volume interval": {ss.DockerSharedVolumeCleanupInterval(), DefaultSharedVolumeCleanupInterval},
		"tart cache max-age":            {ss.TartCacheMaxAge(), DefaultTartCacheMaxAge},
		"tart cache interval":           {ss.TartCacheCleanupInterval(), DefaultTartCacheCleanupInterval},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", name, tc.got, tc.want)
		}
	}
}

func TestRetentionAccessorsKeepExplicitValues(t *testing.T) {
	ss := ScaleSetConfig{
		Docker: DockerConfig{PruneTTL: time.Hour, BuildCacheMaxAge: 2 * time.Hour, PruneInterval: 3 * time.Hour, BuildxCleanupTTL: 4 * time.Hour, BuildxCleanupInterval: 5 * time.Hour, SharedVolumeCleanupInterval: 6 * time.Hour},
		Tart:   TartConfig{CacheMaxAge: 7 * time.Hour, CacheCleanupInterval: 8 * time.Hour},
	}
	got := []time.Duration{ss.DockerPruneTTL(), ss.DockerBuildCacheMaxAge(), ss.DockerPruneInterval(), ss.DockerBuildxCleanupTTL(), ss.DockerBuildxCleanupInterval(), ss.DockerSharedVolumeCleanupInterval(), ss.TartCacheMaxAge(), ss.TartCacheCleanupInterval()}
	for i, d := range got {
		if want := time.Duration(i+1) * time.Hour; d != want {
			t.Errorf("accessor %d = %v, want the explicit %v", i, d, want)
		}
	}
}

func TestDiskConfigResolveAppliesDefaultsAndParses(t *testing.T) {
	got, err := DiskConfig{}.Resolve()
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if got.MinFree != DefaultDiskMinFree || got.TargetFree != DefaultDiskTargetFree || got.MinFreeThreshold.Percent != 10 || got.TargetFreeThreshold.Percent != 20 || got.Interval != DefaultDiskGuardInterval || got.MaxTier != DefaultDiskMaxTier {
		t.Fatalf("defaults resolved to %+v", got)
	}
	got, err = DiskConfig{MinFree: "5GB", TargetFree: "10GB", Interval: -time.Minute, MaxTier: 4}.Resolve()
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if got.MinFreeThreshold.Bytes != 5<<30 || got.TargetFreeThreshold.Bytes != 10<<30 || got.Interval != DefaultDiskGuardInterval || got.MaxTier != 4 {
		t.Fatalf("explicit resolved to %+v", got)
	}
	if _, err := (DiskConfig{MinFree: "30%"}).Resolve(); err == nil {
		t.Fatal("min-free above the default target-free resolved")
	}
}

func TestRetryConfigDelayAndCooldown(t *testing.T) {
	r := DefaultRetry()
	if r != (RetryConfig{Initial: DefaultRetryInitial, Max: DefaultRetryMax, CooldownAfter: DefaultRetryCooldownAfter, Cooldown: DefaultRetryCooldown}) {
		t.Fatalf("DefaultRetry() = %+v, want the named defaults", r)
	}
	if r.Delay(1, 0) != 2500*time.Millisecond || r.Delay(1, 1) != 5*time.Second || r.Delay(2, 1) != 10*time.Second {
		t.Fatalf("ramp = %v %v %v", r.Delay(1, 0), r.Delay(1, 1), r.Delay(2, 1))
	}
	if r.CoolingDown(4) || !r.CoolingDown(5) || r.Delay(5, 0.5) != 5*time.Minute {
		t.Fatal("the fifth consecutive failure must enter the cooldown")
	}
}

func TestDrainTimeoutOrDefault(t *testing.T) {
	zero, negative, custom := time.Duration(0), -time.Second, 3*time.Hour
	for _, tc := range []struct {
		in   *time.Duration
		want time.Duration
	}{{nil, DefaultDrainTimeout}, {&zero, 0}, {&negative, -time.Second}, {&custom, 3 * time.Hour}} {
		if got := DrainTimeoutOrDefault(tc.in); got != tc.want {
			t.Errorf("DrainTimeoutOrDefault(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
