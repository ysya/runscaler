package cachestore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/backend"
)

// volumeHelperTimeout bounds every helper-container round trip
// runVolumeHelper makes (create/start/wait/logs/remove) — mirroring
// backend.CleanupSharedVolumeStale's own bound — so a wedged daemon can
// never hang a Measure or Reclaim call.
const volumeHelperTimeout = 10 * time.Minute

// runVolumeHelper runs a throwaway container with volumeName mounted at
// mountPath, executing script via `sh -c`, and returns its stdout. The
// container is always removed, and the call is bounded by
// volumeHelperTimeout so a wedged daemon cannot hang a sweep.
//
// This is backend.CleanupSharedVolumeStale's own container-orchestration
// shape (create with the volume mounted → defer a forced remove → start →
// wait via a three-way select) copied into this package and generalized:
// script is a parameter instead of a fixed find command, and the
// container's stdout comes back to the caller instead of being discarded —
// Measure needs to read `du -sb` output, and Reclaim needs it to report
// bytes freed. CleanupSharedVolumeStale itself has since been retired
// (Task 7 rewired cmd/runner/main.go's periodic sweep to this store's own
// Reclaim(Tier3) and deleted it, since nothing else called it).
func runVolumeHelper(ctx context.Context, client backend.DockerAPI, image, volumeName, mountPath, script string) (string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, volumeHelperTimeout)
	defer cancel()

	name := fmt.Sprintf("runner-cachestore-%d", time.Now().UnixNano())
	c, err := client.ContainerCreate(timeoutCtx, dockerclient.ContainerCreateOptions{
		Config: &container.Config{
			Image: image,
			User:  "root",
			Cmd:   []string{"sh", "-c", script},
			Labels: map[string]string{
				"managed-by": "runner",
				"purpose":    "cachestore-volume-helper",
			},
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{
				Type:   mount.TypeVolume,
				Source: volumeName,
				Target: mountPath,
			}},
		},
		Name: name,
	})
	if err != nil {
		return "", fmt.Errorf("create volume helper container: %w", err)
	}

	// Always remove the container, even if start/wait/logs fails below.
	defer func() {
		_, _ = client.ContainerRemove(context.WithoutCancel(timeoutCtx), c.ID, dockerclient.ContainerRemoveOptions{Force: true})
	}()

	if _, err := client.ContainerStart(timeoutCtx, c.ID, dockerclient.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start volume helper container: %w", err)
	}

	wait := client.ContainerWait(timeoutCtx, c.ID, dockerclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-wait.Error:
		if err != nil {
			return "", fmt.Errorf("wait volume helper container: %w", err)
		}
	case status := <-wait.Result:
		if status.Error != nil {
			return "", fmt.Errorf("volume helper container error: %s", status.Error.Message)
		}
		if status.StatusCode != 0 {
			return "", fmt.Errorf("volume helper container exited with status %d", status.StatusCode)
		}
	case <-timeoutCtx.Done():
		return "", fmt.Errorf("volume helper timed out: %w", timeoutCtx.Err())
	}

	return readContainerStdout(timeoutCtx, client, c.ID)
}

// readContainerStdout fetches a finished container's stdout. Non-TTY
// containers (this file never sets Tty) always multiplex stdout/stderr on
// the wire regardless of which streams were requested — see ContainerLogs'
// doc comment in github.com/moby/moby/client — so the stream is
// demultiplexed via stdcopy before being handed back as plain text; stderr
// is discarded since no caller here needs it.
func readContainerStdout(ctx context.Context, client backend.DockerAPI, containerID string) (string, error) {
	logs, err := client.ContainerLogs(ctx, containerID, dockerclient.ContainerLogsOptions{ShowStdout: true})
	if err != nil {
		return "", fmt.Errorf("read volume helper logs: %w", err)
	}
	defer func() { _ = logs.Close() }()

	var stdout bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, io.Discard, logs); err != nil {
		return "", fmt.Errorf("demultiplex volume helper logs: %w", err)
	}
	return stdout.String(), nil
}

