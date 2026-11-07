# Runner Reliability Implementation Plan

Implementation authorization: the user requested the recommended order on
2026-11-07: installation/upgrade integration coverage, Docker readiness with
failure diagnostics, then startup backoff. This authorizes repository changes
and isolated automated fixtures, not replacement/restart of existing services.
Stages C/D (image maintenance and Prometheus) remain outside this increment.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans for inline implementation or superpowers:subagent-driven-development if the user explicitly selects delegation. Steps use checkbox (`- [ ]`) syntax for tracking. The prerequisite and Stages A/B are authorized for implementation. Stages C/D and live service operations remain unapproved. Task-level commit steps are consolidated into one coherent implementation commit.

**Goal:** Make runner readiness, startup recovery, image maintenance, and local operational metrics explicit and diagnosable.

**Architecture:** Extend the existing provider capabilities and controller state machine without replacing the scaleset listener or host capacity broker. Keep maintenance commands separate from the live manager and export bounded in-memory observations through the existing health server.

**Tech Stack:** Go 1.27.3, existing Moby client v0.6.1, actions/scaleset v0.4.0, Cobra/Viper, Tart public CLI, proposed official Prometheus Go client with a private registry.

**Spec:** [Runner reliability design](../specs/2026-11-07-runner-reliability-design.md), including its defaults and safety constraints. The approved implementation scope is recorded above.

## Global Constraints

- Every host keeps one manager and the existing `/tmp/runner.lock`.
- Honor the confirmed deployment model: Linux system installation with a dedicated non-root runtime account, macOS user mode, and optional Linux user installation. Features must work with the target runtime UID, scope-aware authorized paths, and explicit provider endpoints; system binary installation/update remains an administrative operation. Do not equate a non-root Docker client with a rootless daemon.
- Keep listener callbacks free of provider, filesystem, and GitHub I/O.
- Never remove an apparently idle or unready live runner before the existing GitHub deregistration protection confirms it can be removed.
- Provisioning/removing instances retain their lease until actual cleanup; release is idempotent.
- Keep health liveness/readiness semantics and signal/drain behavior intact.
- HTTP health/metrics only reads memory snapshots. The only proposed new direct runtime dependency is `github.com/prometheus/client_golang`, subject to design approval, a verified pinned version, and vulnerability checks.
- Add compatible health JSON fields; preserve explicit-zero config inheritance.
- Update README.md and README.zh-TW.md together; code, CLI text, tests, agent instructions, and commits use English.
- Do not change runtime UID, privilege escalation, service scope selection, startup image pull policy, or updater behavior in these tasks.
- A real-host replacement, restart, or image refresh requires separate authorization. Unit tests and fake providers do not satisfy live acceptance.

## Review Focus

- JobStarted or JobCompleted arrives while readiness is still pending: job evidence wins and the instance is never rolled back to idle.
- GitHub assigned work before a local callback: JobStillRunning preserves the instance and its lease through readiness timeout and drain.
- Docker logs are multiplexed, unreadable, huge, split across frames, or contain credentials: bounded parsing and explicit failure; no default raw-log persistence.
- Repeated demand callbacks and concurrent readiness results must not bypass backoff or erase a later failure.
- Duplicate provider references on distinct sockets/TART_HOME, custom log locations, and malformed telemetry labels must remain distinct, writable where authorized, and correctly escaped.

## Delivery Boundaries

Implement as four reviewable stages: A readiness/diagnostics (Tasks 1–3), B recovery (Task 4), C image maintenance (Tasks 5–6), D metrics (Tasks 7–8). Each stage can ship independently after its own CI and authorized live gate. Do not begin any stage until its design is accepted.

Installation targeting and runtime identity have a separate [design](../specs/2026-11-07-installation-target-design.md) and [implementation plan](2026-11-07-installation-target.md). They can precede these stages, but these tasks must work with existing explicit `--config` and provider locations if they have not shipped. Do not invent an unimplemented dependency on a target resolver.

