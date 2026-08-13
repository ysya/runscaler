package cachestore

import (
	"context"
	"errors"
	"fmt"
	"time"

	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/backend"
)

// dockerInfoTimeout bounds the one-time Info() call the Docker daemon
// stores make to resolve the daemon's data root. Every other blocking
// Docker API call in this codebase is timeout-bounded (see
// cleanupSharedDockerTimeout, and the Ping calls in
// cmd_validate.go/cmd_doctor.go) so a wedged daemon can never hang the
// caller; a plain Info() call is lighter than those, so a shorter bound is
// enough.
const dockerInfoTimeout = 10 * time.Second

// dockerReclaimTimeout bounds every prune call dockerGarbageStore and
// dockerBuildCacheStore make to the Docker daemon, mirroring the bound the
// retired backend.PruneDockerRuntime used to carry
// (context.WithTimeout(ctx, 10*time.Minute) around its own prune calls) —
// see volumeHelperTimeout in volume.go for the equivalent bound on the
// volume-backed stores. Without this, a wedged daemon blocks the sweeper
// goroutine (or the disk guard's sweep) forever; this is the same class of
// bug commit 6cc66b0 fixed for the exit-time cleanup path.
const dockerReclaimTimeout = 10 * time.Minute

// resolveDockerRootDir resolves the Docker daemon's data-root directory
// once, bounded by dockerInfoTimeout so a wedged daemon cannot hang store
// construction. A failed lookup returns "" rather than an error — the disk
// guard already treats an unresolvable Path() as "skip this store, warn"
// (see StatFor errors), so degrading quietly here is consistent with that.
func resolveDockerRootDir(client backend.DockerAPI) string {
	ctx, cancel := context.WithTimeout(context.Background(), dockerInfoTimeout)
	defer cancel()
	info, err := client.Info(ctx, dockerclient.InfoOptions{})
	if err != nil {
		return ""
	}
	return info.Info.DockerRootDir
}

// --- Garbage store: stopped containers + dangling images (Tier1 only) ---
//
// This is a separate store from the build-cache one below because a store
// carries exactly one Kind, and the disk guard selects stores per tier via
// TiersFor(store.Kind()). Combining daemon garbage (Tier1) with build cache
// (Tier2) under one KindGarbage store left the build-cache branch
// unreachable from the guard (TiersFor(KindGarbage) == {Tier1} only) —
// losing the largest reclaim source on a pressured host (59.8 GB observed
// on one host).

// DockerGarbageConfig mirrors the [docker] prune settings for
// already-unreferenced daemon objects.
type DockerGarbageConfig struct {
	Enabled  bool
	PruneTTL time.Duration
}

type dockerGarbageStore struct {
	client  backend.DockerAPI
	cfg     DockerGarbageConfig
	rootDir string
}

// NewDockerGarbageStore reclaims stopped containers and dangling images.
// It is KindGarbage — everything it removes is already unreferenced — and
// so participates only in Tier1 (see TiersFor).
//
// Path() is resolved once here via the daemon's Info() rather than on every
// call, since the disk guard's cheap pre-job path calls Path() on every
// store (to group them by filesystem) and must stay statfs-cheap — it must
// not pay a daemon round trip per store on every check.
func NewDockerGarbageStore(client backend.DockerAPI, cfg DockerGarbageConfig) CacheStore {
	return &dockerGarbageStore{client: client, cfg: cfg, rootDir: resolveDockerRootDir(client)}
}

func (s *dockerGarbageStore) Name() string    { return "docker-garbage" }
func (s *dockerGarbageStore) Kind() StoreKind { return KindGarbage }
func (s *dockerGarbageStore) Path() string    { return s.rootDir }
func (s *dockerGarbageStore) Enabled() bool   { return s.cfg.Enabled }

// Measure reports the daemon's current image total. TotalSize is a signed
// daemon-reported total; a daemon reporting nothing (or the "unknown"
// sentinel) gives <= 0, which must not wrap around when returned as uint64.
func (s *dockerGarbageStore) Measure(ctx context.Context) (uint64, error) {
	usage, err := s.client.DiskUsage(ctx, dockerclient.DiskUsageOptions{Images: true})
	if err != nil {
		return 0, fmt.Errorf("docker disk usage: %w", err)
	}
	if usage.Images.TotalSize > 0 {
		return uint64(usage.Images.TotalSize), nil
	}
	return 0, nil
}

