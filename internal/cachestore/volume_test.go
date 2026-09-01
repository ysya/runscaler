package cachestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- sharedVolumeStore ---

// TestSharedVolumeStore_IsScratchAndOnlyTier3 pins the architecture's
// central invariant: the shared volume holds live inter-job handoff data,
// so it must be KindScratch and Tier4 (wholesale removal) must be a strict
// no-op that never touches the volume.
func TestSharedVolumeStore_IsScratchAndOnlyTier3(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewSharedVolumeStore(fake, SharedVolumeConfig{
		VolumeName: "runner-shared", MountPath: "/shared",
		HelperImage: "img", MaxAge: 72 * time.Hour,
	})

	if s.Kind() != KindScratch {
		t.Fatalf("Kind() = %v, want KindScratch — it holds live handoff data", s.Kind())
	}
	// Tier4 would remove the whole volume, destroying an in-flight run.
	freed, err := s.Reclaim(context.Background(), Tier4)
	if err != nil {
		t.Fatalf("Reclaim(Tier4) error: %v", err)
	}
	if freed != 0 || fake.volumesRemoved != nil {
		t.Error("shared volume must never be removed wholesale")
	}
}

// TestSharedVolumeStore_NeverReclaimsWholesaleAtAnyTier makes the pinned
// invariant above stricter still: every tier except Tier3 must be a
// complete no-op — no helper container, no freed bytes, no VolumeRemove —
// not just "the volume survives Tier4 specifically".
func TestSharedVolumeStore_NeverReclaimsWholesaleAtAnyTier(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewSharedVolumeStore(fake, SharedVolumeConfig{
		VolumeName: "runner-shared", MountPath: "/shared",
		HelperImage: "img", MaxAge: 72 * time.Hour,
	})

	for _, tier := range []Tier{Tier1, Tier2, Tier4} {
		freed, err := s.Reclaim(context.Background(), tier)
		if err != nil || freed != 0 {
			t.Errorf("Reclaim(%v) should be a no-op, got freed=%d err=%v", tier, freed, err)
		}
	}
	if fake.createdContainers != 0 {
		t.Errorf("no tier but Tier3 should ever run a helper container, got %d", fake.createdContainers)
	}
	if fake.volumesRemoved != nil {
		t.Error("shared volume must never be removed wholesale, at any tier")
	}
}

func TestSharedVolumeStore_NameKindPath(t *testing.T) {
	fake := &fakeDockerAPI{}
	s := NewSharedVolumeStore(fake, SharedVolumeConfig{
		VolumeName: "runner-shared", MountPath: "/shared",
		HelperImage: "img", RootDir: "/mnt/docker-data", MaxAge: time.Hour,
	})

	if want := "shared-volume:runner-shared"; s.Name() != want {
		t.Errorf("Name() = %q, want %q", s.Name(), want)
	}
	if got := s.Path(); got != "/mnt/docker-data" {
		t.Errorf("Path() = %q, want cfg.RootDir %q", got, "/mnt/docker-data")
	}
}

func TestSharedVolumeStore_DisabledReclaimsNothing(t *testing.T) {
	tests := []struct {
		name string
		cfg  SharedVolumeConfig
	}{
		{"zero MaxAge", SharedVolumeConfig{VolumeName: "v", MountPath: "/shared", HelperImage: "img"}},
		{"empty MountPath", SharedVolumeConfig{VolumeName: "v", HelperImage: "img", MaxAge: time.Hour}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeDockerAPI{}
			s := NewSharedVolumeStore(fake, tt.cfg)
			if s.Enabled() {
				t.Fatalf("Enabled() = true, want false for %+v", tt.cfg)
			}
			freed, err := s.Reclaim(context.Background(), Tier3)
			if err != nil {
				t.Fatalf("Reclaim(Tier3) error: %v", err)
			}
			if freed != 0 || fake.createdContainers != 0 {
				t.Errorf("a disabled store must not reclaim, got freed=%d containers=%d", freed, fake.createdContainers)
			}
		})
	}
}

