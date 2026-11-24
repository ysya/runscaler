# Caching

[English](caching.md) | [繁體中文](caching.zh-TW.md)

Ephemeral runners start cold — nothing survives between jobs unless you opt in. Three layers work together: a host-wide **disk guard** that reclaims space under real pressure, each store's own **retention settings** for routine cleanup, and `runner cache` for **visibility** into what is using space right now.

## What Can Be Reclaimed

Everything reclaimable on the host falls into one of three kinds, by what happens if it disappears — this is the classification the disk guard's tier ladder is built from:

| Kind | If it disappears | Examples |
| --- | --- | --- |
| **Garbage** | Nothing — already unreferenced | Dangling images, stopped containers, orphaned buildx builders, expired shared-volume files |
| **Cache** | The next build/job is a bit slower, nothing breaks | Docker layer/build cache, Tart's OCI/IPSW image cache |
| **Scratch** | An in-flight workflow run breaks | The shared volume's not-yet-expired contents |

This is what stops a build cache from landing where handoff data belongs: cache is free to reclaim whenever it's in the way; scratch never is, at any tier, regardless of disk pressure. The shared volume is the one store that is both, depending on age — mid-workflow it's scratch (a later job in the same run may still read what an earlier job wrote there), and once a file passes `shared-volume-max-age` it becomes garbage. Because of this, runner no longer deletes the shared volume at process exit: a restart (including a self-update) between two jobs of one workflow run no longer breaks the later job, and reclamation is left entirely to `shared-volume-max-age` and the guard's tier 3.

## The Disk Guard

`[disk]` runs a guard that watches every configured store, grouped by the filesystem it actually lives on, and reclaims space only under real pressure — it is not a sweeper running on a fixed timer regardless of need:

```toml
[disk]
guard       = true   # reclaim under disk pressure; false opts this host's disk out of guard management entirely
min-free    = "10%"  # free-space floor that triggers reclamation: a percentage of the filesystem, or a size like "20GB"
target-free = "20%"  # level reclamation aims to restore before stopping; must be greater than min-free
interval    = "1h"   # how often the periodic check runs
max-tier    = 3      # highest tier the guard may reclaim at (1-4)
```

**Per filesystem, not per host.** A Mac Studio with `TART_HOME` on a dedicated 931 GB volume and Docker on the system disk gets independent checks and independent thresholds for each — one host-wide "disk full" number would be meaningless there.

When a filesystem drops below `min-free`, the guard escalates through tiers in order, re-checking free space after each one, and stops as soon as `target-free` is met:

| Tier | Reclaims | Kind |
| --- | --- | --- |
| 1 | Dangling images, stopped containers, orphaned buildx builders | garbage |
| 2 | Build cache and image layers past their max-age | cache |
| 3 | shared-volume files past their max-age | scratch (expired portion only) |
| 4 | All unused build cache on the Docker daemon | cache |

`max-tier` defaults to **3**. Set it to 4 only if the guard may prune all unused Docker build cache when lower tiers cannot meet the free-space target.

The guard also checks free space before starting each runner. If free space is below `min-free`, it attempts reclamation before launch and logs any reclaim failure.

## Store Retention Settings

Each store also keeps its own settings for routine, timer-driven cleanup — this is what already existed before the disk guard did, and still runs on its own schedule regardless of disk pressure.