// Reclaim prunes stopped containers and dangling images older than
// PruneTTL at Tier1; every other tier is a no-op. Enabled() is deliberately
// NOT checked here (revised 2026-08-14 — see store.go's Enabled doc
// comment): a disabled store still means the operator left the *periodic*
// prune sweep off, not "never touch this" — cmd/runner's sweeper is what
// consults Enabled() to decide whether to run at all, so by the time this
// method is reached from that path Enabled() is already known true; the
// disk guard reaches this method regardless, on purpose.
func (s *dockerGarbageStore) Reclaim(ctx context.Context, tier Tier) (uint64, error) {
	if tier != Tier1 {
		return 0, nil
	}
	if s.cfg.PruneTTL <= 0 {
		return 0, nil
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, dockerReclaimTimeout)
	defer cancel()

	var freed uint64
	var errs []error

	// Mirrors PruneDockerRuntime's filter construction exactly — `until`
	// parses as a Go duration string on the daemon side (see
	// getUntilFromPruneFilters in moby's daemon/prune.go), verified against
	// moby v28.5.2 — and continues past a container-prune failure to still
	// attempt the image prune, matching that function's resilience.
	untilFilter := make(dockerclient.Filters).Add("until", s.cfg.PruneTTL.String())
	if r, err := s.client.ContainerPrune(timeoutCtx, dockerclient.ContainerPruneOptions{Filters: untilFilter}); err != nil {
		errs = append(errs, fmt.Errorf("prune stopped containers: %w", err))
	} else {
		freed += r.Report.SpaceReclaimed
	}

	imageFilters := make(dockerclient.Filters).Add("dangling", "true").Add("until", s.cfg.PruneTTL.String())
	if r, err := s.client.ImagePrune(timeoutCtx, dockerclient.ImagePruneOptions{Filters: imageFilters}); err != nil {
		errs = append(errs, fmt.Errorf("prune dangling images: %w", err))
	} else {
		freed += r.Report.SpaceReclaimed
	}

	return freed, errors.Join(errs...)
}

// Budget reports no cap: already-unreferenced garbage has no retention
// policy to enforce — it is removed by PruneTTL alone, not a size budget.
func (s *dockerGarbageStore) Budget() (uint64, string) { return 0, "" }

// --- Build-cache store: BuildKit layer cache (Tier2 trim, Tier4 wipe) ---

// DockerBuildCacheConfig mirrors the [docker] build-cache settings.
type DockerBuildCacheConfig struct {
	Enabled  bool
	MaxAge   time.Duration
	BudgetGB int
}

type dockerBuildCacheStore struct {
	client  backend.DockerAPI
	cfg     DockerBuildCacheConfig
	rootDir string
}

// NewDockerBuildCacheStore reclaims BuildKit's build cache. It is
// KindCache — removing it only costs time on the next build — and
// participates in Tier2 (age/budget trim) and Tier4 (unconditional wipe,
// the guard's emergency tier; see TiersFor).
func NewDockerBuildCacheStore(client backend.DockerAPI, cfg DockerBuildCacheConfig) CacheStore {
	return &dockerBuildCacheStore{client: client, cfg: cfg, rootDir: resolveDockerRootDir(client)}
}

func (s *dockerBuildCacheStore) Name() string    { return "docker-build-cache" }
func (s *dockerBuildCacheStore) Kind() StoreKind { return KindCache }
func (s *dockerBuildCacheStore) Path() string    { return s.rootDir }
func (s *dockerBuildCacheStore) Enabled() bool   { return s.cfg.Enabled }

// Measure reports the daemon's current build-cache total (see
// dockerGarbageStore.Measure for why a <= 0 total is treated as zero).
func (s *dockerBuildCacheStore) Measure(ctx context.Context) (uint64, error) {
	usage, err := s.client.DiskUsage(ctx, dockerclient.DiskUsageOptions{BuildCache: true})
	if err != nil {
		return 0, fmt.Errorf("docker disk usage: %w", err)
	}
	if usage.BuildCache.TotalSize > 0 {
		return uint64(usage.BuildCache.TotalSize), nil
	}
	return 0, nil
}

