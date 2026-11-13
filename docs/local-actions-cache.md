# Host-local Actions cache

Runscaler supports two opt-in cache-v2 receiver modes for disposable Docker
runners on a Linux host:

- **Native Go receiver:** the `runner` manager process starts its own private
  HTTP listener and stores cache data in `[actions-cache].data-dir`. Enable it
  globally, then select Docker scale sets with `native-actions-cache = true`.
  No separate cache process or container is required. See [native receiver
  setup and recovery](#native-go-receiver).
- **External receiver:** run a separate service, such as the Rust/Compose
  deployment below, and set an explicit `docker.actions-cache-url` on selected
  scale sets. Its process, storage, and management remain independent of runner.

Supported cache-v2 clients retain their standard Actions cache keys and restore
behavior. Archive creation/extraction and local HTTP transfers still happen;
neither mode is a zero-copy cache.

The [external Rust service compatibility report](local-actions-cache-compatibility.md)
records the original pinned client/runtime gate. The native Go receiver passed
signed-job acceptance covering the fail-closed runner image, client matrix,
restart, and outage behavior. Detailed run evidence is kept in private operator
notes. Both modes require a reachable private endpoint, a cache-capable runner
image, and validation on each new host. The unified `actions-runner` image
supports GitHub and local cache; the scale-set setting selects the endpoint.

## External receiver: ownership and storage

The operator owns `runscaler-actions-cache`; Runscaler does not provision it per
job. Its label is `managed-by=runscaler-cache`, never `managed-by=runner` or
`runscaler.runner-id`. The external volume `runscaler-actions-cache-data` stores
filesystem payloads and SQLite together under `/app/.data`.

Do not mount this data into jobs or put it in `cache-volumes` / `[[docker.cache]]`.
Those stores have separate retention/wipe semantics. Let the server evict its
own entries: 30 days unused and an explicit absolute byte budget. In this pinned
release the absolute budget takes precedence over the filesystem percentage
setting; they are not simultaneous limits. Eviction is not a hard filesystem
quota and temporary uploads can exceed the budget. Reserve host disk headroom
and use a dedicated filesystem/quota when a hard ceiling is required. Do not
delete payload files or SQLite independently.

A stopped service container can be removed by stopped-container pruning. Its
external volume remains the data contract; Compose recreates the service against
that volume. Do not use external volume deletion as routine runner cleanup.

## External receiver: endpoint and budget

Fill every mandatory field in `deploy/local-cache/.env.example`, copied to an
ignored `.env` in the same directory:

- `CACHE_BIND_IP`: a selected private host-interface IP, not `0.0.0.0` or `::`.
- `CACHE_PORT`: host port, default 3000.
- `CACHE_ADVERTISE_URL`: absolute HTTP(S) URL with a trailing slash, reachable
  from the actual runner **and sibling BuildKit** namespaces.
- `CACHE_MAX_SIZE_BYTES`: positive integer bytes chosen from available disk.
- `CACHE_MANAGEMENT_KEY`: private operator credential, supplied only to the service.

Do not assume `localhost`, a Compose DNS name, or `host.docker.internal` reaches
the host from every Linux container. The pilot used a verified private bridge
address inside isolated infrastructure; production must verify its own routes.
Compose requires these inputs; it does not validate that an operator-supplied IP
is private or automatically create firewall rules. Keep the listener private.
The service listens on all interfaces *inside* its container, while host port
publication uses the explicitly selected interface.

## External receiver: startup

Deploy only after reviewing the canary evidence and approving the actual host,
image, endpoint and budget. Never overwrite a pre-existing volume of unknown
ownership. If the named data volume is new, create it explicitly:

```sh
docker volume create --label managed-by=runscaler-cache runscaler-actions-cache-data
```

Prepare private configuration, validate without printing the resolved secret,
and start independently of Runscaler:

```sh
cp deploy/local-cache/.env.example deploy/local-cache/.env
# Fill the mandatory operator inputs in the ignored .env first.
docker compose --env-file deploy/local-cache/.env -f deploy/local-cache/compose.yaml config --quiet
docker compose --env-file deploy/local-cache/.env -f deploy/local-cache/compose.yaml up -d
```

Token validation stays enabled. Do not set `SKIP_TOKEN_VALIDATION` on a real
service. The controlled issuer in the test fixture is exclusively for isolated
authentication tests. No GitHub runtime token or runner registration credential
belongs in this service configuration or the tracked examples.

Probe `/health` using an external HTTP client from the real runner namespace,
then from a client sharing the actual BuildKit network namespace. The server's
distroless image does not provide a shell/curl healthcheck. A host-only health
response is insufficient: also exercise actual upload/download URLs and verify
the restored checksum from both client paths.

## Runner opt-in and image

For the native receiver, set `native-actions-cache = true` on the selected Docker
scale set and configure the global `[actions-cache].advertise-url`. Use a verified
`actions-runner` image by immutable digest for a canary; see
[configuration](configuration.md#host-local-actions-cache-opt-in) and
[image construction/maintenance](../images/actions-runner/README.md#build-and-verification).
The bare fork base does not provide the installed capability check.

Before the listener starts, Runscaler completes volume ownership setup and runs
`/usr/local/bin/runner-local-cache-check`. It checks the exact accepted protocol,
source/version and Worker/Listener hashes against the root-owned manifest under
`/usr/local/share/runscaler/`. Missing/changed files fail before accepting work.
The check makes no network request; availability tests are separate.
The Docker provider supplies `CUSTOM_ACTIONS_RESULTS_URL` for the selected
receiver. An external service's management key is never injected into jobs;
native mode has no separate management key. Empty configuration keeps the
original startup behavior.

## Client boundaries

Use only clients and immutable revisions that passed acceptance. Legacy v1
clients are unsupported. The unified image keeps GitHub's `ACTIONS_CACHE_URL`
without a local endpoint and points it at the local receiver when selected.
The local receiver rejects legacy v1 requests without a cloud fallback.
Raw shell tools need explicit runtime exports; container cache-v2 clients need
an explicit v2 flag if their handler does not set it. The accepted setup-go,
setup-node/npm, Gradle basic and Rust GitHub-provider revisions are listed in
the evidence report; pnpm and other providers are unverified. Rust can still
fetch registry metadata after restoring cached crates; this is not full offline
Cargo support. Toolchain/package-registry downloads are separate from cache
payload transfers.

BuildKit uses `type=gha,version=2` with a distinct scope per image/platform.
Changing this endpoint does not redirect existing registry/S3 cache declarations
or persist mutable `RUN --mount=type=cache` contents automatically. During canary,
disable overlapping shared writable language-cache mounts without deleting them.

Artifacts remain on GitHub and pass through the local Results endpoint. A
service outage can therefore affect artifact actions too. Do not promise every
cache/action failure is nonfatal or silently switch cache payloads to GitHub.
Actual six-client outage tests showed five Node cache helpers warning and
continuing builds; the artifact action failed. Docker's optional export required
`ignore-error=true`; the test omitted imports to isolate that case and still
asserted that an invalid build failed. This does not promise arbitrary cache
imports or other clients tolerate outages. No automatic cloud fallback exists.

## External receiver: download capabilities, backup and rollback

For filesystem storage, returned `/download/<UUIDv4>` paths are bearer
capabilities without an independent expiry; possession permits download until
the data is deleted. Never print/share payload URLs. Authenticated scoped lookup
does not revoke a leaked URL. Review this property before production use.

After draining/pausing affected local-cache jobs, stop the service before taking
a coordinated backup of the **entire** data volume (SQLite and payloads). Store
backups outside the repository with appropriate access controls. Restore both
together using a compatible pinned server image.

v10 has one-way migrations: databases older than v9.10 are dropped and storage
cleared; v9.10 is migrated. Do not downgrade a migrated volume. Use a compatible
coordinated snapshot or a separate new data volume instead.

## Provider-managed canary and recovery

Before changing a live manager, record its binary/configuration and service
definition, confirm its actual shutdown behavior, and arrange a maintenance
window. The manager has one machine-wide lock, no hot reload and no per-set
`enabled` switch; `max-runners=0` is invalid. A second manager is not a canary
isolation mechanism. Adding/removing a scale set requires restarting the manager
and briefly interrupts all its listeners. Preserve unrelated scale-set values.

Stop dispatching canary work and drain current work before replacing files.
For older managers whose drain behavior is unverified, first arrange that no
existing jobs are running; do not assume a new binary changes the running
process's shutdown behavior. Inspect the service stop deadline and align it
with the reviewed drain budget before stopping: a legacy systemd 90-second
limit cannot accommodate the new default two-hour drain. See
[service deadline handling](deployment.md#graceful-shutdown).
Never force-kill business jobs to finish rollout.

After explicit deployment approval, pin the published unified image by its
verified digest. Start the native Go listener on a separate private port and
fresh private data directory. If migrating an existing Rust deployment, keep
that service and its data available for rollback. Add a
canary scale set with only a unique label and one-runner maximum, so generic
organization jobs cannot match it. Verify signed save/restore, artifact
passthrough and sibling BuildKit upload/download on provider-managed runners
before draining and switching the selected existing scale set. Disable
overlapping shared writable language-cache mounts without deleting their data.
Live capacity and production disk performance need their own observations.

Rollback stops canary dispatch and drains work. For migration from an existing
external receiver, restore the reviewed prior binary/config and compatible
service definition with the manager stopped, including the selected scale set's
external endpoint. Before restarting, replace any retired runner-image reference
in a backup with a pullable digest of `ghcr.io/ysya/actions-runner` that was
verified with the prior manager, and validate the resulting config. Do not rely
on locally cached image layers.
Retain each existing cache data store and private backups; do not delete a GitHub scale-set
registration as routine rollback. An isolated JIT test is not a rehearsal of
this production manager restart.

Simply unsetting the endpoint and resuming unchanged cache actions sends payloads
back to GitHub. Keep canary workloads stopped unless the operator explicitly
chooses a reviewed workflow that omits all applicable cache operations. A shared
helper-disable recipe has not been validated here; no automatic resume path is
provided.

## Native Go receiver

The optional native receiver is implemented in the manager on a separate private
port and fresh data directory. It uses token-derived repository/scope permissions,
an immutable completed key, ordered Azure blocks, expiring local payload URLs,
and SQLite plus private files. It forwards non-cache Results calls to the fixed
GitHub upstream for artifacts. Its listener remains available during normal
manager drain. Back up or restore the entire native directory only while the
manager is stopped; the database, WAL, objects, and capability key belong
together. `runner status` reports readiness and usage; `runner cache` reads
completed-entry counts/bytes without creating a missing store.

For local protocol verification, install Node.js and run:

```sh
RUNSCALER_NATIVE_FIXTURE=1 go test -race ./internal/actionscache -run '^TestNativeProtocolFixture$' -count=1
```

This starts only a disposable issuer and cache HTTP listener on loopback. It
checks authenticated block uploads, exact-byte restore, cancellation, store
reopen, and controlled artifact passthrough. It does not register a runner or
prove compatibility with signed GitHub jobs.

When migrating an existing external deployment, retain its service and data as
a rollback endpoint until a separate retention decision. A new native
deployment does not need the Rust service.
The unified runner image keeps GitHub's `ACTIONS_CACHE_URL` without a local
endpoint; native v2 selection points it at the local receiver, which rejects
legacy requests locally. Review the private signed-job acceptance record
before rollout. Native protocol tests alone do not establish real signed-client
compatibility.
