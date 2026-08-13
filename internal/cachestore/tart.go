package cachestore

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/ysya/runscaler/internal/backend"
)

// TartConfig mirrors the [tart] cache-cleanup settings.
type TartConfig struct {
	Enabled  bool
	Home     string
	MaxAge   time.Duration
	BudgetGB int
}

type tartStore struct {
	runner backend.CommandRunner
	cfg    TartConfig
}

// NewTartStore reclaims Tart's OCI/IPSW image cache — not local VM disks.
// It is KindCache — regenerable, only costing a re-pull on the next VM
// start — and structurally participates in Tier2 and Tier4 (see TiersFor);
// Reclaim below keeps Tier4 a permanent no-op by design (see its comment).
func NewTartStore(runner backend.CommandRunner, cfg TartConfig) CacheStore {
	return &tartStore{runner: runner, cfg: cfg}
}

func (s *tartStore) Name() string    { return "tart-cache" }
func (s *tartStore) Kind() StoreKind { return KindCache }

// Path returns the configured TART_HOME — never the system disk — resolved
// in the same three levels and order as internal/backend/tart.go's
// setVMMAC: cfg.Home, then the TART_HOME environment variable, then tart's
// own default ($HOME/.tart). This must keep tracking setVMMAC's resolution
// exactly: an operator can set TART_HOME via the environment instead of
// [tart].home (execCommandRunner only injects TART_HOME into child
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
	// Path() as "skip this store, warn" (see resolveDockerRootDir in
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

// Reclaim trims the Tart cache by age and/or budget at Tier2 via the
// existing backend.PruneTartCache; Tier4 is deliberately never a wholesale
// wipe, and every other tier is a no-op. A disabled store reclaims nothing.
func (s *tartStore) Reclaim(ctx context.Context, tier Tier) (uint64, error) {
	if !s.cfg.Enabled {
		return 0, nil
	}

	switch tier {
	case Tier2:
		if err := backend.PruneTartCache(ctx, s.cfg.Home, s.cfg.MaxAge, s.cfg.BudgetGB, slog.Default()); err != nil {
			return 0, fmt.Errorf("prune tart cache: %w", err)
		}
		// PruneTartCache reports no reclaimed-space total either — 0 here
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
