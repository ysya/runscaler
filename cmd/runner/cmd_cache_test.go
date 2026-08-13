package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ysya/runscaler/internal/cachestore"
)

// fakeCacheStore is a minimal cachestore.CacheStore double for testing
// buildCacheRows / diskStatusesFor without a real Docker daemon or Tart
// installation. measureFn defaults to returning (0, nil) when nil.
type fakeCacheStore struct {
	name        string
	kind        cachestore.StoreKind
	path        string
	enabled     bool
	measureFn   func(context.Context) (uint64, error)
	budgetBytes uint64
	onExceed    string
}

func (f *fakeCacheStore) Name() string               { return f.name }
func (f *fakeCacheStore) Kind() cachestore.StoreKind { return f.kind }
func (f *fakeCacheStore) Path() string               { return f.path }
func (f *fakeCacheStore) Enabled() bool              { return f.enabled }
func (f *fakeCacheStore) Budget() (uint64, string)   { return f.budgetBytes, f.onExceed }
func (f *fakeCacheStore) Reclaim(context.Context, cachestore.Tier) (uint64, error) {
	return 0, nil
}
func (f *fakeCacheStore) Measure(ctx context.Context) (uint64, error) {
	if f.measureFn != nil {
		return f.measureFn(ctx)
	}
	return 0, nil
}

// --- Brief Step 1 tests (verbatim) ---

func TestFormatCacheTable(t *testing.T) {
	rows := []cacheRow{
		{Store: "ccache", Filesystem: "/", SizeBytes: 19 * 1024 * 1024 * 1024,
			FSFreePercent: 38, Policy: "budget=20GB on-exceed=warn"},
		{Store: "runner-shared", Filesystem: "/", SizeBytes: 3 * 1024 * 1024 * 1024,
			FSFreePercent: 38, Policy: "max-age=72h"},
	}
	out := formatCacheTable(rows)
	for _, want := range []string{"ccache", "19.0 GiB", "38%", "budget=20GB", "runner-shared"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestCacheRowsJSONIncludesUnmeasurableStores(t *testing.T) {
	rows := []cacheRow{
		{Store: "ok", SizeBytes: 100},
		{Store: "broken", MeasureError: "permission denied"},
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "broken") || !strings.Contains(string(data), "permission denied") {
		t.Error("a store that failed to measure must still appear, with its error")
	}
}

// --- Additional coverage ---

func TestFormatCacheTableRendersUnknownNotZero(t *testing.T) {
	rows := []cacheRow{{Store: "buildx", Filesystem: "/", SizeUnknown: true, FSFreePercent: 50}}
	out := formatCacheTable(rows)
	if !strings.Contains(out, "unknown") {
		t.Errorf("table output missing %q for an unmeasurable store:\n%s", "unknown", out)
	}
	if strings.Contains(out, "0 B") {
		t.Errorf("table output must not render an unknown size as a real zero:\n%s", out)
	}
}

func TestCacheSubcommandRegistered(t *testing.T) {
	found := false
	for _, c := range cmd.Commands() {
		if c.Name() == "cache" {
			found = true
			break
		}
	}
	if !found {
		t.Error("`cache` subcommand must be registered on root")
	}
}

func TestCacheCommandHasJSONFlag(t *testing.T) {
	if cacheCmd.Flags().Lookup("json") == nil {
		t.Error("`cache` must own a --json flag")
	}
}

func TestBuildCacheRowsEmptyInputYieldsEmptyNotNilSlice(t *testing.T) {
	rows := buildCacheRows(nil)
	if rows == nil {
		t.Fatal("buildCacheRows(nil) = nil, want a non-nil empty slice so --json prints [] not null")
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want empty", rows)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != "[]" {
		t.Errorf("json = %s, want []", data)
	}
}

func TestBuildCacheRowsMeasureErrorStillListed(t *testing.T) {
	broken := &fakeCacheStore{name: "broken", path: "/", measureFn: func(context.Context) (uint64, error) {
		return 0, errors.New("permission denied")
	}}
	rows := buildCacheRows([]cachestore.CacheStore{broken})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly 1 row for the failed store", rows)
	}
	if !strings.Contains(rows[0].MeasureError, "permission denied") {
		t.Errorf("MeasureError = %q, want it to mention the underlying error", rows[0].MeasureError)
	}
}

func TestBuildCacheRowsBuildxHardcodedZeroIsUnknownNotZero(t *testing.T) {
	buildx := &fakeCacheStore{name: "buildx", path: "/"}
	real := &fakeCacheStore{name: "docker-garbage", path: "/", measureFn: func(context.Context) (uint64, error) {
		return 0, nil
	}}

	rows := buildCacheRows([]cachestore.CacheStore{buildx, real})
	byName := make(map[string]cacheRow, len(rows))
	for _, r := range rows {
		byName[r.Store] = r
	}

	if !byName["buildx"].SizeUnknown {
		t.Error("buildx's hardcoded-zero Measure must be reported as SizeUnknown, not a real zero")
	}
	if byName["docker-garbage"].SizeUnknown {
		t.Error("a store that genuinely measured zero must not be marked SizeUnknown")
	}

	// The distinction must survive into --json too, so a script can tell
	// the two apart.
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	for _, r := range raw {
		switch r["store"] {
		case "buildx":
			if v, ok := r["size_unknown"]; !ok || v != true {
				t.Errorf("buildx row JSON = %v, want size_unknown:true", r)
			}
		case "docker-garbage":
			if _, ok := r["size_unknown"]; ok {
				t.Errorf("docker-garbage row JSON = %v, want size_unknown key omitted for a real zero", r)
			}
		}
	}
}

// TestIsUnmeasurableZeroRecognizesRealBuildxStore ties hardcodedZeroMeasureStores
// to cachestore.NewBuildxStore's actual Name(), not just a literal repeated
// in this package's own fakes. Every other test above builds a
// fakeCacheStore{name: "buildx", ...} by hand, so none of them would notice
// if internal/cachestore/buildx.go's Name() were ever renamed — this one
// would fail instead, since it asks the real store what its name is rather
// than assuming. A nil Docker client is fine: Name() never touches it (see
// buildxStore's fields — only Measure/Reclaim/Path use client/cfg).
func TestIsUnmeasurableZeroRecognizesRealBuildxStore(t *testing.T) {
	store := cachestore.NewBuildxStore(nil, cachestore.BuildxConfig{})
	if !isUnmeasurableZero(store.Name(), 0) {
		t.Errorf("isUnmeasurableZero(%q, 0) = false, want true — hardcodedZeroMeasureStores in cmd_cache.go "+
			"must stay in sync with cachestore.NewBuildxStore's real Name() (see cachestore/buildx.go's Name doc comment)",
			store.Name())
	}
}

func TestBuildCacheRowsPolicyFromBudget(t *testing.T) {
	tests := []struct {
		name        string
		budgetBytes uint64
		onExceed    string
		want        string
	}{
		{"no budget configured", 0, "", ""},
		{"budget with explicit on-exceed", 20 * 1024 * 1024 * 1024, "wipe", "budget=20.0 GiB on-exceed=wipe"},
		{"budget with default on-exceed", 1024 * 1024 * 1024, "", "budget=1.0 GiB on-exceed=warn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &fakeCacheStore{name: "s", path: "/", budgetBytes: tt.budgetBytes, onExceed: tt.onExceed}
			rows := buildCacheRows([]cachestore.CacheStore{s})
			if rows[0].Policy != tt.want {
				t.Errorf("Policy = %q, want %q", rows[0].Policy, tt.want)
			}
		})
	}
}