// shellQuote single-quotes value for safe interpolation into a `sh -c`
// script. Duplicated from internal/backend/docker.go's unexported helper of
// the same name rather than exported across the package boundary for one
// function.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// duCommand measures mountPath's usage in bytes. Both stores' Measure run
// exactly this inside a container with the volume mounted at mountPath, and
// Reclaim runs it before and after a delete to compute bytes freed.
func duCommand(mountPath string) string {
	return "du -sb " + shellQuote(mountPath)
}

// parseDuSizes reads the leading byte-count field from each of the first
// `want` non-blank lines of one or more `du -sb` invocations' combined
// output (the second field, du's target path, is ignored) and requires at
// least that many usable lines to be present.
func parseDuSizes(stdout string, want int) ([]uint64, error) {
	sizes := make([]uint64, 0, want)
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		size, err := strconv.ParseUint(strings.Fields(line)[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse du output line %q: %w", line, err)
		}
		sizes = append(sizes, size)
		if len(sizes) == want {
			return sizes, nil
		}
	}
	return nil, fmt.Errorf("du output had %d usable line(s), want %d: %q", len(sizes), want, stdout)
}

// reclaimAndMeasureFreed runs `du -sb mountPath`, then deleteScript, then
// `du -sb mountPath` again — all inside one helper container — and returns
// the difference. Bundling the before/after measurement into the same
// container as the delete avoids a second round trip through the daemon
// just to find out what the delete freed.
//
// A `du` line that fails to parse (or is missing entirely, e.g. because the
// daemon gave back no output at all) degrades to "freed unknown" (0, nil)
// rather than failing the reclaim: the delete itself already ran and
// already succeeded by the time parsing is attempted, and bytes-freed is a
// reporting nicety layered on top of that, not the action's success
// criterion.
//
// The script deliberately runs without `set -e`, and each `du` carries its
// own `|| true`. `du -sb` exits non-zero when a file vanishes mid-walk,
// which is routine on a volume jobs are actively writing to — precisely
// what the shared volume exists for. Under `set -e` that transient
// measurement failure aborted the script *before deleteScript ran*, so the
// sweep reclaimed nothing and reported an error; the trailing `du` is
// neutralized for the mirror-image reason, since runVolumeHelper treats a
// non-zero container exit status as a failed reclaim. Measurement must
// never decide whether the delete happens, only what it can report about
// it — the same tolerance both `find` invocations already carry.
func reclaimAndMeasureFreed(ctx context.Context, client backend.DockerAPI, image, volumeName, mountPath, deleteScript string) (uint64, error) {
	du := duCommand(mountPath)
	script := fmt.Sprintf("%s || true; %s; %s || true", du, deleteScript, du)
	stdout, err := runVolumeHelper(ctx, client, image, volumeName, mountPath, script)
	if err != nil {
		return 0, err
	}

	sizes, err := parseDuSizes(stdout, 2)
	if err != nil {
		return 0, nil
	}
	before, after := sizes[0], sizes[1]
	if after >= before {
		return 0, nil
	}
	return before - after, nil
}

// --- Shared-volume store: live inter-job handoff data (Tier3 only) ---

// SharedVolumeConfig configures the shared-volume store.
type SharedVolumeConfig struct {
	VolumeName  string
	MountPath   string
	HelperImage string
	RootDir     string
	MaxAge      time.Duration
}

type sharedVolumeStore struct {
	client backend.DockerAPI
	cfg    SharedVolumeConfig
}

// NewSharedVolumeStore reclaims stale files from the shared volume — the
// mount runner uses to hand artifacts from one job to a later job in the
// same workflow run. It is KindScratch: its un-expired contents are live
// data a later job may still read, so it participates only in Tier3 (its
// TTL-expired portion; see TiersFor) and is never removed wholesale.
func NewSharedVolumeStore(client backend.DockerAPI, cfg SharedVolumeConfig) CacheStore {
	return serialize(&sharedVolumeStore{client: client, cfg: cfg})
}