The confirmed model defaults Linux to system installation with a dedicated non-root runtime account and macOS to user mode; Linux user installation remains selectable. Existing current runner root services remain supported and retain their identity; no root-to-non-root conversion is required or planned. Include system/non-root and both user-service modes in authorized live acceptance, plus separate rootless-Docker compatibility checks before claiming that support.

## File Responsibilities

| Files | Responsibility |
| --- | --- |
| `internal/config/config.go`, `defaults.go`, `load.go` | Proposed Docker readiness, per-scale-set retry, and global diagnostics settings; default/inheritance validation. |
| `internal/provider/provider.go`, `docker.go`, new `docker_readiness.go`, `docker_diagnostics.go` | Optional readiness and diagnostic capabilities, bounded Moby log/inspect adapters. |
| `internal/controller/controller.go`, `state.go`, `runtime.go`, new `readiness.go`, `retry.go` | Ownership of lifecycle transitions, safe readiness teardown, retry eligibility, observation timing. |
| new `internal/diagnostics/store.go` and tests | Private bounded diagnostic files and optional sanitized excerpts. |
| `internal/provider/tart.go`, new `internal/backoff/backoff.go` | Shared delay arithmetic; separate Tart pool retry state and pool observations. |
| new `cmd/runner/cmd_image.go`, `image_docker.go`, `image_tart.go` and tests | Inspect/probe/refresh orchestration using resolved config/provider paths. |
| `internal/metrics/recorder.go`, `runtime.go`, new `observations.go`, `prometheus.go` and tests | Event observations, fixed histograms, fixed-schema text rendering. |
| `internal/health/health.go`, `cmd/runner/main.go`, `cmd_status.go` | Register snapshot producers once, serve `/metrics`, display retry state. |
| `cmd/runner/logpath.go`, `cmd_service.go`, `internal/layout` | Resolve a diagnostic directory within the effective service log directory; preserve systemd write boundaries. |
| `config.example.toml`, `README.md`, `README.zh-TW.md` | New settings, commands, unknown/disabled states, privacy limits, metric definitions, live checklist. |

## Implementation evidence (2026-11-07)

- Prerequisite and Stages A/B implemented in the current worktree; new features
  use optional provider capabilities and existing controller/capacity ownership.
- Full `go test -race ./...`, `go vet ./...`, `govulncheck ./...` (no vulnerabilities), local lint, and Linux tagged fixture
  cross-compilation passed. Linux tagged lint also passed.
- Fresh-context review found five lifecycle/retry/fixture issues. Each was
  corrected; regressions cover generation overlap, job-exit accounting,
  readiness/exit ownership, unknown-ID drain and fixture write permissions.
  Narrow follow-up review confirmed the source fixes.
- Real Linux systemd service execution awaits the isolated CI matrix. This
  macOS environment has no Docker, Tart or systemd executable; it cannot prove
  real provider/job/rootless-daemon or launchd acceptance.
- The integration updater uses offline download substitution while exercising
  real systemd discovery, installed-version checks and exact destination
  selection. It does not prove release-download/network behavior.
- Image maintenance and Prometheus are deferred (Stages C/D).

## Stage A: Readiness and Diagnostics

### Prerequisite: Installation and upgrade integration coverage

- Add static Linux `systemd-analyze verify` tests for generated current units.
- Add opt-in Linux/root integration tests and an isolated GitHub Actions job
  for new system/non-root, existing system/root and current-user services.
- Use a non-GitHub service fixture to exercise real systemd UID/groups/HOME,
  config access, lifecycle signals, service metadata and offline update target
  selection. This does not claim a real GitHub job or rootless daemon test.
- Fail before modifying any pre-existing runner unit/config/state/log path;
  require an explicit fixture opt-in and never run these tests implicitly on
  a developer or production host. Track created fixture paths for cleanup.
- Keep actual Linux fixture execution pending until the workflow is run;
  macOS unit/plist checks are distinct from a logged-in LaunchAgent live gate.

### Task 1: Define settings and provider contracts

**Files:** Modify `internal/config/{config.go,defaults.go,load.go,config_test.go,load_test.go}`, `internal/provider/provider.go`; add focused contract tests as needed.