**Docker layer/build cache (automatic).** With `dind = true` jobs talk to the host daemon directly, so `docker pull`/`docker build` layer caches are shared across all jobs and survive restarts. On a daemon dedicated to runners, the opt-in runtime prune sweep bounds growth — see [Config File (TOML)](configuration.md#config-file-toml) above for `prune`, `prune-ttl`, `build-cache-max-age`, `build-cache-budget`.

> **BuildKit tip:** `docker/setup-buildx-action` creates a throwaway builder per job whose cache dies with the job (`buildx-cleanup` reclaims the leftovers). To actually reuse build cache across jobs, build with the daemon's built-in BuildKit (plain `docker build`, or buildx with `driver: docker`) so the cache lands where `build-cache-*` retention manages it instead.

**Shared volume (`shared-volume`, `shared-volume-max-age`).** A general-purpose volume mounted at the same path in every runner (exposed as `$SHARED_DIR`) for workflows that pass files between jobs of one run explicitly:

```toml
[docker]
shared-volume                  = "/shared"
shared-volume-max-age          = "168h"  # delete files older than this
shared-volume-cleanup-interval = "6h"
```

Files are deleted only once older than `shared-volume-max-age` — never at process exit, and never while still within that window no matter how much disk pressure there is (the scratch classification above, enforced by the guard's tier 3 too). Set it longer than your longest workflow run.

`shared-volume-max-age` is the volume's *only* retention policy, so leaving it unset means nothing ever reclaims that volume: it isn't deleted at exit, and the guard won't touch un-expired scratch data at any tier. runner still measures it (it appears in `runner cache`) and warns at startup, but a shared volume with no max-age grows without bound.

**Tart OCI/IPSW cache (`cache-cleanup`, `cache-max-age`, `cache-budget`).** Xcode VM images are 50–80 GB each and `:latest` tags accumulate old layers under `$TART_HOME/cache/` — see [Config File (TOML)](configuration.md#config-file-toml) above for the full `[tart]` example. The sweeper only touches OCI/IPSW caches, never your local VMs.

**Enable switches gate the routine sweep only.** `prune` (which covers both the dangling-image/stopped-container store and the build-cache store), `buildx-cleanup`, and `cache-cleanup` each turn that store's own *periodic, timer-driven* cleanup on or off — they do not exempt the store from the disk guard. `prune` and `buildx-cleanup` default off because a shared daemon may run workloads runner doesn't own, but the guard reclaims through their stores under real disk pressure regardless of the switch. It has to: `prune` and `buildx-cleanup` both default off, and `max-tier` defaults to 3 — a guard that also honored these switches would, on a Docker-only host, ship enabled by default yet unable to reclaim anything at all until an operator explicitly opted in to routine cleanup too. If a disk truly isn't runner's to manage — a daemon shared with unrelated workloads, say — the correct switch is `[disk] guard = false`, which opts that host's disk out of guard management entirely. Per-store switches were never designed to mean that.

## Migration to Actions cache

The release after v0.24.0 rejects `cache-volumes` and `cache` in both `[docker]` and `[scaleset.docker]`. This includes `[[docker.cache]]`, `[[scaleset.docker.cache]]` and empty overrides (`cache-volumes = []`, `cache = []`). Migrate while the previous version is still available.

1. Back up the current binary, config and workflow revision. Record each named volume and the tool path it supplies. Keep those Docker volumes for rollback. Runscaler does not convert or delete their contents.
2. Choose a destination for each workflow. For the [native receiver](configuration.md#native-go-receiver-opt-in), enable `[actions-cache]`, set `native-actions-cache = true` on a canary Docker scale set and omit `actions-cache-url`. A URL matching the enabled receiver's `advertise-url` fails validation even when native caching is enabled. For an external receiver, set its distinct URL and disable native caching on the canary. Use a [cache-capable runner image](local-actions-cache.md) for either local receiver.
3. Replace each tool mount with an explicit `actions/cache` step or a supported setup action. Choose paths and keys for the tool and its dependencies. Buildx can import and export layers with `type=gha,version=2`. The native receiver applies `max-size-bytes` and `max-idle-days` across entries. There is no per-volume equivalent to `budget` or `on-exceed`. [Dependency caching](https://docs.github.com/en/actions/concepts/workflows-and-actions/dependency-caching) · [Buildx GHA backend](https://docs.docker.com/build/cache/backends/gha/)
4. Run the workflow twice on separate fresh canary runners without the old mounts. Use a new key for the first run, confirm a miss and a successful save at the chosen receiver, then rerun with the same key and verify a restore hit. Once the canary passes, remove all old keys from the service config, validate it with the new binary, drain the affected jobs and switch the service.

For rollback, stop dispatch to the affected sets and drain active jobs. Restore the previous workflow revision, binary and config that reference the retained volumes, validate with that binary, then resume the service. Keep the receiver's data separately. Workflows that use `shared-volume` for file handoff can keep using it.

## Visibility

`runner cache` measures every configured store and prints its size, the filesystem it lives on, that filesystem's current free percentage, and its retention policy:

```
$ runner cache
STORE               FILESYSTEM  SIZE     FS FREE  POLICY
docker-build-cache  /           12.4 GB  38%
runner-shared       /           3.3 GB   38%
tart-cache          /Volumes/…  142 GB   58%
```

Add `--json` for scripting. Measuring can be expensive — some stores walk a volume via a short-lived helper container — so it is never on a hot path. For a cheap view that is safe to poll instead, `/healthz`'s `disk` section reports each filesystem's free percentage from a plain `statfs`, never a real measurement.

**Per-job Docker cache.** In `daemon-mode = "dind"`, all inner image layers,
BuildKit state, and daemon-created volumes belong to one runner and are removed
with it. Warm idle runners have independent caches too; they are never reused
for a later job. Outer-daemon prune settings and disk accounting still operate
on the outer daemon; there is no inner-daemon prune loop or shared Docker cache.
The optional `shared-volume` persists across jobs and is mounted at the same
path in the runner and sidecar for nested bind mounts.