// Reclaim trims build cache by age and/or budget at Tier2, and performs an
// unconditional full wipe at Tier4; every other tier is a no-op. Enabled()
// is deliberately NOT checked here — see dockerGarbageStore.Reclaim's
// identical note and store.go's Enabled doc comment (revised 2026-08-14):
// the disk guard reaches this method regardless of Enabled(), by design.
func (s *dockerBuildCacheStore) Reclaim(ctx context.Context, tier Tier) (uint64, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, dockerReclaimTimeout)
	defer cancel()
	switch tier {
	case Tier2:
		return s.reclaimByAgeAndBudget(timeoutCtx)
	case Tier4:
		return s.reclaimAll(timeoutCtx)
	default:
		return 0, nil
	}
}

// reclaimByAgeAndBudget matches PruneDockerRuntime's two-call shape
// exactly: age and budget are separate BuildCachePrune calls (a Filters
// value and MaxUsedSpace are never combined into one call there), and a
// failure in one does not skip the other.
func (s *dockerBuildCacheStore) reclaimByAgeAndBudget(ctx context.Context) (uint64, error) {
	if s.cfg.MaxAge <= 0 && s.cfg.BudgetGB <= 0 {
		return 0, nil
	}
	var freed uint64
	var errs []error

	if s.cfg.MaxAge > 0 {
		opts := dockerclient.BuildCachePruneOptions{
			All:     true,
			Filters: make(dockerclient.Filters).Add("until", s.cfg.MaxAge.String()),
		}
		if r, err := s.client.BuildCachePrune(ctx, opts); err != nil {
			errs = append(errs, fmt.Errorf("prune build cache by age: %w", err))
		} else {
			freed += r.Report.SpaceReclaimed
		}
	}

	if s.cfg.BudgetGB > 0 {
		// MaxUsedSpace is BuildKit's hard cache cap. ReservedSpace means the
		// opposite (bytes protected from pruning) and must not be used here.
		opts := dockerclient.BuildCachePruneOptions{
			All:          true,
			MaxUsedSpace: int64(s.cfg.BudgetGB) * 1024 * 1024 * 1024,
		}
		if r, err := s.client.BuildCachePrune(ctx, opts); err != nil {
			errs = append(errs, fmt.Errorf("prune build cache to budget: %w", err))
		} else {
			freed += r.Report.SpaceReclaimed
		}
	}

	return freed, errors.Join(errs...)
}

// reclaimAll wipes the entire build cache unconditionally, ignoring
// MaxAge/BudgetGB entirely — no Filters, no MaxUsedSpace. Tier4 is the
// guard's emergency tier, reached only once lower tiers failed to free
// enough space; everything here is regenerable, so nothing is held back.
// This is the whole reason the store is split from the garbage store: it
// needs its own Kind (KindCache) to be reachable at Tier4 at all.
func (s *dockerBuildCacheStore) reclaimAll(ctx context.Context) (uint64, error) {
	r, err := s.client.BuildCachePrune(ctx, dockerclient.BuildCachePruneOptions{All: true})
	if err != nil {
		return 0, fmt.Errorf("wipe build cache: %w", err)
	}
	return r.Report.SpaceReclaimed, nil
}

// Budget reports no cap for the disk guard's independent budget-enforcement
// pass. cfg.BudgetGB is a real cap, but it is already fully consumed by
// reclaimByAgeAndBudget above via BuildKit's own MaxUsedSpace — a
// disk-pressure-gated (Tier2), graceful trim performed by BuildKit itself.
// Surfacing it here too would let the guard's unconditional, pressure-
// independent Tier4 wipe (see Sweep's budget phase) fire on the same
// number that already drives a softer mechanism, entangling the two
// budget concepts the architecture deliberately keeps separate.
func (s *dockerBuildCacheStore) Budget() (uint64, string) { return 0, "" }