func TestBuildCacheRowsSortedByFilesystemThenStoreName(t *testing.T) {
	sB := &fakeCacheStore{name: "bbb", path: "/"}
	sA := &fakeCacheStore{name: "aaa", path: "/"}
	sC := &fakeCacheStore{name: "ccc", path: "/"}
	rows := buildCacheRows([]cachestore.CacheStore{sB, sA, sC})
	got := []string{rows[0].Store, rows[1].Store, rows[2].Store}
	want := []string{"aaa", "bbb", "ccc"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("order = %v, want %v", got, want)
			break
		}
	}
}

func TestBuildCacheRowsFSFreePercentFromRealPath(t *testing.T) {
	s := &fakeCacheStore{name: "s", path: "/"}
	rows := buildCacheRows([]cachestore.CacheStore{s})
	if rows[0].FSFreePercent < 0 || rows[0].FSFreePercent > 100 {
		t.Errorf("FSFreePercent = %v, want a value in [0, 100] for a real filesystem", rows[0].FSFreePercent)
	}
	if rows[0].MeasureError != "" {
		t.Errorf("MeasureError = %q, want empty for a real path and a store whose Measure defaults to (0, nil)", rows[0].MeasureError)
	}
}

func TestBuildCacheRowsUnstattablePathReportsError(t *testing.T) {
	s := &fakeCacheStore{name: "s", path: "/definitely/does/not/exist/xyz123"}
	rows := buildCacheRows([]cachestore.CacheStore{s})
	if rows[0].MeasureError == "" {
		t.Error("MeasureError must be set when the store's filesystem cannot be stat'd")
	}
}
