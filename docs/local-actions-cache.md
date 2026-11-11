# Host-local Actions cache

Run one persistent cache service on the same Linux host as disposable Docker
runners. Supported cache-v2 clients retain their standard Actions cache keys and
restore behavior; archive creation/extraction and local HTTP transfers still
happen. This is not a zero-copy cache.

The current compatibility gate covers Linux amd64, the pinned server/runner
candidates, actions/cache v6.1.0, setup-go, setup-node/npm, Gradle basic,
Rust GitHub-provider cache, artifact passthrough and BuildKit v2 export/import.
See [immutable revisions, three-pair measurements and limits](local-actions-cache-compatibility.md).
The isolated real-job tests, provider guard and full toolchain image build passed.
This is not production rollout evidence: publication of the assembled image and
a provider-managed host canary remain gated on operator approval. Arm64 and
other clients are unverified. The standard runner image is unchanged.

## Ownership and storage

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

## Select the endpoint and budget

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

## Operator startup

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

## Opt-in runner startup

Set `actions-cache-url` only on the selected Docker scale set, with a verified
cache-capable variant by immutable digest; see [configuration](configuration.md#host-local-actions-cache-opt-in)
and [image construction/maintenance](../images/actions-runner/README.md#opt-in-host-local-actions-cache-variant).
The bare fork or a standard image does not provide the installed capability check.

Before the listener starts, Runscaler completes volume ownership setup and runs
`/usr/local/bin/runner-local-cache-check`. It checks the exact accepted protocol,
source/version and Worker/Listener hashes against the root-owned manifest under
`/usr/local/share/runscaler/`. Missing/changed files fail before accepting work.
The check makes no network request; availability tests are separate.
Only `CUSTOM_ACTIONS_RESULTS_URL` is added for local mode; management credentials
remain with the service. Empty configuration keeps the original startup behavior.

## Client boundaries

Use only clients and immutable revisions that passed acceptance. Legacy v1
clients are unsupported. The original accepted image supplies GitHub's
`ACTIONS_CACHE_URL`; the separate native v2 fail-closed image points it at the
local receiver, which rejects legacy v1 requests without a cloud fallback.
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

## Download capabilities, backup and rollback

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

After explicit deployment approval, publish the separately tagged amd64 image,
verify its digest/attestation, start the independent service and append a
dedicated canary entry with a unique label and one-runner maximum. Disable
overlapping shared writable language-cache mounts without deleting their data.
Verify real runner and sibling BuildKit upload/download paths, a fresh-runner
restore after service recreation, and end-to-end timings before expansion.
Two concurrent isolated jobs passed; live capacity still needs checking.

Rollback stops canary dispatch and drains work. With the manager stopped, remove
only the canary configuration entry and restore the reviewed prior binary/config
and compatible service definition, then restart the remaining entries unchanged.
Retain the external data volume and private backups; do not delete a GitHub
scale-set registration as routine rollback. An isolated JIT test is not a
rehearsal of this production manager restart.

Simply unsetting the endpoint and resuming unchanged cache actions sends payloads
back to GitHub. Keep canary workloads stopped unless the operator explicitly
chooses a reviewed workflow that omits all applicable cache operations. A shared
helper-disable recipe has not been validated here; no automatic resume path is
provided.

## Native Go receiver development

The optional native receiver is implemented in the manager on a separate private
port and fresh data directory. It uses token-derived repository/scope permissions,
an immutable completed key, ordered Azure blocks, expiring local payload URLs,
and SQLite plus private files. It forwards non-cache Results calls to the fixed
GitHub upstream for artifacts. Its listener remains available during normal
manager drain. Back up or restore the entire native directory only while the
manager is stopped; the database, WAL, objects, and capability key belong
together. `runner status --json` reports readiness and `runner cache` reads
completed-entry counts/bytes without creating a missing store.

For local protocol verification, install Node.js and run:

```sh
RUNSCALER_NATIVE_FIXTURE=1 go test -race ./internal/actionscache -run '^TestNativeProtocolFixture$' -count=1
```

This starts only a disposable issuer and cache HTTP listener on loopback. It
checks authenticated block uploads, exact-byte restore, cancellation, store
reopen, and controlled artifact passthrough. It does not register a runner or
prove compatibility with signed GitHub jobs.

The Rust service and its data remain the live rollback endpoint until cutover.
The original runner image still gives legacy v1 clients GitHub's
`ACTIONS_CACHE_URL`; the separate fail-closed image must pass the signed canary
gate before claiming local-only cache routing. Native protocol tests alone do not establish real
signed-client compatibility. Do not switch a host until the separate signed
actions/cache, language-helper, BuildKit, artifact, restart and outage gates pass.
