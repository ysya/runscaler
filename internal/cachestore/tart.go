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

// Path returns the configured TART_HOME, falling back to tart's own default
// ($HOME/.tart) when unset — never the system disk. On a host where
// TART_HOME points at a dedicated volume (e.g. /Volumes/FrankData, 931 GB)
// while Docker lives on the system disk, the disk guard groups stores by
// filesystem; returning the wrong path here would make its thresholds
// meaningless for this store. os.UserHomeDir() is a plain env-var read on
// Unix, not a syscall, so — unlike the Docker stores' one-time Info()
// resolution in docker.go — there is no need to cache this at construction
// time; it is cheap enough for the disk guard's per-check Path() call on
// every store.
func (s *tartStore) Path() string {
	if s.cfg.Home != "" {
		return s.cfg.Home
	}
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