// TestSharedVolumeStore_Tier3RunsTwoPhaseDeleteAndReportsFreedBytes checks
// the full happy path: one helper container, the same two-phase delete
// script as provider.CleanupSharedVolumeStale (mtime days rounded, mount
// path shell-quoted), and freed bytes computed from the before/after `du`
// lines the fake plays back.
func TestSharedVolumeStore_Tier3RunsTwoPhaseDeleteAndReportsFreedBytes(t *testing.T) {
	fake := &fakeDockerAPI{containerLogsStdout: "5000000\t/shared data\n1200000\t/shared data\n"}
	s := NewSharedVolumeStore(fake, SharedVolumeConfig{
		VolumeName: "runner-shared", MountPath: "/shared data",
		HelperImage: "img", MaxAge: 72 * time.Hour, // -> 3 days
	})

	freed, err := s.Reclaim(context.Background(), Tier3)
	if err != nil {
		t.Fatalf("Reclaim(Tier3) error: %v", err)
	}
	if freed != 3_800_000 {
		t.Errorf("freed = %d, want %d (5000000 - 1200000)", freed, 3_800_000)
	}
	if fake.createdContainers != 1 {
		t.Fatalf("expected 1 helper container, got %d", fake.createdContainers)
	}

	script := fake.helperScripts[0]
	quoted := shellQuote("/shared data")
	if !strings.Contains(script, "-mtime +3") {
		t.Errorf("script should use -mtime +3, got: %q", script)
	}
	if !strings.Contains(script, quoted) {
		t.Errorf("script should shell-quote the mount path (%q), got: %q", quoted, script)
	}
	if !strings.Contains(script, "-type f -o -type l") {
		t.Errorf("script should delete files and symlinks, got: %q", script)
	}
	if !strings.Contains(script, "-type d -empty -delete") {
		t.Errorf("script should also prune emptied directories, got: %q", script)
	}
	if got := strings.Count(script, "du -sb"); got != 2 {
		t.Errorf("script should measure before and after the delete (2x du -sb), got %d", got)
	}

	// The volume mount itself must be correct — Source is the named
	// volume, Target is the container mount path.
	call := fake.createCalls[0]
	if call.HostConfig == nil || len(call.HostConfig.Mounts) != 1 {
		t.Fatalf("expected exactly one mount, got %+v", call.HostConfig)
	}
	m := call.HostConfig.Mounts[0]
	if m.Source != "runner-shared" || m.Target != "/shared data" {
		t.Errorf("mount = %+v, want Source=runner-shared Target=/shared data", m)
	}
}

func TestSharedVolumeStore_Tier3RoundsSubDayTTLUp(t *testing.T) {
	fake := &fakeDockerAPI{containerLogsStdout: "0\t/shared\n0\t/shared\n"}
	s := NewSharedVolumeStore(fake, SharedVolumeConfig{
		VolumeName: "runner-shared", MountPath: "/shared",
		HelperImage: "img", MaxAge: 6 * time.Hour,
	})

	if _, err := s.Reclaim(context.Background(), Tier3); err != nil {
		t.Fatalf("Reclaim(Tier3) error: %v", err)
	}
	if !strings.Contains(fake.helperScripts[0], "-mtime +1") {
		t.Errorf("sub-day TTL should round up to -mtime +1, got: %q", fake.helperScripts[0])
	}
}

func TestSharedVolumeStore_Measure(t *testing.T) {
	fake := &fakeDockerAPI{containerLogsStdout: "7340032\t/shared\n"}
	s := NewSharedVolumeStore(fake, SharedVolumeConfig{
		VolumeName: "runner-shared", MountPath: "/shared", HelperImage: "img",
	})

	got, err := s.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure() error: %v", err)
	}
	if got != 7_340_032 {
		t.Errorf("Measure() = %d, want %d", got, 7_340_032)
	}
	if fake.helperScripts[0] != "du -sb '/shared'" {
		t.Errorf("Measure script = %q, want %q", fake.helperScripts[0], "du -sb '/shared'")
	}
}

func TestSharedVolumeStore_Measure_PropagatesParseError(t *testing.T) {
	fake := &fakeDockerAPI{containerLogsStdout: "not-a-number\t/shared\n"}
	s := NewSharedVolumeStore(fake, SharedVolumeConfig{
		VolumeName: "runner-shared", MountPath: "/shared", HelperImage: "img",
	})

	if _, err := s.Measure(context.Background()); err == nil {
		t.Fatal("Measure() should error when du output does not parse")
	}
}