**Interfaces:**
- Consume: existing `InstanceProvider`, `InstanceWatcher`, map-level inheritance, runtime snapshot.
- Produce: optional `InstanceReadiness` with `WaitReady(context.Context, string) error`; optional `InstanceDiagnostics` with `CollectDiagnostics(context.Context, string) (ExitDiagnostics, error)`.
- `ExitDiagnostics` carries optional exit code/OOM/finish time and a safe category; no raw credentials or provider error strings. Readiness error categories: `readiness_timeout`, `readiness_unavailable`, `runner_exited_before_ready`.
- `ScaleSetConfig.Retry` uses a concrete `RetryConfig`; `Config.Diagnostics` is global-only and must be excluded from inherited scale-set defaults.

- [x] Write config tests asserting readiness defaults true, timeout 5m, explicit false survives inheritance; retry defaults 5s/1m/5 failures/5m; invalid ordering/durations rejected; diagnostics defaults enabled with capture-logs false and remains global-only.
- [x] Run `go test ./internal/config`; verify the new assertions fail for the missing behavior.
- [x] Add the settings/contracts and fixed failure types, without changing readiness execution yet. Use existing pointer/explicit-presence conventions, not special string values.
- [x] Run `go test ./internal/config ./internal/provider`; verify PASS and no existing config regression.
- [x] Include this task in the consolidated implementation commit.

### Task 2: Verify Docker readiness without breaking lifecycle ownership

**Files:** Modify `internal/provider/docker.go`, `internal/controller/{controller.go,state.go,runtime.go}`; create `internal/provider/docker_readiness.go`, `internal/controller/readiness.go` and matching tests; extend existing Docker fakes and deregistration tests.

**Interfaces:**
- Consume: Task 1 optional capability and fixed readiness error categories.
- Produce: `(*DockerProvider).WaitReady(ctx context.Context, instanceID string) error`; `(*instanceState).markProvisioned(name, instanceID string, runnerID int)` attaches IDs without changing busy/removing state; controller readiness completion records one verified success or one safe failure.
- Background readiness workers must be joined after the reconcile worker can no longer create more workers. Cancellation is owned per instance, and JobStarted/Completed can terminate its probe as successful evidence.

- [x] Add `TestDockerReadinessMultiplexedAndSplitMarker`, `TestDockerReadinessRequiresSettle`, `TestDockerReadinessUnreadableLogs`, and `TestDockerReadinessCancellationClosesStream`. Assert a 5-second settle after the marker, a 5-minute configured deadline, bounded buffers, and explicit unavailable status instead of assumed success. Inject test clocks/durations; do not wait five minutes in tests.
- [x] Add controller tests for job-start-before-ID, job-complete-before-ready, completion/exit/readiness racing, late success after removal, drain while waiting, missing runner ID, JobStillRunning and failed deregistration. Assert no removal of assigned jobs, no double release, no busy-to-idle transition, and cleanup of other instances while a readiness probe waits. An unexpectedly exited instance whose removal fails stays tracked and keeps its lease until cleanup succeeds.
- [x] Run `go test ./internal/provider ./internal/controller`; confirm new behavior is missing before implementing it.
- [x] Attach resource/runner IDs before starting an asynchronous readiness wait. Include a protected unready-removal reason in deregistration/restoreBusy eligibility. Preserve the existing watcher error meaning and demandStale barrier. Replace early markDead detach/release with tracked removing and normal cleanup retry; release only after confirmed removal.
- [x] Run `go test -race ./internal/provider ./internal/controller ./internal/health`; require PASS, bounded worker shutdown, and existing drain/capacity tests unchanged in meaning.
- [x] Include this task in the consolidated implementation commit.

### Task 3: Preserve bounded failure diagnostics

**Files:** Create `internal/provider/docker_diagnostics.go`, `internal/diagnostics/{store.go,store_test.go}`; modify DockerAPI and fakes, controller cleanup/readiness paths, `cmd/runner/logpath.go`, service/log-path tests, health/status runtime rendering, and both READMEs.

