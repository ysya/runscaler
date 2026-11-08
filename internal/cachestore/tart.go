package cachestore

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ysya/runscaler/internal/provider"
)

// TartConfig mirrors the [tart] cache-cleanup settings.
type TartConfig struct {
	Enabled  bool
	Home     string
	MaxAge   time.Duration
	BudgetGB int
	// KeepImages are the runner images of the scale sets sharing Home; each
	// prune first marks them used so an idle week cannot evict one.
	KeepImages []string
}

type tartStore struct {
	runner provider.CommandRunner
	cfg    TartConfig
}

// NewTartStore reclaims Tart's OCI/IPSW image cache — not local VM disks.
// It is KindCache — regenerable, only costing a re-pull on the next VM
// start — and structurally participates in Tier2 and Tier4 (see TiersFor);
// Reclaim below keeps Tier4 a permanent no-op by design (see its comment).
func NewTartStore(runner provider.CommandRunner, cfg TartConfig) CacheStore {
	return serialize(&tartStore{runner: runner, cfg: cfg})
}

func (s *tartStore) Name() string    { return "tart-cache" }
func (s *tartStore) Kind() StoreKind { return KindCache }

// Path returns the configured TART_HOME — never the system disk — resolved
// in the same three levels and order as internal/provider/tart.go's
// setVMMAC: cfg.Home, then the TART_HOME environment variable, then tart's
// own default ($HOME/.tart). This must keep tracking setVMMAC's resolution
// exactly: an operator can set TART_HOME via the environment instead of
// [tart].home (provider.TartCommandRunner only injects TART_HOME into child
// processes when cfg.Home is non-empty, otherwise the ambient environment —
// including an operator-exported TART_HOME — passes through untouched), so
// dropping the middle level would make this store report a path tart
// itself never reads or writes from. On a host where TART_HOME points at a
// dedicated volume (e.g. /Volumes/FrankData, 931 GB) while Docker lives on
// the system disk, the disk guard groups stores by filesystem; naming the
// wrong one here makes its thresholds meaningless for this store. Neither
// the env read nor os.UserHomeDir() is a syscall, so — unlike the Docker
// stores' one-time Info() resolution in docker.go — there is no need to
// cache this at construction time; it is cheap enough for the disk guard's
// per-check Path() call on every store.
func (s *tartStore) Path() string {
	if s.cfg.Home != "" {
		return s.cfg.Home
	}
	if env := os.Getenv("TART_HOME"); env != "" {
		return env
	}
	// A failed lookup degrades to "" rather than an error — Path() has no
	// error return, and the disk guard already treats an unresolvable
	// Path() as "skip this store, warn" (see ResolveDockerRootDir in
	// docker.go for the same precedent), so degrading quietly here is
	// consistent with the rest of this package.
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tart")
}

// Enabled reports whether the operator left Tart cache cleanup on.
func (s *tartStore) Enabled() bool { return s.cfg.Enabled }

// Measure runs `du -sb` on the cache subdirectory directly on the host via
// the injected CommandRunner. Tart's cache lives on the local filesystem,
// not in a Docker volume, so — unlike the shared-volume/cache-volume
// stores' Measure in volume.go — there is no container to run it inside.
func (s *tartStore) Measure(ctx context.Context) (uint64, error) {
	out, err := s.runner.Run(ctx, "du", "-sb", filepath.Join(s.Path(), "cache"))
	if err != nil {
		return 0, fmt.Errorf("measure tart cache: %w", err)
	}

	sizes, err := parseDuSizes(string(out), 1)
	if err != nil {
		return 0, fmt.Errorf("parse tart cache du output: %w", err)
	}
	return sizes[0], nil
}

