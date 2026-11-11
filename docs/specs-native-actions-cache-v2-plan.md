# Native Actions Cache v2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in, host-local GitHub Actions cache-v2 receiver to the Runscaler Go manager, with a reversible migration from the current Rust service.

**Architecture:** A separate private HTTP listener in the manager serves cache RPC and local signed payload URLs, and forwards non-cache Results calls to a fixed GitHub upstream. SQLite plus filesystem objects provide durable, atomic, immutable cache entries; the receiver survives until scale-set drain finishes.

**Tech Stack:** Go 1.27.4, `net/http`, `database/sql` with one justified pure-Go SQLite driver, generated protobuf messages plus `google.golang.org/protobuf`, existing `github.com/golang-jwt/jwt/v5`, filesystem storage.

**Spec:** `docs/specs-native-actions-cache-v2-design.md`. Implementation is in an isolated feature worktree; host cutover remains a separate approval gate.

## Global Constraints

- Do not touch the current Rust service/volume, active manager, or repository CI during implementation.
- Preserve `docker.actions-cache-url` and the verified fork boundary; the stock runner is not an accepted custom-endpoint client.
- No cache payload request may fall through to GitHub. Artifacts intentionally remain on GitHub through Results passthrough.
- Linux amd64 is the first end-to-end acceptance target. No arm64 claim without its own gate.
- Config is opt-in, global, and defaults to disabled; use a fresh native data directory.
- Cache secrets, JWTs, signed URLs, private IDs, payloads, and acceptance logs stay out of tracked files.
- Every implementation commit uses conventional-commit format; obtain architectural approval before writing runtime code.

## Review Focus

- A cancelled block or process crash must leave no exact/prefix-visible partial entry: Stage 2 tests both.
- A forged RPC metadata scope or wrong repository must never expand access: Stage 1 tests both.
- Concurrent same-key saves must yield one immutable winner and readable complete bytes: Stage 2 tests both.
- A live range download must not be truncated by eviction: Stage 2 tests this.
- An unknown cache RPC or legacy v1 path must never be proxied to GitHub: Stage 3 tests this.

## Proposed file map

- `internal/config/actions_cache.go`, `internal/config/config.go`, `internal/config/load.go`: global opt-in settings and validation.
- `internal/actionscache/proto/`: the three verified CacheService messages and metadata/scope definitions, with generated Go types.
- `internal/actionscache/auth.go`: trusted JWKS fetch/cache, JWT verification, scope authorization.
- `internal/actionscache/store.go`: SQLite schema, reservations, ordered part manifests, lookup order, publication and eviction bookkeeping.
- `internal/actionscache/blob.go`: streamed single-shot/block/block-list upload, signed capability, range-spanning-part download.
- `internal/actionscache/http.go`: Twirp JSON/protobuf dispatch and fixed Results passthrough.
- `internal/actionscache/service.go`: listener, readiness, sweeper, drain/shutdown, cheap stats.
- `cmd/runner/main.go`, `internal/health/health.go`, `cmd/runner/cmd_cache.go`: manager lifetime and operator status.
- `docs/configuration.md`, `docs/local-actions-cache.md`, `docs/local-actions-cache.zh-TW.md`, `docs/deployment.md`: opt-in operations, semantics, backup, rollback and client boundary.

## Proposed Go interfaces

- `func (c ActionsCacheConfig) Validate() error` in `internal/config/actions_cache.go`; disabled config has no runtime side effects.
- `func New(cfg Config, logger *slog.Logger) (*Service, error)` in `internal/actionscache/service.go`; `Config` holds validated listener, advertise URL, storage, auth and upstream values.
- `func (s *Service) Start() error`, `func (s *Service) Shutdown(ctx context.Context) error`, and `func (s *Service) Stats() Stats`; `Start` binds and bootstraps before returning, `Shutdown` stops after runner drain.
- `func (v *Verifier) Verify(ctx context.Context, rawJWT string) (Principal, error)`; `Principal` has an exact repository identifier and ordered `(scope, permission)` grants.
- `func (s *Store) Reserve(ctx context.Context, p Principal, key, version string) (Reservation, error)`, `Finalize(ctx context.Context, p Principal, key, version string, size int64) (Entry, error)`, and `Lookup(ctx context.Context, p Principal, key string, restoreKeys []string, version string) (Entry, bool, error)`; the HTTP layer maps domain errors to Twirp status codes.
- `func (s *Service) Handler() http.Handler` owns local cache routes and the fixed upstream proxy. Do not expose storage paths or raw JWTs to this interface's callers.

## Stage 1: Protocol and authorization