**Interfaces:**
- Consume: Task 1 diagnostic contract; Task 2 safe failure/exit paths.
- Produce: `(*DockerProvider).CollectDiagnostics(ctx context.Context, instanceID string) (ExitDiagnostics, error)` using Moby ContainerInspect; `diagnostics.NewStore(dir string, captureLogs bool)` and `(*Store).Save(ctx context.Context, record Record) (string, error)`.
- `Record` contains timestamp, scaleset, provider, runner/instance IDs, safe reason, optional exit/OOM/finished metadata, and an optional bounded excerpt. Store failures are logged safely and never change cleanup eligibility.

- [x] Add tests for inspect exit code 137 with OOM true, unknown inspect fields, already-removed containers, inspect/log timeout, and diagnostics failure followed by successful cleanup.
- [x] Add store tests for mode 0700/0600, symlink rejection, atomic writes, 20-file/2-MiB/7-day retention, no deletion outside the owned namespace, and disabled raw-log capture. With opt-in, assert 100-line/64-KiB caps and redaction of configured tokens/JIT/Authorization/PEM fixtures.
- [x] Add path tests for Linux system default/custom LogsDirectory/fallback, Linux user, macOS user, disabled log-file, and diagnostics-disabled operation; verify the store stays inside the directory the service actually grants.
- [x] Run focused tests; implement only failed-start/unexpected-exit collection before removal, with a 5-second diagnostic deadline contained within the current cleanup budget.
- [x] Run `go test -race ./internal/diagnostics ./internal/provider ./internal/controller ./internal/health ./cmd/runner`; verify raw logs never reach health JSON and collect/save failure does not strand capacity.
- [x] Update configuration examples and both READMEs; include them in the consolidated implementation commit.

## Stage B: Recovery

### Task 4: Enforce retry deadlines and warm-pool cooldown

**Files:** Create `internal/backoff/{backoff.go,backoff_test.go}`, `internal/controller/{retry.go,retry_test.go}`; modify controller runtime/reconcile/readiness paths, `internal/provider/tart.go` and pool tests, health/status tests, config example, both READMEs.

**Interfaces:**
- Consume: RetryConfig and verified readiness outcomes from Stage A.
- Produce: pure `backoff.Delay(failures int, initial, maximum, cooldown time.Duration, cooldownAfter int, sample float64) time.Duration`, with sample in [0,1]; a controller retry state holding failure generation, deadline, and one cancellable timer; RuntimeStatus adds `RetryState` and optional `NextRetryAt`.
- Tart pool workers consume the same pure delay function but retain independent counters; listener reconnect and cleanup backoff stay independent.

- [x] Add exact arithmetic tests: samples 0/1 yield 2.5s/5s for failure 1; 5s/10s for failure 2; failure 5 and later yield 5m; huge failure counts cannot overflow; reset after verified success returns the next failure to the first interval.
- [x] Add controller tests for demand callbacks during cooldown, target-satisfied cancellation, drain cancellation, other-scale-set progress, cleanup while delayed, cancellation not counted as failure, immediate unassigned exit, and an older success not clearing a later failure generation. Assert no start occurs before next_retry_at. With min-runners=2 and an empty queue, retry still replenishes two runners; zero queue alone must not clear the deadline.
- [x] Add pool tests for repeated boot failure, cooldown cancellation, successful refill reset, and separate pool/startup failure counters.
- [x] Run focused tests, then implement retry eligibility in reconcile (not merely a delayed signal). Keep at most one live timer and expose fixed state/reason fields.
- [x] Run `go test -race ./internal/backoff ./internal/controller ./internal/provider ./internal/health ./cmd/runner`; verify PASS and no real-time long sleeps.
- [x] Include this task in the consolidated implementation commit.


## Stage C: Image Maintenance

### Task 5: Inspect image evidence without starting resources

**Files:** Create `cmd/runner/{cmd_image.go,cmd_image_test.go,image_docker.go,image_tart.go}` and focused tests; modify registration in main.go if needed; update both READMEs.