func (s *sharedVolumeStore) Name() string    { return "shared-volume:" + s.cfg.VolumeName }
func (s *sharedVolumeStore) Kind() StoreKind { return KindScratch }
func (s *sharedVolumeStore) Path() string    { return s.cfg.RootDir }

// Enabled mirrors the existing shared-volume-cleanup gate in
// cmd/runner/main.go's startSharedVolumeCleanup: both a configured mount
// path and a positive TTL are required, or there is nothing to reclaim
// against.
func (s *sharedVolumeStore) Enabled() bool {
	return s.cfg.MountPath != "" && s.cfg.MaxAge > 0
}

// Measure runs `du -sb` on the mounted volume and parses the byte count.
func (s *sharedVolumeStore) Measure(ctx context.Context) (uint64, error) {
	stdout, err := runVolumeHelper(ctx, s.client, s.cfg.HelperImage, s.cfg.VolumeName, s.cfg.MountPath, duCommand(s.cfg.MountPath))
	if err != nil {
		return 0, fmt.Errorf("measure shared volume: %w", err)
	}
	sizes, err := parseDuSizes(stdout, 1)
	if err != nil {
		return 0, fmt.Errorf("parse shared volume du output: %w", err)
	}
	return sizes[0], nil
}

// Reclaim acts only at Tier3, deleting shared-volume files (and any
// directories left empty by that delete) whose mtime is older than MaxAge —
// the same two-phase delete backend.CleanupSharedVolumeStale used to run
// before Task 7 retired it and rewired cmd/runner/main.go's periodic sweep
// to call this method directly, so both what used to be two sweeps agree on
// what "stale" means.
//
// Every other tier — Tier4 included — is a strict no-op. TiersFor(KindScratch)
// already keeps the guard from calling Reclaim(Tier4) on this store at all;
// this check is the store's own second line of defense against a future
// caller bypassing TiersFor, per the architecture's central invariant: the
// shared volume's un-expired contents are live handoff data for a
// still-running workflow, and wiping it wholesale breaks that run.
func (s *sharedVolumeStore) Reclaim(ctx context.Context, tier Tier) (uint64, error) {
	if tier != Tier3 || !s.Enabled() {
		return 0, nil
	}

	// find -mtime works in 24h units; round up so sub-day TTLs still sweep,
	// matching backend.CleanupSharedVolumeStale exactly.
	days := int(s.cfg.MaxAge / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	mountPath := shellQuote(s.cfg.MountPath)
	// `|| true` on both finds keeps a mid-walk error (e.g. a file another
	// job removed concurrently) from failing the whole sweep — matching
	// CleanupSharedVolumeStale's own comment on this exact script shape.
	deleteScript := fmt.Sprintf(
		"find %[1]s -mindepth 1 -mtime +%[2]d \\( -type f -o -type l \\) -delete 2>/dev/null || true; "+
			"find %[1]s -mindepth 1 -type d -empty -delete 2>/dev/null || true",
		mountPath, days,
	)

	freed, err := reclaimAndMeasureFreed(ctx, s.client, s.cfg.HelperImage, s.cfg.VolumeName, s.cfg.MountPath, deleteScript)
	if err != nil {
		return 0, fmt.Errorf("reclaim shared volume: %w", err)
	}
	return freed, nil
}

// Budget reports no cap: this store's un-expired contents are live
// handoff data (see NewSharedVolumeStore), not a cache with a retention
// budget — SharedVolumeConfig carries no such field.
func (s *sharedVolumeStore) Budget() (uint64, string) { return 0, "" }

// --- Cache-volume store: persistent tool caches (Tier4 wipe only) ---

// CacheVolumeConfig configures one persistent cache-volume store (e.g.
// ccache, a Gradle cache). One store is constructed per configured cache
// volume mount. BudgetBytes and OnExceed are not consumed by this store —
// they carry the operator's budget policy for a later task's tier-reclaim
// loop to act on.
type CacheVolumeConfig struct {
	VolumeName  string
	MountPath   string
	HelperImage string
	RootDir     string
	BudgetBytes uint64
	OnExceed    string
}

type cacheVolumeStore struct {
	client backend.DockerAPI
	cfg    CacheVolumeConfig
}

// NewCacheVolumeStore reclaims one persistent named cache volume (ccache, a
// package manager cache, etc.). It is KindCache — regenerable, only costing
// time on the next build — so it participates in Tier4 (see TiersFor);
// unlike the daemon's own build cache, it has no age metadata to trim
// safely by, so Tier2 is always a no-op here (see Reclaim).
func NewCacheVolumeStore(client backend.DockerAPI, cfg CacheVolumeConfig) CacheStore {
	return serialize(&cacheVolumeStore{client: client, cfg: cfg})
}

func (s *cacheVolumeStore) Name() string    { return "cache-volume:" + s.cfg.VolumeName }
func (s *cacheVolumeStore) Kind() StoreKind { return KindCache }
func (s *cacheVolumeStore) Path() string    { return s.cfg.RootDir }

// Enabled requires a configured mount path, matching the "empty path means
// unconfigured" convention the shared-volume store's own Enabled follows.
func (s *cacheVolumeStore) Enabled() bool { return s.cfg.MountPath != "" }

// Measure runs `du -sb` on the mounted volume and parses the byte count.
func (s *cacheVolumeStore) Measure(ctx context.Context) (uint64, error) {
	stdout, err := runVolumeHelper(ctx, s.client, s.cfg.HelperImage, s.cfg.VolumeName, s.cfg.MountPath, duCommand(s.cfg.MountPath))
	if err != nil {
		return 0, fmt.Errorf("measure cache volume: %w", err)
	}
	sizes, err := parseDuSizes(stdout, 1)
	if err != nil {
		return 0, fmt.Errorf("parse cache volume du output: %w", err)
	}
	return sizes[0], nil
}

// Reclaim acts only at Tier4, deleting everything under the mount — a full
// wipe, not an age-based trim: a cache volume carries no per-file
// eviction-safe age signal the way the shared volume's mtime does, so
// Tier2 is always a no-op and the guard's emergency tier is the only path
// in. The volume itself is never removed, only its contents — the next
// build finds it again, just empty, so no re-creation is needed.
func (s *cacheVolumeStore) Reclaim(ctx context.Context, tier Tier) (uint64, error) {
	if tier != Tier4 || !s.Enabled() {
		return 0, nil
	}

	deleteScript := fmt.Sprintf("find %s -mindepth 1 -delete 2>/dev/null || true", shellQuote(s.cfg.MountPath))
	freed, err := reclaimAndMeasureFreed(ctx, s.client, s.cfg.HelperImage, s.cfg.VolumeName, s.cfg.MountPath, deleteScript)
	if err != nil {
		return 0, fmt.Errorf("reclaim cache volume: %w", err)
	}
	return freed, nil
}

// Budget reports cfg.BudgetBytes/cfg.OnExceed verbatim — carried unused
// since NewCacheVolumeStore (see CacheVolumeConfig's doc comment) precisely
// so the disk guard's independent budget-enforcement pass could act on
// them once it existed. Unlike the Docker build cache and Tart stores, this
// store has no tool-native "trim to budget" mechanism of its own (Reclaim
// only ever does a full Tier4 wipe), so there is no other consumer of this
// cap to entangle with — the guard's own enforcement is the only one.
func (s *cacheVolumeStore) Budget() (uint64, string) { return s.cfg.BudgetBytes, s.cfg.OnExceed }
