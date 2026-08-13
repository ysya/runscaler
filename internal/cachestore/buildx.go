package cachestore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ysya/runscaler/internal/backend"
)

// BuildxConfig mirrors the [docker] buildx-cleanup settings.
type BuildxConfig struct {
	Enabled bool
	MaxAge  time.Duration
	RootDir string
}

type buildxStore struct {
	client backend.DockerAPI
	cfg    BuildxConfig
}

// NewBuildxStore reclaims orphaned buildx BuildKit builder containers
// (named buildx_buildkit_*) and their `_state` volumes. It is KindGarbage —
// an orphaned builder is already-abandoned data that nothing observes the
// removal of — and so participates only in Tier1 (see TiersFor).
func NewBuildxStore(client backend.DockerAPI, cfg BuildxConfig) CacheStore {
	return &buildxStore{client: client, cfg: cfg}
}

func (s *buildxStore) Name() string    { return "buildx" }
func (s *buildxStore) Kind() StoreKind { return KindGarbage }

// Path returns cfg.RootDir directly rather than resolving it from the
// daemon's Info() the way docker.go's stores do at construction time: the
// brief's config struct already carries RootDir as a field for the caller
// (Task 7's wiring) to fill in once and share — the same precedent
// SharedVolumeConfig/CacheVolumeConfig set in volume.go, not the one
// DockerGarbageConfig/DockerBuildCacheConfig set in docker.go.
func (s *buildxStore) Path() string { return s.cfg.RootDir }

// Enabled reports whether the operator left buildx-builder cleanup on.
// Builders may be actively reused across builds, so a disabled store must
// not remove any of them.
func (s *buildxStore) Enabled() bool { return s.cfg.Enabled }

// Measure always returns 0. Unlike the daemon's image/build-cache totals
// (DiskUsage's Images/BuildCache fields, see docker.go), there is no cheap
// way to report just what this store would free: it would require
// correlating each orphaned builder container with its `_state` volume's
// size, which the Docker API only exposes per-volume via an expensive
// verbose disk-usage call — effectively a second implementation of
// CleanupOrphanedBuildxBuilders' own (unexported) name-matching logic, for
// a number nothing downstream depends on (see the comment in Reclaim).
func (s *buildxStore) Measure(_ context.Context) (uint64, error) {
	return 0, nil
}

// Reclaim removes orphaned buildx builders older than cfg.MaxAge at Tier1;
// every other tier is a no-op. A disabled store removes nothing — the
// operator turned this off deliberately.
func (s *buildxStore) Reclaim(ctx context.Context, tier Tier) (uint64, error) {
	if !s.cfg.Enabled || tier != Tier1 {
		return 0, nil
	}

	if err := backend.CleanupOrphanedBuildxBuilders(ctx, s.client, s.cfg.MaxAge, slog.Default()); err != nil {
		return 0, fmt.Errorf("cleanup orphaned buildx builders: %w", err)
	}

	// CleanupOrphanedBuildxBuilders reports no reclaimed-space total —
	// unlike ContainerPrune/ImagePrune/BuildCachePrune's Report.SpaceReclaimed
	// (see dockerGarbageStore/dockerBuildCacheStore in docker.go), it only
	// issues individual ContainerRemove/VolumeRemove calls with no daemon-side
	// sum. Returning 0 here is deliberate, not a placeholder: the disk guard
	// re-runs statfs after each tier to judge whether the free-space target
	// was met, so it never depends on a store's self-reported total.
	return 0, nil
}

// Budget reports no cap: orphaned builders are already-abandoned garbage
// (see NewBuildxStore), not a retained cache with a size policy to enforce.
func (s *buildxStore) Budget() (uint64, string) { return 0, "" }
