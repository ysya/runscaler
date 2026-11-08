package cachestore

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
)

// BuildxConfig mirrors the [docker] buildx-cleanup settings.
type BuildxConfig struct {
	Enabled bool
	MaxAge  time.Duration
	RootDir string
}

type buildxStore struct {
	client DockerAPI
	cfg    BuildxConfig
}

// NewBuildxStore reclaims orphaned buildx BuildKit builder containers
// (named buildx_buildkit_*) and their `_state` volumes. It is KindGarbage —
// an orphaned builder is already-abandoned data that nothing observes the
// removal of — and so participates only in Tier1 (see TiersFor).
func NewBuildxStore(client DockerAPI, cfg BuildxConfig) CacheStore {
	return serialize(&buildxStore{client: client, cfg: cfg})
}

// Name returns "buildx". cmd/runner/cmd_cache.go keys its "size unknown" rendering off this exact string (Measure below always returns 0) — a rename here must update that map too.
func (s *buildxStore) Name() string    { return "buildx" }
func (s *buildxStore) Kind() StoreKind { return KindGarbage }

// Path returns the daemon data root the caller resolved once per socket.
func (s *buildxStore) Path() string { return s.cfg.RootDir }

// Enabled reports whether the operator left buildx-builder cleanup on —
// meaning cmd/runner's periodic buildx sweeper, and nothing else. It is off
// by default because builders may be actively reused across builds on a
// daemon runner does not own. The disk guard ignores it and calls
// Reclaim(Tier1) regardless once the disk is under pressure (see store.go's
// Enabled doc comment and the identical notes on docker.go's and tart.go's
// stores); a host whose disk the guard must never touch opts out with
// `[disk] guard = false`.
func (s *buildxStore) Enabled() bool { return s.cfg.Enabled }

// Measure always returns 0. Unlike the daemon's image/build-cache totals
// (DiskUsage's Images/BuildCache fields, see docker.go), there is no cheap
// way to report just what this store would free: it would require
// correlating each orphaned builder container with its `_state` volume's
// size, which the Docker API only exposes per-volume via an expensive
// verbose disk-usage call — effectively a second implementation of
// cleanupOrphanedBuildxBuilders' own name-matching logic, for
// a number nothing downstream depends on (see the comment in Reclaim).
func (s *buildxStore) Measure(_ context.Context) (uint64, error) {
	return 0, nil
}

// Reclaim removes orphaned buildx builders older than cfg.MaxAge at Tier1;
// every other tier is a no-op. Enabled() is deliberately NOT checked here —
// see dockerGarbageStore.Reclaim's identical note and store.go's Enabled
// doc comment: the disk guard reaches this method regardless of Enabled(),
// by design.
func (s *buildxStore) Reclaim(ctx context.Context, tier Tier) (uint64, error) {
	if tier != Tier1 {
		return 0, nil
	}

	if err := cleanupOrphanedBuildxBuilders(ctx, s.client, s.cfg.MaxAge, slog.Default()); err != nil {
		return 0, fmt.Errorf("cleanup orphaned buildx builders: %w", err)
	}

	// cleanupOrphanedBuildxBuilders reports no reclaimed-space total —
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

// buildxBuilderPrefix is the name prefix Docker gives to BuildKit builder
// containers and their state volumes (e.g. buildx_buildkit_builder-<uuid>0).
const buildxBuilderPrefix = "buildx_buildkit_"

// buildxContainerName returns the unprefixed name of a buildx BuildKit builder
// container, or "" if the container is not one.
func buildxContainerName(c container.Summary) string {
	for _, n := range c.Names {
		n = strings.TrimPrefix(n, "/")
		if strings.HasPrefix(n, buildxBuilderPrefix) {
			return n
		}
	}
	return ""
}

// cleanupOrphanedBuildxBuilders removes buildx BuildKit builder containers
// (named buildx_buildkit_*) older than maxAge, along with their named `_state`
// volumes. Such builders are created by `docker buildx create` (e.g. via
// docker/setup-buildx-action); on a persistent host sharing one Docker daemon
// they accumulate when per-job cleanup never runs, each leaving behind a
// multi-GB state volume. maxAge is kept well above any realistic build so a
// sweep never disrupts an in-progress build. A no-op when maxAge <= 0.
func cleanupOrphanedBuildxBuilders(ctx context.Context, client DockerAPI, maxAge time.Duration, logger *slog.Logger) error {
	if maxAge <= 0 {
		return nil
	}

	containerResult, err := client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}

	cutoff := time.Now().Add(-maxAge)
	var removedContainers, removedVolumes int

	for _, c := range containerResult.Items {
		name := buildxContainerName(c)
		if name == "" {
			continue
		}
		// Created is unix seconds; skip young builders that may back an
		// in-progress build.
		if time.Unix(c.Created, 0).After(cutoff) {
			continue
		}

		if _, err := client.ContainerRemove(ctx, c.ID, dockerclient.ContainerRemoveOptions{Force: true}); err != nil {
			logger.Warn("Failed to remove orphaned buildx builder",
				slog.String("container", name), slog.Any("error", err))
			continue
		}
		removedContainers++

		// `docker rm` leaves named volumes intact; the builder's state lives in
		// "<container-name>_state", so remove it explicitly.
		stateVol := name + "_state"
		if _, err := client.VolumeRemove(ctx, stateVol, dockerclient.VolumeRemoveOptions{Force: true}); err != nil {
			logger.Debug("Failed to remove buildx state volume",
				slog.String("volume", stateVol), slog.Any("error", err))
		} else {
			removedVolumes++
		}
	}

	// Also reap dangling buildx state volumes whose containers were already
	// gone (e.g. from a partial manual cleanup). Dangling means unreferenced,
	// so removal is safe regardless of age.
	danglingFilter := make(dockerclient.Filters).Add("dangling", "true")
	if volList, err := client.VolumeList(ctx, dockerclient.VolumeListOptions{Filters: danglingFilter}); err != nil {
		logger.Debug("Failed to list volumes for buildx cleanup", slog.Any("error", err))
	} else {
		for _, v := range volList.Items {
			if !strings.HasPrefix(v.Name, buildxBuilderPrefix) {
				continue
			}
			if _, err := client.VolumeRemove(ctx, v.Name, dockerclient.VolumeRemoveOptions{Force: true}); err != nil {
				logger.Debug("Failed to remove dangling buildx state volume",
					slog.String("volume", v.Name), slog.Any("error", err))
				continue
			}
			removedVolumes++
		}
	}

	if removedContainers > 0 || removedVolumes > 0 {
		logger.Info("Removed orphaned buildx builders",
			slog.Int("containers", removedContainers),
			slog.Int("volumes", removedVolumes))
	}
	return nil
}