// --- cacheVolumeStore ---

// TestCacheVolumeStore_Tier4WipesOnlyWhenAllowed pins that a cache volume
// (no safe age-based eviction) only reclaims at the guard's emergency tier.
func TestCacheVolumeStore_Tier4WipesOnlyWhenAllowed(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewCacheVolumeStore(fake, CacheVolumeConfig{
		VolumeName: "ccache", MountPath: "/home/runner/.ccache", HelperImage: "img",
	})

	if _, err := s.Reclaim(context.Background(), Tier2); err != nil {
		t.Fatalf("Reclaim(Tier2) error: %v", err)
	}
	if fake.createdContainers != 0 {
		t.Error("cache volume has no age-based reclaim; Tier2 must be a no-op")
	}
	if _, err := s.Reclaim(context.Background(), Tier4); err != nil {
		t.Fatalf("Reclaim(Tier4) error: %v", err)
	}
	if fake.createdContainers != 1 {
		t.Error("Tier4 should run one helper container to empty the volume")
	}
}

func TestCacheVolumeStore_NameKindPath(t *testing.T) {
	fake := &fakeDockerAPI{}
	s := NewCacheVolumeStore(fake, CacheVolumeConfig{
		VolumeName: "ccache", MountPath: "/home/runner/.ccache",
		HelperImage: "img", RootDir: "/mnt/docker-data",
	})

	if s.Kind() != KindCache {
		t.Errorf("Kind() = %v, want KindCache", s.Kind())
	}
	if want := "cache-volume:ccache"; s.Name() != want {
		t.Errorf("Name() = %q, want %q", s.Name(), want)
	}
	if got := s.Path(); got != "/mnt/docker-data" {
		t.Errorf("Path() = %q, want cfg.RootDir %q", got, "/mnt/docker-data")
	}
}

func TestCacheVolumeStore_DisabledReclaimsNothing(t *testing.T) {
	fake := &fakeDockerAPI{}
	s := NewCacheVolumeStore(fake, CacheVolumeConfig{VolumeName: "ccache", HelperImage: "img"}) // MountPath unset

	if s.Enabled() {
		t.Fatal("Enabled() = true, want false when MountPath is unset")
	}
	freed, err := s.Reclaim(context.Background(), Tier4)
	if err != nil {
		t.Fatalf("Reclaim(Tier4) error: %v", err)
	}
	if freed != 0 || fake.createdContainers != 0 {
		t.Errorf("a disabled store must not reclaim, got freed=%d containers=%d", freed, fake.createdContainers)
	}
}

// TestCacheVolumeStore_Tier4DeletesEverythingAndReportsFreedBytes checks the
// full wipe's script (no -mtime filter, unlike the shared volume) and its
// freed-bytes accounting.
func TestCacheVolumeStore_Tier4DeletesEverythingAndReportsFreedBytes(t *testing.T) {
	fake := &fakeDockerAPI{containerLogsStdout: "900000\t/home/runner/.ccache\n0\t/home/runner/.ccache\n"}
	s := NewCacheVolumeStore(fake, CacheVolumeConfig{
		VolumeName: "ccache", MountPath: "/home/runner/.ccache", HelperImage: "img",
	})

	freed, err := s.Reclaim(context.Background(), Tier4)
	if err != nil {
		t.Fatalf("Reclaim(Tier4) error: %v", err)
	}
	if freed != 900_000 {
		t.Errorf("freed = %d, want %d", freed, 900_000)
	}

	script := fake.helperScripts[0]
	if strings.Contains(script, "-mtime") {
		t.Errorf("cache volume wipe must not filter by age, got: %q", script)
	}
	if !strings.Contains(script, "-mindepth 1 -delete") {
		t.Errorf("script should delete everything under the mount, got: %q", script)
	}

	call := fake.createCalls[0]
	m := call.HostConfig.Mounts[0]
	if m.Source != "ccache" || m.Target != "/home/runner/.ccache" {
		t.Errorf("mount = %+v, want Source=ccache Target=/home/runner/.ccache", m)
	}
}

