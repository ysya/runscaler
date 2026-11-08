# Caching

[English](caching.md) | [繁體中文](caching.zh-TW.md)

Ephemeral runners start cold — nothing survives between jobs unless you opt in. Three layers work together: a host-wide **disk guard** that reclaims space under real pressure, each store's own **retention settings** for routine cleanup, and `runner cache` for **visibility** into what is using space right now.

## What Can Be Reclaimed

Everything reclaimable on the host falls into one of three kinds, by what happens if it disappears — this is the classification the disk guard's tier ladder is built from:

| Kind | If it disappears | Examples |
| --- | --- | --- |
| **Garbage** | Nothing — already unreferenced | Dangling images, stopped containers, orphaned buildx builders, expired shared-volume files |
| **Cache** | The next build/job is a bit slower, nothing breaks | Docker layer/build cache, cache volumes, Tart's OCI/IPSW image cache |
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
| 4 | Cache volumes, wiped entirely | cache |

`max-tier` defaults to **3**: garbage and age-based trims happen automatically, but wiping a cache volume (tier 4) makes the next build cold, so it needs an explicit opt-in.

The guard also enforces any store's own configured budget (see below) on every sweep, independent of whether that store's filesystem is under pressure at all — and checks free space, a single cheap `statfs` rather than a full sweep, immediately before starting each runner, reclaiming synchronously first if that filesystem is already below `min-free`. This is what stops a job from filling the disk mid-build and taking down the whole host; a reclaim failure here is only logged, never fatal — refusing to start jobs is worse than a full disk.

## Store Retention Settings

Each store also keeps its own settings for routine, timer-driven cleanup — this is what already existed before the disk guard did, and still runs on its own schedule regardless of disk pressure.

**Docker layer/build cache (automatic).** With `dind = true` jobs talk to the host daemon directly, so `docker pull`/`docker build` layer caches are shared across all jobs and survive restarts. On a daemon dedicated to runners, the opt-in runtime prune sweep bounds growth — see [Config File (TOML)](configuration.md#config-file-toml) above for `prune`, `prune-ttl`, `build-cache-max-age`, `build-cache-budget`.

> **BuildKit tip:** `docker/setup-buildx-action` creates a throwaway builder per job whose cache dies with the job (`buildx-cleanup` reclaims the leftovers). To actually reuse build cache across jobs, build with the daemon's built-in BuildKit (plain `docker build`, or buildx with `driver: docker`) so the cache lands where `build-cache-*` retention manages it instead.

**Cache volumes (`cache-volumes`, `[[docker.cache]]`).** Named volumes mounted into every runner container at tool-default cache paths, so workflows hit warm caches with zero workflow changes:

```toml
[docker]
cache-volumes = [
  "gradle-cache:/home/runner/.gradle",
  "pnpm-store:/home/runner/.local/share/pnpm/store",
]

# Long form, for an entry that needs a size budget. A name in both forms
# resolves entirely from its [[docker.cache]] entry, not merged field by field.
[[docker.cache]]
name      = "ccache"
path      = "/home/runner/.ccache"
budget    = "20GB"
on-exceed = "warn"   # warn (default): log only, leave eviction to the tool's own LRU | wipe: delete everything over budget
```

The short form is enough for most caches: created on first use, persistent, never swept by any TTL. Same volume name across scale sets shares the cache; different names isolate it. The long form adds an optional `budget`, enforced independently of disk pressure — the guard checks it on every sweep regardless of `max-tier` or whether the filesystem is actually under pressure. There is no partial eviction: `wipe` deletes the volume's entire contents once over budget. Fine-grained LRU is deliberately left to the tool itself (e.g. ccache's own `max_size`) — a content-addressed pnpm store or Gradle's `modules-2` metadata index would both break under a generic file-by-file sweep.

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

## Visibility

`runner cache` measures every configured store and prints its size, the filesystem it lives on, that filesystem's current free percentage, and its retention policy:

```
$ runner cache
STORE               FILESYSTEM  SIZE     FS FREE  POLICY
docker-build-cache  /           12.4 GB  38%
ccache              /           18.1 GB  38%      budget=20.0 GiB on-exceed=warn
runner-shared       /           3.3 GB   38%
tart-cache          /Volumes/…  142 GB   58%
```

Add `--json` for scripting. Measuring can be expensive — some stores walk a volume via a short-lived helper container — so it is never on a hot path. For a cheap view that is safe to poll instead, `/healthz`'s `disk` section reports each filesystem's free percentage from a plain `statfs`, never a real measurement.
