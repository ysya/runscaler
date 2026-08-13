package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	dockerclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/backend"
	"github.com/ysya/runscaler/internal/cachestore"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/diskguard"
)

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Show disk usage of every configured cache store",
	Long: `Measure every cache store this config defines — Docker's dangling images
and stopped containers, its build cache, orphaned buildx builders, the
shared volume, named cache volumes, and Tart's image cache — and print how
much space each is using, the filesystem it lives on, that filesystem's
current free percentage, and any retention policy configured for it.

This calls Measure() on every store, which can be expensive: some stores
run a short-lived helper container to walk a volume. For a cheap,
filesystem-level view that is safe to poll instead, see the "disk" section
of 'runner status --json'.`,
	Example: `  runner cache
  runner cache --json`,
	RunE: runCache,
}

func init() {
	cacheCmd.Flags().Bool("json", false, "Output as JSON")
}

// cacheMeasureTimeout bounds each store's Measure() call. Several stores
// this reaches call the Docker daemon or run `du` with no internal timeout
// of their own — dockerGarbageStore/dockerBuildCacheStore/tartStore's
// Measure, unlike their own Reclaim (dockerReclaimTimeout) or the
// volume-backed stores' Measure (internally bounded by volumeHelperTimeout;
// see cachestore/docker.go, tart.go, and volume.go) — so without a bound
// here, an unreachable daemon or a wedged `du` would hang this command
// forever. Matches those two constants' own value: long enough for a real
// volume walk, still finite. Applied fresh per store (not once for the
// whole command) so one slow or wedged store cannot eat into the time
// budget of the stores measured after it.
const cacheMeasureTimeout = 10 * time.Minute

// cacheRow is one line of `runner cache`'s output: how much space one
// cachestore.CacheStore is using, the filesystem it lives on and that
// filesystem's current free percentage, its retention policy if it has
// one, and — if either failed — why. JSON tags follow internal/health's
// style (snake_case, omitempty for data that is not always present).
type cacheRow struct {
	Store      string `json:"store"`
	Filesystem string `json:"filesystem"`
	SizeBytes  uint64 `json:"size_bytes"`
	// SizeUnknown is true when SizeBytes is not a real measurement — e.g.
	// buildxStore.Measure hardcodes 0 because there is no cheap way for it
	// to report a real total (see cachestore/buildx.go's Measure doc
	// comment). A bare "0" would read as "this store is empty"; SizeUnknown
	// lets the table print "unknown" instead of a misleading "0 B", and
	// lets a script tell a genuinely empty store (SizeBytes: 0, this field
	// absent — omitempty drops false) apart from an unmeasurable one
	// (SizeUnknown: true).
	SizeUnknown   bool    `json:"size_unknown,omitempty"`
	FSFreePercent float64 `json:"fs_free_percent"`
	Policy        string  `json:"policy,omitempty"`
	// MeasureError carries either Measure's or StatFor's failure for this
	// row — both mean "this row's numbers could not be fully determined",
	// so they share one field rather than forcing a script to check two.
	// A store that fails to measure still gets a row, with this set: a
	// missing row would read as "nothing there", not "could not be read".
	MeasureError string `json:"measure_error,omitempty"`
}

// hardcodedZeroMeasureStores lists the CacheStore.Name() values whose
// Measure is documented to always return (0, nil) as a placeholder, not a
// real reading — currently just buildxStore ("buildx"): correlating each
// orphaned builder with its _state volume's size would need a second
// implementation of its own prune logic, for a number nothing downstream
// consumes (see cachestore/buildx.go's Measure doc comment). CacheStore
// exposes no way to tell "really zero" from "unknown" apart other than by
// name — extending the interface to do so is outside this command's file
// scope — so this is a deliberate, narrow, name-based special case.
var hardcodedZeroMeasureStores = map[string]bool{
	"buildx": true,
}

// isUnmeasurableZero reports whether a store's own (0, nil) Measure result
// should be treated as "unknown" rather than a genuine zero. Checking
// size == 0 as well as the name means that if a future version of a named
// store starts returning a real total, it stops being flagged automatically
// instead of silently hiding a real number behind "unknown".
func isUnmeasurableZero(name string, size uint64) bool {
	return size == 0 && hardcodedZeroMeasureStores[name]
}