func TestCacheVolumeStore_Tier4NeverRemovesVolumeWholesale(t *testing.T) {
	fake := &fakeDockerAPI{}
	s := NewCacheVolumeStore(fake, CacheVolumeConfig{
		VolumeName: "ccache", MountPath: "/home/runner/.ccache", HelperImage: "img",
	})

	if _, err := s.Reclaim(context.Background(), Tier4); err != nil {
		t.Fatalf("Reclaim(Tier4) error: %v", err)
	}
	if fake.volumesRemoved != nil {
		t.Error("Tier4 empties the volume's contents; it must never call VolumeRemove")
	}
}

func TestCacheVolumeStore_Measure(t *testing.T) {
	fake := &fakeDockerAPI{containerLogsStdout: "2048\t/home/runner/.ccache\n"}
	s := NewCacheVolumeStore(fake, CacheVolumeConfig{
		VolumeName: "ccache", MountPath: "/home/runner/.ccache", HelperImage: "img",
	})

	got, err := s.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure() error: %v", err)
	}
	if got != 2048 {
		t.Errorf("Measure() = %d, want 2048", got)
	}
}

// TestReclaimGuardsAgainstFreedUnderflow pins that a "usage went up between
// the before/after du calls" reading (e.g. a concurrent writer) reports 0
// rather than underflowing the unsigned subtraction into a huge number.
func TestReclaimGuardsAgainstFreedUnderflow(t *testing.T) {
	fake := &fakeDockerAPI{containerLogsStdout: "100\t/home/runner/.ccache\n500\t/home/runner/.ccache\n"}
	s := NewCacheVolumeStore(fake, CacheVolumeConfig{
		VolumeName: "ccache", MountPath: "/home/runner/.ccache", HelperImage: "img",
	})

	freed, err := s.Reclaim(context.Background(), Tier4)
	if err != nil {
		t.Fatalf("Reclaim(Tier4) error: %v", err)
	}
	if freed != 0 {
		t.Errorf("freed = %d, want 0 (after > before must not underflow)", freed)
	}
}