**Interfaces:**
- Consume: resolved ScaleSetConfig including Docker.Socket/Platform and Tart.Home/RunnerDir.
- Produce: `inspectImage(ctx context.Context, ss config.ScaleSetConfig) (ImageInspection, error)` behind injected Docker/Tart adapters for tests. `ImageInspection` carries configured ref, provider location, optional local identity, optional Actions runner version and version source, and checked_at.
- Scope: `runner image inspect [--scaleset NAME] [--json]`; no probe or refresh yet. Version fields remain unknown unless backed by a dedicated, verified source.

- [ ] Add tests for all/selected scale sets, missing images, same ref on distinct sockets/homes/platforms, unreadable provider, Docker digests, Tart metadata without digest support, deterministic JSON, unknown version, and truthful Docker startup-pull/Tart if-not-present policy display. Assert zero pull/create/start calls and no authenticated GitHub requests.
- [ ] Run `go test ./cmd/runner`; implement Moby ImageInspect and Tart list adapters using the pinned/current verified APIs. Do not inspect private Tart cache paths or reinterpret a generic OCI version label.
- [ ] If freshness lookup is included, test timeout/unknown, no credential forwarding, and GHES reporting without github.com compatibility claims; otherwise omit freshness from this stage rather than fabricate it.
- [ ] Run `go test -race ./cmd/runner ./internal/provider`; commit: `feat(image): inspect configured runner image metadata`.

### Task 6: Probe baked versions and refresh explicitly

**Files:** Extend image command/adapters/tests; reuse `internal/lock` and existing Docker/Tart command runners; update both READMEs.

**Interfaces:**
- Consume: Task 5 result model and adapters.
- Produce: `probeImage(ctx context.Context, ss config.ScaleSetConfig) (ImageInspection, error)`; `refreshImage(ctx context.Context, ss config.ScaleSetConfig) (ImageInspection, error)`.
- Commands: `runner image inspect --probe --scaleset NAME [--json]`, `runner image refresh --scaleset NAME`. Multi-scale-set refresh/probe requires an explicit selection; a single configured scale set may omit it.

- [ ] Add tests proving machine-lock refusal before any mutation, no health-only fallback, no token/JIT/socket/shared/cache mounts, safe fixed argv for Runner.Listener --version, and a new clone instead of booting a base Tart VM.
- [ ] Add timeout/cancellation tests (Docker 30s, Tart 5m, refresh 30m), independent cleanup deadline 30s, cleanup failure with a precise resource identifier, local Tart base refusal for refresh, remote pull success, and no config/base deletion/service restart.
- [ ] Implement maintenance orchestration with unique owned resource names, confirmed provider socket/home/platform, and exact probe version output. Keep the stopped-manager requirement even if the host appears idle.
- [ ] Run `go test -race ./cmd/runner ./internal/lock ./internal/provider`; commit: `feat(image): add explicit runner image probes and refresh`.
- [ ] Complete authorized live acceptance separately: one Docker image probe, one Tart clone probe, one remote refresh on a stopped manager; verify the next manager start uses the observed image and active workflows were not interrupted.

## Stage D: Operational Metrics

### Task 7: Record well-defined lifecycle observations

**Files:** Create `internal/metrics/{observations.go,observations_test.go}`; modify recorder/runtime/controller state and lifecycle paths, Tart pool selection, main.go wiring, focused tests, and go.mod/go.sum only for the approved Prometheus client.

**Interfaces:**
- Consume: verified readiness, retry state, per-instance lifecycle timestamps, capacity allocator, and pool outcomes.
- Produce: `metrics.NewObserver(scaleset, provider string) *Observer`; methods `ObserveCapacityWait(time.Duration)`, `ObserveStartup(time.Duration)`, `ObserveCleanup(time.Duration)`, and `RecordPoolAcquisition(PoolOutcome)`. The Observer owns official client histograms/counters with the fixed buckets and labels in the spec and implements prometheus.Collector. `(*Recorder).Snapshot()` gains a distinct statistics timestamp. Keep this concrete and local to metrics, not a generic event bus.
- Only a successfully acquired capacity lease produces a capacity-wait observation; only verified success produces startup duration; cleanup emits exactly once after final successful removal, including retry delays.

