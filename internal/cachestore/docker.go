package cachestore

import (
	"context"
	"errors"
	"fmt"
	"time"

	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/backend"
)

// dockerInfoTimeout bounds the one-time Info() call NewDockerDaemonStore
// makes to resolve the daemon's data root. Every other blocking Docker API
// call in this codebase is timeout-bounded (see cleanupSharedDockerTimeout,
// and the Ping calls in cmd_validate.go/cmd_doctor.go) so a wedged daemon
// can never hang the caller; a plain Info() call is lighter than those, so a
// shorter bound is enough.
const dockerInfoTimeout = 10 * time.Second

// DockerDaemonConfig mirrors the [docker] prune settings for one daemon.
type DockerDaemonConfig struct {
	Enabled            bool
	PruneTTL           time.Duration
	BuildCacheMaxAge   time.Duration
	BuildCacheBudgetGB int
}

type dockerDaemonStore struct {
	client  backend.DockerAPI
	cfg     DockerDaemonConfig
	rootDir string
}

// NewDockerDaemonStore reclaims daemon-owned garbage and build cache. It is
// KindGarbage: everything Tier1 removes is already unreferenced.
//
// Path() is resolved once here via the daemon's Info() rather than on every
// call, since the disk guard's cheap pre-job path calls Path() on every
// store (to group them by filesystem) and must stay statfs-cheap — it must
// not pay a daemon round trip per store on every check. A failed lookup
// leaves Path() empty rather than failing construction; the guard already
// treats an unresolvable path as "skip this store, warn" (see StatFor
// errors), so degrading quietly here is consistent with that.
func NewDockerDaemonStore(client backend.DockerAPI, cfg DockerDaemonConfig) CacheStore {
	s := &dockerDaemonStore{client: client, cfg: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), dockerInfoTimeout)
	defer cancel()
	if info, err := client.Info(ctx, dockerclient.InfoOptions{}); err == nil {
		s.rootDir = info.Info.DockerRootDir
	}
	return s
}

func (s *dockerDaemonStore) Name() string    { return "docker-daemon" }
func (s *dockerDaemonStore) Kind() StoreKind { return KindGarbage }
func (s *dockerDaemonStore) Path() string    { return s.rootDir }
func (s *dockerDaemonStore) Enabled() bool   { return s.cfg.Enabled }

// Measure reports the combined size of Docker images and build cache. Only
// those two categories are requested from the daemon — containers and
// volumes are covered by other stores — which also skips their computation
// server-side. TotalSize is a signed daemon-reported total; a store with
// nothing of a kind (or an "unknown" sentinel) reports <= 0, which must not
// wrap around when added into the unsigned running total.
func (s *dockerDaemonStore) Measure(ctx context.Context) (uint64, error) {
	usage, err := s.client.DiskUsage(ctx, dockerclient.DiskUsageOptions{Images: true, BuildCache: true})
	if err != nil {
		return 0, fmt.Errorf("docker disk usage: %w", err)
	}
	var total uint64
	if usage.Images.TotalSize > 0 {
		total += uint64(usage.Images.TotalSize)
	}
	if usage.BuildCache.TotalSize > 0 {
		total += uint64(usage.BuildCache.TotalSize)
	}
	return total, nil
}

// Reclaim frees stopped containers and dangling images at Tier1, and build
// cache at Tier2. A disabled store prunes nothing — the operator turned it
// off deliberately (prune touches the whole daemon and may affect
// non-runner objects), and the guard must not override that choice.
func (s *dockerDaemonStore) Reclaim(ctx context.Context, tier Tier) (uint64, error) {
	if !s.cfg.Enabled {
		return 0, nil
	}
	switch tier {
	case Tier1:
		return s.reclaimGarbage(ctx)
	case Tier2:
		return s.reclaimBuildCache(ctx)
	default:
		return 0, nil
	}
}

// reclaimGarbage prunes stopped containers and dangling images older than
// PruneTTL. This mirrors PruneDockerRuntime's filter construction exactly —
// `until` parses as a Go duration string on the daemon side (see
// getUntilFromPruneFilters in moby's daemon/prune.go), verified against
// moby v28.5.2 — and continues past a container-prune failure to still
// attempt the image prune, matching that function's resilience.
func (s *dockerDaemonStore) reclaimGarbage(ctx context.Context) (uint64, error) {
	if s.cfg.PruneTTL <= 0 {
		return 0, nil
	}
	var freed uint64
	var errs []error

	untilFilter := make(dockerclient.Filters).Add("until", s.cfg.PruneTTL.String())
	if r, err := s.client.ContainerPrune(ctx, dockerclient.ContainerPruneOptions{Filters: untilFilter}); err != nil {
		errs = append(errs, fmt.Errorf("prune stopped containers: %w", err))
	} else {
		freed += r.Report.SpaceReclaimed
	}

	imageFilters := make(dockerclient.Filters).Add("dangling", "true").Add("until", s.cfg.PruneTTL.String())
	if r, err := s.client.ImagePrune(ctx, dockerclient.ImagePruneOptions{Filters: imageFilters}); err != nil {
		errs = append(errs, fmt.Errorf("prune dangling images: %w", err))
	} else {
		freed += r.Report.SpaceReclaimed
	}

	return freed, errors.Join(errs...)
}

// reclaimBuildCache prunes build cache by age and/or budget, matching
// PruneDockerRuntime's two-call shape exactly: age and budget are separate
// BuildCachePrune calls (MaxUsedSpace and an age Filter are never combined
// into one call there), and a failure in one does not skip the other.
func (s *dockerDaemonStore) reclaimBuildCache(ctx context.Context) (uint64, error) {
	if s.cfg.BuildCacheMaxAge <= 0 && s.cfg.BuildCacheBudgetGB <= 0 {
		return 0, nil
	}
	var freed uint64
	var errs []error

	if s.cfg.BuildCacheMaxAge > 0 {
		opts := dockerclient.BuildCachePruneOptions{
			All:     true,
			Filters: make(dockerclient.Filters).Add("until", s.cfg.BuildCacheMaxAge.String()),
		}
		if r, err := s.client.BuildCachePrune(ctx, opts); err != nil {
			errs = append(errs, fmt.Errorf("prune build cache by age: %w", err))
		} else {
			freed += r.Report.SpaceReclaimed
		}
	}

	if s.cfg.BuildCacheBudgetGB > 0 {
		// MaxUsedSpace is BuildKit's hard cache cap. ReservedSpace means the
		// opposite (bytes protected from pruning) and must not be used here.
		opts := dockerclient.BuildCachePruneOptions{
			All:          true,
			MaxUsedSpace: int64(s.cfg.BuildCacheBudgetGB) * 1024 * 1024 * 1024,
		}
		if r, err := s.client.BuildCachePrune(ctx, opts); err != nil {
			errs = append(errs, fmt.Errorf("prune build cache to budget: %w", err))
		} else {
			freed += r.Report.SpaceReclaimed
		}
	}

	return freed, errors.Join(errs...)
}