// policyString renders a store's Budget() as an operator-readable policy,
// e.g. "budget=20.0 GiB on-exceed=warn". A zero budget means no policy is
// configured (CacheStore.Budget's own doc comment), rendered as "".
//
// Several stores that do have a real retention setting (docker-build-cache
// and shared-volume's MaxAge, tart's MaxAge) deliberately hard-return
// (0, "") from Budget() regardless — see each store's own Budget() doc
// comment: it is already fully consumed by their own Reclaim(Tier2)/(Tier3)
// via a tool-native trim, and surfacing it here too would let the disk
// guard's separate, unconditional budget-enforcement pass double-drive the
// same number. Their Policy column is legitimately empty for that reason —
// CacheStore has no method to read those settings back, only Budget().
func policyString(budgetBytes uint64, onExceed string) string {
	if budgetBytes == 0 {
		return ""
	}
	if onExceed == "" {
		onExceed = "warn"
	}
	return fmt.Sprintf("budget=%s on-exceed=%s", backend.FormatBytes(budgetBytes), onExceed)
}

// buildCacheRows measures every store and stats the filesystem it lives on,
// building one row per store. A store that fails either call still gets a
// row (see cacheRow.MeasureError's doc comment) — the rest proceed
// normally. Rows are sorted by (filesystem, store name) so the table reads
// grouped by disk and output is stable across runs, since the stores
// buildCacheStores returns are not in a deterministic order (map
// iteration internally).
func buildCacheRows(stores []cachestore.CacheStore) []cacheRow {
	rows := make([]cacheRow, 0, len(stores))
	for _, s := range stores {
		row := cacheRow{Store: s.Name(), Filesystem: s.Path()}

		budgetBytes, onExceed := s.Budget()
		row.Policy = policyString(budgetBytes, onExceed)

		var errs []string

		measureCtx, cancel := context.WithTimeout(context.Background(), cacheMeasureTimeout)
		size, err := s.Measure(measureCtx)
		cancel()
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("measure: %v", err))
		case isUnmeasurableZero(s.Name(), size):
			row.SizeUnknown = true
		default:
			row.SizeBytes = size
		}

		if st, err := diskguard.StatFor(s.Path()); err != nil {
			errs = append(errs, fmt.Sprintf("stat filesystem: %v", err))
		} else if st.TotalBytes > 0 {
			row.FSFreePercent = float64(st.FreeBytes) / float64(st.TotalBytes) * 100
		}

		row.MeasureError = strings.Join(errs, "; ")
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Filesystem != rows[j].Filesystem {
			return rows[i].Filesystem < rows[j].Filesystem
		}
		return rows[i].Store < rows[j].Store
	})
	return rows
}

// formatCacheTable renders rows as a tabwriter-aligned table. It never
// sorts — buildCacheRows already decided the order — so it stays a pure
// renderer of whatever it is given, which is what makes it directly
// testable against hand-built rows.
func formatCacheTable(rows []cacheRow) string {
	var buf strings.Builder
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STORE\tFILESYSTEM\tSIZE\tFS FREE\tPOLICY\tERROR")
	for _, r := range rows {
		size := backend.FormatBytes(r.SizeBytes)
		if r.SizeUnknown {
			size = "unknown"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%.0f%%\t%s\t%s\n",
			r.Store, r.Filesystem, size, r.FSFreePercent, r.Policy, r.MeasureError)
	}
	w.Flush()
	return buf.String()
}

func runCache(cmd *cobra.Command, args []string) error {
	jsonOutput, _ := cmd.Flags().GetBool("json")

	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		fmt.Printf("  ⚠ config: %s\n", w)
	}

	scaleSets := cfg.ResolveScaleSets()
	logger := config.NewLogger(cfg.LogLevel, cfg.LogFormat)

	// One best-effort client per distinct Docker socket, like run()'s own
	// loop — but deliberately without run()/validate's Ping check: an
	// unreachable daemon must not make its stores vanish from the table, it
	// must make them appear with their error (see cacheRow.MeasureError —
	// the actual failure surfaces there, per-store, once Measure is
	// attempted).
	dockerClients := make(map[string]*dockerclient.Client)
	defer func() {
		for _, c := range dockerClients {
			_ = c.Close()
		}
	}()
	for socket := range groupDockerScaleSets(scaleSets) {
		client, err := dockerclient.New(
			dockerclient.FromEnv,
			dockerclient.WithHost("unix://"+socket),
		)
		if err != nil {
			fmt.Printf("  ⚠ cache: cannot create Docker client for %s: %s\n", socket, err)
			continue
		}
		dockerClients[socket] = client
	}

	stores := buildCacheStores(scaleSets, dockerClients, logger)
	rows := buildCacheRows(stores)

	if jsonOutput {
		data, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal cache rows: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	if len(rows) == 0 {
		fmt.Println("No cache stores configured.")
		return nil
	}
	fmt.Print(formatCacheTable(rows))
	return nil
}