- [ ] Add deterministic-clock tests for capacity wait 2s, startup 5s including reclaim/JIT, cleanup 12s including retry, and duplicate racing outcomes producing one lifecycle observation. Assert nonnegative durations and correct cumulative buckets.
- [ ] Add pool ready/waited/cold outcome tests and separate pool boot failure counts; ensure all observers are wired once per scale set across listener reconnect.
- [ ] Add tests proving job event timestamps are not mislabeled as exact execution-start latency and statistics freshness only changes on RecordStatistics.
- [ ] After dependency approval, verify and pin the official Prometheus client version and run vulnerability checks. Run focused tests, implement bounded snapshots/event-time recording with official histogram collectors, then run `go test -race ./internal/metrics ./internal/controller ./internal/provider ./cmd/runner`.
- [ ] Commit: `feat(metrics): record runner lifecycle latency and pool outcomes`.

### Task 8: Serve the fixed Prometheus schema and verify contracts

**Files:** Create `internal/metrics/{prometheus.go,prometheus_test.go}`; modify `internal/health/health.go` and tests, main.go wiring, both READMEs.

**Interfaces:**
- Consume: existing health snapshots plus Task 7 copied observations; no I/O on scrape.
- Produce: `metrics.NewExporter() *Exporter`, `(*Exporter).Register(observer *Observer, snapshot func() ScaleSetSnapshot) error`, and `(*Exporter).Handler() http.Handler`. Exporter owns a private prometheus.Registry and promhttp.HandlerFor; the per-scale-set snapshot callback reads only memory and contains no provider handles. Register observers once per scale set, outside listener reconnect.
- Match every metric name, label, bucket, zero/unknown rule, and phase from the spec. Exposition/negotiation and escaping are delegated to the official client; do not require a manually fixed Content-Type that breaks supported negotiation.

- [ ] Add tests using official client testutil and httptest for metric names/HELP/TYPE, escaped labels, monotone cumulative buckets, +Inf=count, sum/count values, empty scale sets, absent timestamps, finite reason/outcome values, duplicate registrations, isolated registries, and concurrent scrapes.
- [ ] Add endpoint tests asserting health-port=0 disables metrics as well; liveness stays 200 when degraded, readiness semantics remain intact, and fail-on-call Docker/Tart/GitHub fakes receive zero requests during repeated scrapes.
- [ ] Implement the private exporter using the official client, with fixed-schema snapshot collectors for existing gauges/counters and Task 7 observers for histograms/pool events. Never double-register observers or count snapshots as new events. Do not add a package-global registry, automatic Go/process collectors, high-cardinality identifiers, raw errors, or per-request disk measurement.
- [ ] Run `go test -race ./...` and `go vet ./...`; run repository lint and existing CI. Validate a saved scrape with `promtool check metrics` where promtool is available, using a temporary tool/container without adding a runtime dependency or touching the host daemon without authorization.
- [ ] Document sample capacity/startup latency and failure alert queries with units and process-reset semantics; commit: `feat(metrics): expose runner operational metrics`.
- [ ] Complete authorized live acceptance: two scrapes around real Docker/Tart jobs, verify counters/durations and no scrape-triggered resources; rerun drain acceptance with one active job.

## Draft Self-Review

- Coverage: spec sections 1/2/3/4 map to Stages A/C/B/D; installation targeting remains a separate proposed project.
- Review focus: readiness/assignment races are owned by Task 2; malformed/private logs by Tasks 2–3; backoff bypass by Task 4; provider identity by Tasks 5–6; exposition escaping by Task 8.
- Compatibility: opt-out readiness, additive JSON, existing health endpoints, explicit config operation, scope boundaries, and no implicit restart are retained.
- Dependency limit: existing dependencies plus the proposed official Prometheus Go client only, requiring design approval and a pinned verified version; no package installation during planning.
- Execution gate: all boxes are intentionally unchecked. The design and these draft task boundaries require review before implementation; future commits in this document are proposed messages, not commits performed now.