- [ ] Pin the protocol definition to the accepted `actions/toolkit` generated wire contract; add Go generated messages and JSON/protobuf golden request/response tests, including int64 strings and optional metadata.
- [ ] Add a bounded JWKS client using only configured issuer/JWKS URLs, with algorithm/issuer/optional-exact-audience/time checks, key rotation and network-failure tests; use Rust v10's bounded refresh and cooldown as a reference.
- [ ] Add repository/scope authorization tests for write bit 2, read bit 1, read-only token, metadata forgery, branch isolation, Rust-compatible permission ordering, and exact/prefix lookup.
- [ ] Commit a self-contained protocol/auth increment after `go test ./internal/actionscache/...` passes.

## Stage 2: Atomic local payload store

- [ ] Add the native versioned SQLite schema and private filesystem layout with tests for startup, recovery, schema rejection and correct permissions.
- [ ] Implement reservation conflicts and immutable completed-key publication; test two writers and distinct bytes, plus restart persistence.
- [ ] Implement signed local upload/download capabilities, Rust-compatible upload URL and `x-ms-request-id`, single-shot blobs, out-of-order blocks, ordered XML block lists, `GET`/`HEAD` and ranges spanning parts; test tamper/expiry, declared size, cancellation and ENOSPC.
- [ ] Implement staging cleanup, 30-day idle and 50 GiB style byte budget eviction; test a pinned reader, stale reservation, retry after eviction failure, dangling-entry self-healing and no deletion outside the native directory.
- [ ] Commit each independently testable store increment with conventional commits and `go test -race ./internal/actionscache/...`.

## Stage 3: HTTP routing and manager lifecycle

- [ ] Add Twirp handlers for JSON/protobuf and typed errors; test `CacheService` and legacy-v1 requests never reach a fake upstream.
- [ ] Add fixed-origin Results proxy; test artifact request/response streaming, upstream outage, no client-selected upstream, and no credential/capability logging.
- [ ] Wire `[actions-cache]` config, private listener and data path into `runManager`; test disabled behavior, startup failure, health readiness, active transfer across drain, forced cancellation, and shutdown ordering.
- [ ] Add status/cache byte counts and operator docs; run `go test ./...` and the repository's normal lint/build gates before a review commit.

## Stage 4: Client contract and isolated acceptance

- [ ] Decide and implement the separately reviewed fail-closed legacy-v1 fork change or prove an equivalent mechanism; build a new immutable, capability-checked runner image. Do not replace the deployed image.
- [ ] Adapt `scripts/local-cache/cache-protocol-fixture.mjs`/isolated harness to target the Go receiver; pass JSON and protobuf RPC, auth negatives, multi-block and single-shot roundtrip, prefix lookup, cancellation/restart, and controlled Results passthrough.
- [ ] Run actual signed provider-managed cold/warm jobs for direct `actions/cache`, setup-go, setup-node/npm, Gradle basic, Rust GitHub provider, and BuildKit `type=gha,version=2`; require exact restored checksums and local payload origin from runner and sibling BuildKit namespaces. Include artifact upload/download and outage behavior.
- [ ] Repeat a multi-pair fresh-runner cold/warm comparison and manager/service recreation. Verify immutable same-key conflict and BuildKit numbered-index refresh; do not substitute a protocol fixture for signed-job evidence.
- [ ] Review security, recovery and operational logs without copying private IDs/tokens into public docs. Publish a scoped compatibility report only after all gates pass.

## Stage 5: Separately approved host cutover

- [ ] Record current binary/config/unit, Rust volume backup and service stop deadline; verify current jobs are drained before any manager restart.
- [ ] Start native listener on a separate private endpoint and fresh data directory, retain Rust and its volume, and opt in only the selected scale set after host-specific approval.
- [ ] Verify cold and warm real jobs, artifact passthrough, restart persistence, 50 GiB/30-day policy, and alert/health behavior on the fixed Linux host.
- [ ] If a gate fails, drain and restore the prior Rust endpoint and manager configuration; do not clear the cache URL or delete either data store. Retire Rust only by a later explicit decision.

## Acceptance commands and evidence

Use `go test -race ./internal/actionscache/...`, `go test ./...`, `node --test testdata/local-cache-probe/observer.test.cjs testdata/local-cache-probe/proxy.test.mjs`, and `actionlint -config-file testdata/local-cache-gate/actionlint.yaml testdata/local-cache-gate/acceptance.yml` as applicable. The existing `scripts/local-cache/probe.sh` currently assumes a containerized Rust server, so adapt an isolated Go receiver harness rather than claiming its unchanged execution validates Go. Run signed-job fixtures only in an explicitly approved isolated/canary environment. The acceptance report must distinguish protocol fixture, real signed jobs, BuildKit, artifact passthrough, restart, outage and architecture support.
