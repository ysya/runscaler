package cachestore

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/provider"
)

// scriptedRunner answers commands by exact "name args" key or by prefix and
// records every call, for the prune tests that need more than one canned
// result.
type scriptedRunner struct {
	results map[string]scriptedResult
	calls   []string
}

type scriptedResult struct {
	out string
	err error
}

func (r *scriptedRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, key)
	if res, ok := r.results[key]; ok {
		return []byte(res.out), res.err
	}
	for prefix, res := range r.results {
		if strings.HasPrefix(key, prefix) {
			return []byte(res.out), res.err
		}
	}
	return nil, nil
}

func (r *scriptedRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	_, err := r.Run(ctx, name, args...)
	return err
}

func (r *scriptedRunner) count(prefix string) int {
	n := 0
	for _, call := range r.calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// ociCacheListing is `tart list --format json --source oci` on a host that
// pulled one Xcode image: its tag and the digest the tag points at.
const ociCacheListing = `[
  {"State":"stopped","Disk":140,"Accessed":"2026-10-07T20:47:57Z","Name":"ghcr.io\/cirruslabs\/macos-golden-gate-xcode:27","Source":"OCI","Running":false,"Size":81},
  {"State":"stopped","Disk":140,"Accessed":"2026-10-07T23:00:11Z","Name":"ghcr.io\/cirruslabs\/macos-golden-gate-xcode@sha256:324ea5656dee8ab9b0a0df70fda2cfed8912051ad0eca3b1b883bf6ddac88fab","Source":"OCI","Running":false,"Size":81}
]`

var discard = slog.New(slog.DiscardHandler)

func TestTartStore_Tier2PrunesThroughInjectedRunner(t *testing.T) {
	r := &scriptedRunner{}
	s := NewTartStore(r, TartConfig{Enabled: true, Home: "/Volumes/A", MaxAge: 48 * time.Hour, BudgetGB: 10})
	if _, err := s.Reclaim(context.Background(), Tier2); err != nil {
		t.Fatalf("Reclaim(Tier2) error: %v", err)
	}
	want := []string{"tart prune --entries caches --older-than 2 --space-budget 10"}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q — Reclaim must prune through the store's own runner", r.calls, want)
	}
}

func TestPruneTartCache_DisabledWhenNoCriteria(t *testing.T) {
	r := &scriptedRunner{}
	if err := pruneTartCache(context.Background(), r, "/some/home", 0, 0, []string{"ghcr.io/cirruslabs/macos-golden-gate-xcode:27"}, discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected 0 calls when no criteria set, got %v — nothing is pruned, so nothing needs marking", r.calls)
	}
}

func TestPruneTartCache_MarksKeptImageUsedBeforePruning(t *testing.T) {
	r := &scriptedRunner{results: map[string]scriptedResult{
		"tart list --format json --source oci": {out: ociCacheListing},
	}}
	keep := []string{"ghcr.io/cirruslabs/macos-golden-gate-xcode:27"}
	if err := pruneTartCache(context.Background(), r, "/some/home", 7*24*time.Hour, 150, keep, discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	clone, del, prune := -1, -1, -1
	var throwaway string
	for i, call := range r.calls {
		fields := strings.Fields(call)
		switch {
		case strings.HasPrefix(call, "tart clone "):
			clone, throwaway = i, fields[3]
			if fields[2] != keep[0] {
				t.Errorf("cloned %q, want the kept image %q", fields[2], keep[0])
			}
		case strings.HasPrefix(call, "tart delete "):
			del = i
			if fields[2] != throwaway {
				t.Errorf("deleted %q, want the throwaway clone %q", fields[2], throwaway)
			}
		case strings.HasPrefix(call, "tart prune "):
			prune = i
		}
	}
	if clone < 0 || del < 0 || prune < 0 {
		t.Fatalf("want clone, delete and prune; calls: %v", r.calls)
	}
	if clone >= del || del >= prune {
		t.Errorf("order clone=%d delete=%d prune=%d, want the image marked used before pruning", clone, del, prune)
	}
	if !provider.OwnsTartVM(provider.TartVM{Name: throwaway, Source: "local"}) {
		t.Errorf("throwaway VM %q is not one startup reconciliation would reclaim", throwaway)
	}
}

func TestPruneTartCache_DoesNotCloneImageMissingFromCache(t *testing.T) {
	r := &scriptedRunner{results: map[string]scriptedResult{
		"tart list --format json --source oci": {out: ociCacheListing},
	}}
	// Cloning an image that is not cached would pull it — 80+ GB.
	if err := pruneTartCache(context.Background(), r, "/some/home", 7*24*time.Hour, 0, []string{"ghcr.io/cirruslabs/macos-tahoe-xcode:26.5"}, discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := r.count("tart clone"); n != 0 {
		t.Errorf("tart clone called %d times for an image not in the cache, want 0", n)
	}
	if n := r.count("tart prune"); n != 1 {
		t.Errorf("tart prune called %d times, want 1", n)
	}
}

func TestPruneTartCache_PrunesEvenWhenMarkingFails(t *testing.T) {
	r := &scriptedRunner{results: map[string]scriptedResult{
		"tart list --format json --source oci": {out: ociCacheListing},
		"tart clone":                           {err: errors.New("no space left on device")},
	}}
	if err := pruneTartCache(context.Background(), r, "/some/home", 7*24*time.Hour, 0, []string{"ghcr.io/cirruslabs/macos-golden-gate-xcode:27"}, discard); err != nil {
		t.Fatalf("unexpected error: %v; marking is best-effort", err)
	}
	if n := r.count("tart prune"); n != 1 {
		t.Errorf("tart prune called %d times after a failed mark, want 1 — cleanup must keep working", n)
	}
}

func TestPruneTartCache_Arguments(t *testing.T) {
	for _, tt := range []struct {
		name   string
		maxAge time.Duration
		budget int
		want   string
	}{
		{"age only", 7 * 24 * time.Hour, 0, "tart prune --entries caches --older-than 7"},
		{"age and budget", 7 * 24 * time.Hour, 50, "tart prune --entries caches --older-than 7 --space-budget 50"},
		// Sub-day windows floor to 1 day; --older-than=0 would wipe the whole cache.
		{"sub-day age floors to one day", 12 * time.Hour, 0, "tart prune --entries caches --older-than 1"},
		{"budget only", 0, 50, "tart prune --entries caches --space-budget 50"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &scriptedRunner{}
			if err := pruneTartCache(context.Background(), r, "/some/home", tt.maxAge, tt.budget, nil, discard); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(r.calls) != 1 || r.calls[0] != tt.want {
				t.Errorf("calls = %q, want [%q]", r.calls, tt.want)
			}
		})
	}
}

func TestPruneTartCache_PropagatesError(t *testing.T) {
	r := &scriptedRunner{results: map[string]scriptedResult{"tart prune": {err: errors.New("disk full")}}}
	err := pruneTartCache(context.Background(), r, "/some/home", 7*24*time.Hour, 0, nil, discard)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %v, want the prune failure wrapped", err)
	}
}