// TestReclaimDeletesEvenWhenDuFails executes the script production actually
// generates, through a real `sh`, against a real directory, with a `du` stub
// that fails the way du does when a file vanishes mid-walk (exit 1, nothing
// on stdout) — routine on a volume jobs are actively writing to, which is
// exactly what the shared volume is for. The script used to begin with
// `set -e`, so that transient measurement failure aborted it *before* the
// delete ran: the sweep reclaimed nothing and returned an error.
//
// The witness at the end is not redundant. Without it, the assertion above
// would also pass against the old, broken shape on any host whose real du
// happened to succeed — a silently vacuous test.
func TestReclaimDeletesEvenWhenDuFails(t *testing.T) {
	// Recover the exact script production builds, then run it for real.
	fake := &fakeDockerAPI{}
	mount := t.TempDir()
	s := NewCacheVolumeStore(fake, CacheVolumeConfig{
		VolumeName: "ccache", MountPath: mount, HelperImage: "img",
	})
	freed, err := s.Reclaim(context.Background(), Tier4)
	if err != nil {
		t.Fatalf("Reclaim(Tier4) error: %v — an unreadable du must not fail the reclaim", err)
	}
	if freed != 0 {
		t.Errorf("freed = %d, want 0 (unparseable du output is 'freed unknown', not an error)", freed)
	}
	if len(fake.helperScripts) != 1 {
		t.Fatalf("expected 1 helper script, got %d", len(fake.helperScripts))
	}
	script := fake.helperScripts[0]

	// A du that exits non-zero with no output, placed first on PATH.
	stubBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubBin, "du"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write du stub: %v", err)
	}
	env := append(os.Environ(), "PATH="+stubBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	runScript := func(sh string) {
		cmd := exec.Command("/bin/sh", "-c", sh)
		cmd.Env = env
		// The exit status is deliberately ignored here: the script tolerates
		// every failure it can hit, so only its effect on disk matters.
		_ = cmd.Run()
	}

	if err := os.WriteFile(filepath.Join(mount, "cached.o"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runScript(script)
	entries, err := os.ReadDir(mount)
	if err != nil {
		t.Fatalf("read mount: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("mount still holds %d entry/entries after the reclaim script — a failing du "+
			"must not stop the delete from running; script was: %q", len(entries), script)
	}

	// Witness: under the pre-fix `set -e; <bare du>; …` shape, this same du
	// stub must stop whatever follows from running at all.
	marker := filepath.Join(mount, "delete-would-have-run")
	runScript(fmt.Sprintf("set -e; %s; touch %s", duCommand(mount), shellQuote(marker)))
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("witness failed: `set -e` did not abort after the du stub exited non-zero, "+
			"so the assertion above cannot distinguish the fixed script from the broken one "+
			"(marker %q exists)", marker)
	}
}

// --- runVolumeHelper ---

func TestRunVolumeHelper_CreatesStartsRemovesAndReturnsStdout(t *testing.T) {
	fake := &fakeDockerAPI{containerLogsStdout: "hello\n"}

	stdout, err := runVolumeHelper(context.Background(), fake, "img", "myvol", "/mnt", "echo hello")
	if err != nil {
		t.Fatalf("runVolumeHelper() error: %v", err)
	}
	if stdout != "hello\n" {
		t.Errorf("stdout = %q, want %q", stdout, "hello\n")
	}
	if fake.createdContainers != 1 {
		t.Fatalf("expected 1 created container, got %d", fake.createdContainers)
	}
	if len(fake.containersRemoved) != 1 {
		t.Errorf("expected the helper container to be removed, got %d removes", len(fake.containersRemoved))
	}

	call := fake.createCalls[0]
	if call.Config.Image != "img" {
		t.Errorf("image = %q, want img", call.Config.Image)
	}
	if call.Config.User != "root" {
		t.Errorf("user = %q, want root", call.Config.User)
	}
	if call.Config.Labels["managed-by"] != "runner" {
		t.Errorf("missing managed-by label, got: %v", call.Config.Labels)
	}
}

func TestRunVolumeHelper_PropagatesNonZeroExit(t *testing.T) {
	fake := &fakeDockerAPI{waitStatus: 2}

	_, err := runVolumeHelper(context.Background(), fake, "img", "myvol", "/mnt", "exit 2")
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "status 2") {
		t.Errorf("error should mention status 2, got: %v", err)
	}
	// Container must still be cleaned up on failure.
	if len(fake.containersRemoved) != 1 {
		t.Errorf("expected container removed after failure, got %d removes", len(fake.containersRemoved))
	}
}

func TestRunVolumeHelper_PropagatesWaitError(t *testing.T) {
	fake := &fakeDockerAPI{waitErr: errors.New("docker died")}

	_, err := runVolumeHelper(context.Background(), fake, "img", "myvol", "/mnt", "true")
	if err == nil || !strings.Contains(err.Error(), "docker died") {
		t.Fatalf("expected wait error to propagate, got: %v", err)
	}
	if len(fake.containersRemoved) != 1 {
		t.Errorf("expected container removed after failure, got %d removes", len(fake.containersRemoved))
	}
}

// --- parseDuSizes ---

func TestParseDuSizes(t *testing.T) {
	tests := []struct {
		name    string
		stdout  string
		want    int
		wantErr bool
		sizes   []uint64
	}{
		{"single line", "12345\t/shared\n", 1, false, []uint64{12345}},
		{"two lines", "5000\t/x\n1000\t/x\n", 2, false, []uint64{5000, 1000}},
		{"ignores blank lines", "\n\n12345\t/shared\n\n", 1, false, []uint64{12345}},
		{"too few lines", "12345\t/shared\n", 2, true, nil},
		{"no usable lines", "", 1, true, nil},
		{"non-numeric field", "oops\t/shared\n", 1, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDuSizes(tt.stdout, tt.want)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDuSizes(%q, %d) = %v, want error", tt.stdout, tt.want, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDuSizes(%q, %d) error: %v", tt.stdout, tt.want, err)
			}
			if len(got) != len(tt.sizes) {
				t.Fatalf("parseDuSizes(%q, %d) = %v, want %v", tt.stdout, tt.want, got, tt.sizes)
			}
			for i := range got {
				if got[i] != tt.sizes[i] {
					t.Errorf("parseDuSizes(%q, %d)[%d] = %d, want %d", tt.stdout, tt.want, i, got[i], tt.sizes[i])
				}
			}
		})
	}
}