// Reclaim trims the Tart cache by age and/or budget at Tier2 through
// pruneTartCache on the store's own runner; Tier4 is deliberately never a
// wholesale wipe, and every other tier is a no-op. Enabled() is
// deliberately NOT checked here — see dockerGarbageStore.Reclaim's
// identical note and store.go's Enabled doc comment: the disk guard
// reaches this method regardless of Enabled(), by design.
func (s *tartStore) Reclaim(ctx context.Context, tier Tier) (uint64, error) {
	switch tier {
	case Tier2:
		if err := pruneTartCache(ctx, s.runner, s.cfg.Home, s.cfg.MaxAge, s.cfg.BudgetGB, s.cfg.KeepImages, slog.Default()); err != nil {
			return 0, fmt.Errorf("prune tart cache: %w", err)
		}
		// pruneTartCache reports no reclaimed-space total either — 0 here
		// for the same reason buildxStore.Reclaim gives (see its comment):
		// the guard re-verifies via statfs, not a summed self-reported total.
		return 0, nil

	case Tier4:
		// Deliberately NOT a wholesale wipe. Tart's cache backs every VM
		// pull with OCI/IPSW layers, dominated by a full macOS base image
		// (140 GB observed on one host); re-pulling one after an emergency
		// wipe costs far more — in time, network, and disk churn — than any
		// disk pressure the wipe would relieve. tart prune (Tier2, above)
		// already evicts by its own LRU, so an unconditional wipe here would
		// only add that cost, not additional headroom worth having.
		return 0, nil

	default:
		return 0, nil
	}
}

// Budget reports no cap for the disk guard's independent budget-enforcement
// pass. cfg.BudgetGB is a real cap, but it already drives Tier2's
// disk-pressure-gated trim above via `tart prune --space-budget` — tart's
// own LRU eviction, not the guard's. Surfacing it here too would let the
// guard's unconditional Tier4 wipe fire on the same number, which would
// also break the explicit "never wholesale-wipe Tart's cache" decision in
// the Tier4 case above.
func (s *tartStore) Budget() (uint64, string) { return 0, "" }

// pruneTartCache runs `tart prune --entries caches` through runner against
// home, trimming OCI/IPSW caches. Local VMs are never touched. Two criteria
// can apply: age (maxAge, via --older-than) reclaims layers left behind by
// image updates, and an optional size cap (spaceBudgetGB, via
// --space-budget) bounds the total. A no-op when both are off.
func pruneTartCache(ctx context.Context, runner provider.CommandRunner, home string, maxAge time.Duration, spaceBudgetGB int, keepImages []string, logger *slog.Logger) error {
	args := []string{"prune", "--entries", "caches"}
	attrs := []any{slog.String("home", home)}

	if maxAge > 0 {
		// tart --older-than granularity is whole days. Floor sub-day windows to
		// 1 day: --older-than=0 would mean "older than 0 days" and wipe the
		// entire cache, which is never the intent.
		days := int(maxAge.Hours() / 24)
		if days < 1 {
			days = 1
		}
		args = append(args, "--older-than", strconv.Itoa(days))
		attrs = append(attrs, slog.Int("older_than_days", days))
	}
	if spaceBudgetGB > 0 {
		args = append(args, "--space-budget", strconv.Itoa(spaceBudgetGB))
		attrs = append(attrs, slog.Int("space_budget_gb", spaceBudgetGB))
	}
	if len(args) == 3 {
		// No pruning criterion — `tart prune` requires at least one and would
		// error. Treat as a disabled sweep rather than failing.
		return nil
	}

	markTartImagesUsed(ctx, runner, keepImages, logger)

	logger.Debug("Pruning Tart cache", attrs...)
	if _, err := runner.Run(ctx, "tart", args...); err != nil {
		return fmt.Errorf("tart prune: %w", err)
	}
	return nil
}

// markTartImagesUsed protects images from the prune that follows. tart prune
// evicts by last access, and cloning an image is what records an access, so
// each image still in the OCI cache is cloned into a throwaway VM that is
// deleted right away. An image missing from the cache is skipped, since
// cloning it would pull it. Best-effort: failures are logged.
func markTartImagesUsed(ctx context.Context, runner provider.CommandRunner, images []string, logger *slog.Logger) {
	if len(images) == 0 {
		return
	}
	cached, err := provider.ListTartVMs(ctx, runner, "--source", "oci")
	if err != nil {
		logger.Warn("Failed to list the Tart cache; runner images are not protected from this prune",
			slog.Any("error", err))
		return
	}
	present := make(map[string]bool, len(cached))
	for _, entry := range cached {
		present[entry.Name] = true
	}

	for _, image := range images {
		if !present[image] {
			continue
		}
		vm := provider.KeepVMName()
		if _, err := runner.Run(ctx, "tart", "clone", image, vm); err != nil {
			logger.Warn("Failed to mark a runner image as used; this prune may evict it",
				slog.String("image", image), slog.Any("error", err))
			continue
		}
		if _, err := runner.Run(ctx, "tart", "delete", vm); err != nil {
			logger.Warn("Failed to delete a throwaway VM; startup reconciliation will remove it",
				slog.String("vm", vm), slog.Any("error", err))
		}
	}
}
