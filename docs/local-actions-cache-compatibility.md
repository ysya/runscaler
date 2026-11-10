# Local Actions Cache Compatibility Evidence

Date: 2026-11-10
Implementation baseline: `ded9e96d7857e1903c728db5047718f8dae0559d`.

**Gate verdict: GO for implementation on Linux amd64, with the scoped inputs below.**
The pinned candidates passed the isolated protocol/runtime gate and the real-job
canary. This authorizes continuing integration work, not production deployment,
fleet promotion, arm64 publication, or claims about all language-cache clients.
Metadata alone is not runtime compatibility evidence.

The user approved one bounded privileged DinD test environment on the selected
Linux host, and separately approved a dedicated operator-controlled canary branch plus temporary JIT runner
registrations. Operator-specific repository, run and registration identifiers
are retained in the local evidence ledger rather than this public report. Existing
scale sets, the default branch, and business CI settings were not modified.

## Candidate images resolved from the published releases

These are accepted immutable integration-test inputs for **linux/amd64**. They
are not deployed production defaults. Do not resolve mutable tags again.

| Candidate | Multi-platform index digest |
|---|---|
| `ghcr.io/falcondev-oss/github-actions-cache-server:10.0.0` | `sha256:05c4d9e7169f525f2b74d412c4a081aeec5446a017ec88bae668e63ebfb16aa9` |
| `ghcr.io/falcondev-oss/actions-runner:2.338.2` | `sha256:462ebdbc95fabf0bdf264ffe02ff5182665c6d99ac0f870b67d7f649c1ef1746` |

| Image | Platform | Manifest digest |
|---|---|---|
| Server | linux/amd64 | `sha256:2221d72de0597bc25de605696869a37f98c1641a809dd3c43947d97029149384` |
| Server | linux/arm64 | `sha256:686e40ffd96327d30a1a096568a0f23b93b2f23c00443fb2d7735b9aa83219f7` |
| Runner | linux/amd64 | `sha256:37979728bd9c11eed54fc4195ce7572256bc473e1e033ad420695d43d33e8153` |
| Runner | linux/arm64 | `sha256:136d7e1a08f6ae995a26bf7625de53e3e390c299843e4e1fd392130b41607d1d` |

The index also contains attestation manifests with `unknown/unknown` platform;
those are not additional supported execution architectures. Linux amd64 passed
the scoped runtime gate; arm64 remains unverified and must not be published as
a supported local-cache runner variant until its gate passes.

## Source and provenance findings

### Cache server

The planned source tag is [`c829dd6f58b28b785e090dd938ec1e4ee7c477a2`](https://github.com/falcondev-oss/github-actions-cache-server/tree/c829dd6f58b28b785e090dd938ec1e4ee7c477a2).
Embedded BuildKit provenance for both platforms names actual build revision
[`1b4323045263b70e95a8f1bae81f0ed527f4dc02`](https://github.com/falcondev-oss/github-actions-cache-server/tree/1b4323045263b70e95a8f1bae81f0ed527f4dc02).
Both have exactly the same Git tree: `71cb8c03fec457eb9f0955a28f6d1bd6e49fe6a1`.
The different history comes from the release merge into `master`.

The image label instead names `0a739b2debfee8d4a310451d992f6bc6954a36be`, whose
tree is `fabdf3140ab4cc9cb3e52ee57074cf6ec7f3eab2`. The tag adds only package
versions in Cargo.toml/Cargo.lock and Helm chart metadata. This is explainable,
but the label alone is insufficient provenance. Embedded BuildKit statements
have not been independently authenticated for the server.

OCI config declares user `0`, working directory `/app`, entrypoint
`/github-actions-cache-server`, and port `3000/tcp`. The pinned Dockerfile uses
a distroless final image. Do not add an assumed shell/curl healthcheck.
The actual binary reports 10.0.0. Health, authenticated protocol behavior and
filesystem/SQLite persistence were tested in the isolated amd64 daemon below.

### Runner fork

The planned commit [`987f42c65cee13938ee921d3a292acfdfec4ae9b`](https://github.com/falcondev-oss/github-actions-runner/tree/987f42c65cee13938ee921d3a292acfdfec4ae9b)
is the CD workflow trigger, not the actual OCI source checkout. BuildKit
provenance for both platform manifests names
[`3efdc9c0d8a5b7a441cba3069b0fdc33d1931adb`](https://github.com/falcondev-oss/github-actions-runner/tree/3efdc9c0d8a5b7a441cba3069b0fdc33d1931adb).
The workflow merges upstream, then separately checks out mutable `main` for
build/package/image jobs. Its GitHub workflow attestation verifies against the
fork repository and identifies trigger `987f42c`, [CD run 41275014681](https://github.com/falcondev-oss/github-actions-runner/actions/runs/41275014681).
That attestation does not establish reproducible binaries from the trigger commit.

The six additional commits update the .NET SDK from 8.0.426 to 8.0.427, undici
6.29.0 to 6.29.1, CodeQL/login actions, broker websocket error reporting, and
merge upstream. Cache-handler and update-guard files are unchanged in that range.
The actual image source is **91 commits ahead and 3 behind** official v2.338.2;
the plan's trigger source was 85 ahead and 3 behind. Missing official commits:

- `197ca027b90de199464dee3bb9ff198a1db60967`: Update release version to 2.338.0.
- `e78522a9a7b01c3bcd7ac0da6e3a9bb6cd513a76`: Create patch version 2.338.1.
- `d1eb12d4b115c301201de27d667284e1a80f2ab5`: Create patch version 2.338.2.

The runtime delta was reviewed: Results overrides; update guards; an upstream
embedded-GitHub-context feature flag refactor; migrated broker-listener fallback
restriction and error reporting; updater marker paths/working directories; and
.NET/NuGet/Node upgrades. Packaging and removed upstream CI also differ. The
scoped gate is not a full regression suite for all those upstream changes. Do not describe this image as
official v2.338.2 plus a single cache patch.

Source inspection found:

- Node and container handlers read `CUSTOM_ACTIONS_RESULTS_URL` for Results API
  requests. They still assign the GitHub-provided `ACTIONS_CACHE_URL`.
- Node actions set `ACTIONS_CACHE_SERVICE_V2` only when the job supplies
  `actions_uses_cache_service_v2`; the custom URL does not force that flag.
- Both refresh-message handlers skip self-update when the custom URL is set.
- No custom override was found in CompositeActionHandler or ExecutionContext;
  nested handlers and shell exports need actual job tests.
- Stock runner handlers also overwrite Results URLs, so an ordinary runner plus
  a URL variable cannot be assumed to support local caching.

OCI config declares user `runner`, working directory `/home/runner`, default
`/bin/bash`, and no entrypoint. The Dockerfile creates UID 1001 and Docker group
123, and downloads versioned runner release archives. The release lists archive
SHA-256 digests; assembly equality was verified as described below.
Version labels and source string matches are not a substitute for that check.

The actual amd64 binary reports commit `3efdc9c`, version 2.338.2. `run.sh
--version` succeeds; Node 20.20.2 and 24.22.1, Docker 29.9.2 and Buildx 0.38.1
are present. Actual image assemblies byte-for-byte match the published linux-x64
archive after verifying archive SHA256
`13d35ec05c78c1b0bbd5255b363cd34c464db110ed7759271dfb0a1b1ef57075`:

| Assembly | SHA256 |
|---|---|
| Runner.Listener.dll | `5fcd3b762b1ef5c38c03692f886d9d525162e6b416424852de7a070e7c64b148` |
| Runner.Worker.dll | `5516e3a6b4cdebef7ee9381790bb5929d8c699b6cf33f11dd93746822a796188` |

Decompiling the verified Listener assembly with ILSpy 9.1.0.7988 confirmed both
refresh-message branches test `CUSTOM_ACTIONS_RESULTS_URL` before entering either
self-updater. No artificial GitHub refresh message was injected. Real JIT startup
and jobs below retained the custom endpoint throughout main and post actions.

## Runtime and real-job evidence

The test daemon was Docker 29.1.3, Linux amd64, nested inside a separately approved
DinD container bounded to 2 CPUs, 8 GiB RAM and 8 GiB Docker-data tmpfs. It has no
production Docker socket/directory mounts or outer-host published ports. Inner
runner socket/work-directory mounts refer only to that test daemon and its data.
Existing host runner settings were inspected read-only.

The controlled-issuer fixture uses disposable in-memory RSA keys and RS256 JWTs,
with validation enabled, explicit issuer/JWKS/audience, filesystem + SQLite,
and a 64 MiB disposable budget. It passed:

- Missing/malformed/expired/wrong-issuer/wrong-audience/invalid-signature token rejection.
- Read-only save rejection and read-only restore success.
- Authenticated multi-block upload/finalize, exact restore and prefix lookup.
- Other-repository and other-branch misses.
- Exact restore after stopping/removing/recreating the service against the same data volume.
- A controlled non-cache Results passthrough; this alone is not GitHub artifact evidence.

Payload: 300,000 generated bytes; SHA256
`3c65ea93424a9c362fec0e3a69ea36031e8a358441479dd665cc6110eabe7b08`.

**Download capability caveat:** filesystem `/download/<UUIDv4>` paths are opaque
bearer capabilities, not expiring query signatures. Unknown capability paths
return 404; possession of a valid URL permits download without another JWT until
data is deleted. Do not log/share those URLs or describe them as short-lived
signed URLs. Authenticated lookup and repository/branch scope checks do not revoke
an already disclosed capability. Review this limitation before production rollout.

### Actual GitHub job

An operator-controlled two-job canary passed at an immutable fixture revision.
Its save and restore jobs used distinct JIT registrations and fresh working
directories, the exact fork digest, normal GitHub token verification, and a
separate local service/data volume. The advertised URL was bound only on the
inner daemon bridge and verified from the runner namespace. The private run
link, source commit and registration IDs are retained in the execution ledger.
No default-branch or business-CI configuration change was made.

Passed handlers/actions:

| Check | Pinned revision / result |
|---|---|
| Node main + post | Local guard verified custom Results endpoint, v2 flag and real-token lookup |
| Composite nested Node main + post | Same checks passed |
| Container main + post-entrypoint | Same endpoint/auth checks passed; explicit `ACTIONS_CACHE_SERVICE_V2=true` required |
| Standard cache cold restore + real post-save | actions/cache v6.1.0, `55cc8345863c7cc4c66a329aec7e433d2d1c52a9` |
| Separate fresh-runner warm restore | actions/cache/restore at the same revision; exact hit and SHA256 passed |
| Actual GitHub artifact upload | v7.0.2, `cf430e030ddbb5b0abf93d22962f4752f3646cd9` |
| Actual GitHub artifact download | v8.0.2, `9000827ccba6bdab643e8b6fd33ac0654aef8333`; SHA256 passed |
| Checkout | v7, `3d3c42e5aac5ba805825da76410c181273ba90b1` |

The GitHub cache listing for the canary's exact run/attempt cache key returned
zero entries after the successful local save/restore. This supports local storage
for that key; it is not packet-level accounting of all CI traffic. Artifact
payloads intentionally still use GitHub.

### Actual BuildKit client

Buildx 0.30.1 and BuildKit 0.34.2, immutable index
`moby/buildkit@sha256:c5f8862f52499773af85bc07695b2379ef08e8002352b0df2c4864c91d78dabe`,
ran in two distinct docker-container builders on the isolated fixture network.
The first exported `type=gha,version=2` to the local controlled-issuer service;
the fresh second builder imported the cache manifest and fetched cached layer
`sha256:a8d2498c003f650fe90843119b4d0f9c3ba4f58bee4ea6e63716b41644768321`.
Both exported filesystem payloads match the SHA256 above. This proves cache
protocol and payload reachability from actual BuildKit namespaces; it is a
minimal COPY-layer probe, not a compilation speed benchmark or cache-mount test.

## Image/provider integration verification

The complete toolchain Dockerfile with local mode enabled passed a hosted
validation build with publication and registry-cache writes disabled. It used
the exact accepted fork base. The final image build reran its installed checker
with an offline endpoint after all toolchain layers were installed.

The provider injects the endpoint only for enabled scale sets, executes the
checker after volume ownership setup, and reaches the listener only on success.
Executable isolated-image tests rejected missing endpoint/manifest/checksum,
wrong metadata and modified Worker/Listener assemblies before a sentinel listener;
the verified contract passed with networking disabled. The official standard
base plus the opt-in build flag was rejected, and the disabled standard contract
contained no installed checker or valid manifest.

Root/child cleanup and startup/doctor-fix regression tests preserve the independent
cache service/data. The exact Compose model also passed real upload/restore from
runner and BuildKit namespaces, then restored the same checksum after its stopped
service container was pruned and recreated against the external volume.

## Remaining acceptance work

The isolated language-client, outage, concurrency, post-handler and partial-upload
cancellation checks are complete; the dated results below record their scope
and measurements. Remaining promotion work is:

- Host-specific provider-managed canary, maintenance restart and recovery validation before expansion. The assembled amd64 image is already published; see below.
- Linux arm64 runtime/handler/client parity. Do not imply it from the OCI index.
- Actual deployment address, disk budget and operator-reviewed download capability limitation.

Legacy v1 clients remain unsupported. A shell command does not automatically
receive Node-action runtime exports. Do not promise transparent support for
arbitrary existing actions or shell scripts. There is no approved cloud-cache
fallback or cloud baseline upload.

## Published assembled image and current server semantics (2026-11-11)

The assembled Linux amd64 image is published at
`ghcr.io/ysya/actions-runner-local-cache@sha256:ce94d981fbd73f15788bf98af0055d8ccea29a50932484e50c962c67cd45d577`.
Anonymous registry reads and strict GitHub build-provenance verification passed
against source `5cab9dff768d5974e4ae59da416ae9a1a585ad2d` and the cache-image workflow.
Publication is separate from each host's rollout acceptance.

A supplemental isolated test used different 1 MiB payloads for the same cache
identity while reading concurrently: three saves completed, four reservations
were rejected, and 35 successful reads each matched one entire known payload.
No mixed or partial payload was observed. Auth negatives, repository/branch
isolation, controlled Results passthrough and interrupted-upload invisibility
also passed in that fixture. These are scoped results, not an immutable-key or
universal concurrency guarantee.

The v10 source explicitly supports replacement of completed entries; its upload
reservation only excludes concurrent in-progress uploads. A transactional
promotion replaces the published storage location. Unlike GitHub cache keys,
completed entries are therefore mutable, and a stale download capability can
return 404 after replacement. See [operator implications](local-actions-cache.md#cache-semantics).

## Read-only probe

Run only with an operator-provisioned isolated Linux daemon/context:

```sh
scripts/local-cache/probe.sh \
  --context <isolated-context> \
  --server-image ghcr.io/falcondev-oss/github-actions-cache-server@sha256:05c4d9e7169f525f2b74d412c4a081aeec5446a017ec88bae668e63ebfb16aa9 \
  --runner-image ghcr.io/falcondev-oss/actions-runner@sha256:462ebdbc95fabf0bdf264ffe02ff5182665c6d99ac0f870b67d7f649c1ef1746
```

It refuses missing/shared contexts and mutable image references before any
Docker request, clears ambient Docker connection variables, then reads Linux
daemon information and remote manifests using only the selected context.
It deliberately exits **3** (`compatibility_gate=incomplete`) even if those
requests succeed. It creates, pulls, removes, and prunes no resources. A renamed
context is not isolation; provision a separate daemon for eventual lifecycle tests.

Reproduce registry/source evidence with `docker buildx imagetools inspect`, its
`--format '{{json .Image.Config}}'` / `--format '{{json .Provenance}}'` views,
`gh attestation verify oci://<runner-index-ref> --repo falcondev-oss/github-actions-runner`,
and GitHub commit/tree/compare APIs. Keep raw tokens, job payloads and cache
contents outside Git and logs.

## Integration boundary

Continue the service bundle and opt-in image/provider implementation using the
accepted digests on Linux amd64. Keep production deployment behind the later
operator approval and remaining acceptance work. Do not silently change carrier,
disable validation, infer arm64 support, or fall back to cloud cache storage.

## Standard client acceptance (2026-11-10)

The client cohort uses the accepted amd64 base plus the tested
`local-cache-contract` checker overlay. The complete Android/Java/Node toolchain
variant passed its separate build/offline-check gate; its live deployment
acceptance remains an operator approval gate. Each measurement job used a fresh
JIT runner/container and workspace, with at most two concurrent jobs inside the
same 2-CPU/8-GiB DinD boundary. Cache data is on its **RAM-backed tmpfs**, not the
production host filesystem. These synthetic fixtures are compatibility evidence,
not production SSD throughput or an application CI speedup claim.

Additional immutable actions and selected toolchains:

| Client/setup action | Revision | Selected toolchain/provider |
|---|---|---|
| actions/setup-go v7.0.0 | `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e` | Go 1.27.3; normal module/build cache |
| actions/setup-node v7.1.0 | `949feb2413d6458794dcd2491c4babbbce0c15c1` | Node 24.22.1; npm package store |
| actions/setup-java v6.2.0 | `77dd5b2ce225ea11469928ac317850245a74ae78` | Temurin 17.0.17+10; JDK/dependency cache disabled |
| gradle/actions/setup-gradle v6.4.1 | `8da46b539cd8fe6ab27ca75128919a18d757604c` | Gradle 9.2.1; basic provider, explicit writes on canary branch |
| dtolnay/rust-toolchain | `b725eda525362e7cf34af9a7243cac1e3f01a7ec` | Rust 1.99.1 |
| Swatinem/rust-cache v2.9.3 | `d1527c8f67ef9af357290475d4af4b0fab8f12e0` | GitHub provider; shared key; job-id key disabled; environment/lock hashes retained |
| docker/setup-buildx-action v4.4.1 | `f87e5991a6d7451dcb8d9637bfbc97413f497069` | Buildx 0.38.1; fresh docker-container builder; binary cache disabled |
| docker/build-push-action v7.4.0 | `c3c9e263c25d99ce0380d002d59b67737d91b0dc` | BuildKit 0.34.2 digest above; explicit local gha v2 URL and image/platform scope |

The direct fixture verifies 300,000 restored bytes against SHA256
`3c65ea93424a9c362fec0e3a69ea36031e8a358441479dd665cc6110eabe7b08`.
Go/npm/Gradle/Rust outputs share the fixed public fixture result SHA256
`533180a95f969ef7892d1466a16a0b59b5cc43e79ff08bfa084ffa1e7033ce60`.
Docker compares the exported payload byte-for-byte with that same fixture file.
Rust additionally checks the cached itoa 1.0.15 archive against the Cargo.lock
checksum before normal locked resolution. npm's warm install runs offline.
Rust's upstream cleanup removes registry metadata: a cache hit is not a promise
of completely offline Cargo resolution or reuse of every compilation output.

### Measurements

All **36 jobs** in three cold/warm pairs per workload passed. Each cold pair used
fresh dedicated service data; each warm phase used new runners and the same
compatible keys. Medians below are instrumented fixture measurements (seconds);
they are not a matched comparison with production CI or GitHub storage.

| Workload | Pairs | Cold window | Warm window | Cold build | Warm build | Warm body bytes |
|---|---:|---:|---:|---:|---:|---:|
| direct | 3 | 0.511 | 0.955 | 0.009 | 0.010 | 1805 |
| go | 3 | 18.515 | 7.755 | 10.573 | 0.175 | 14230821 |
| node | 3 | 6.025 | 6.494 | 0.536 | 0.304 | 6420 |
| gradle | 3 | 27.495 | 23.055 | 10.449 | 6.930 | 135748124 |
| rust | 3 | 21.586 | 25.337 | 0.232 | 0.129 | 85964 |
| docker | 3 | 8.446 | 10.845 | <1 | 1.000 | 717 |

| Workload | Cold archive ms | Warm extraction ms | Warm HTTP body ms |
|---|---:|---:|---:|
| direct | 5 | 4 | 2 |
| go | 1203 | 254 | 93 |
| node | 5 | 4 | 1 |
| gradle | 2451 | 537 | 441 |
| rust | 9 | 11 | 5 |
| docker | unknown | unknown | unknown |

Go and Gradle had lower fixture medians; tiny npm/Rust/Docker/direct fixtures
did not consistently improve the measured total window. Tool setup and shared
CPU/network scheduling remain significant. No general end-to-end speedup is
inferred. Docker `<1` is a sub-second step timestamp interval, not a fabricated
zero-duration build. Docker byte residuals matched the corrected proxy exactly
on the third cold/warm pair.

`total_ms` covers the report action through cache post-saves, including tool
setup but excluding checkout/JIT startup. Build, archive, extraction and HTTP
body transfers are measured separately. Docker build action time uses GitHub
step timestamps (one-second resolution); its internal archive/extraction time is
unknown. HTTP body counts omit headers/transport overhead and include upload
block-list records. Missing measurements are unknown, never fabricated zeros.
No comparable historical cloud-cache baseline was collected, and no cloud-cache
benchmark was created.

Node preload and an isolated streaming proxy observe cache destinations without
logging tokens, headers, object paths or capability URLs. The proxy's original
scope substring also matched Rust's shared key; a regression test fixed its
BuildKit key classification before publication. Historical Docker body counts
use aggregate proxy bodies minus the five independent Node-client counters,
cross-checked against the corrected classifier. This affects reporting labels,
not cache routing or restored contents.

### Failure and persistence evidence

Actual pinned `actions/cache/save` and `restore` bundles, with a disposable
controlled issuer and validation enabled, passed these scoped negative cases:

| Scenario | Save/restore outcome | Payload outcome |
|---|---|---|
| Invalid controlled token | Both exit 0 with a warning | No successful save or restored payload |
| Separate 1 MiB payload filesystem reaches ENOSPC | Both exit 0 with a warning | No successful save or restored payload; server ENOSPC independently observed |
| Unavailable test service | Both exit 0 with a warning | No successful save or restored payload |

The test observer refused foreign cache origins; none were attempted. This is
not a claim about every helper's error handling. The dedicated real-job outage
cohort and artifact/build outcomes are recorded below after execution.

A separate real GitHub six-job outage cohort passed with the dedicated cache
endpoint stopped: direct cache, Go, npm, Gradle/basic and Rust/GitHub helpers
warned and their builds continued. No foreign cache origin was attempted. The
artifact action failed as expected; its test explicitly checked the failure
outcome, so ordinary artifact-dependent CI can fail when this service is down.
Docker omitted imports to isolate export behavior: `ignore-error=true` allowed
the real build to succeed despite export failure, while an invalid Dockerfile
still failed and the test explicitly checked that outcome. This does not prove
that every import failure is optional. All negative cases were confined to the
approved isolated environment.

The executable probe's runtime, authentication, lifecycle and protocol benchmark
suites passed in the isolated daemon. Invalid/default contexts, mutable image
refs, unknown suites and pre-existing resource-name collisions are refused.
Subprocess tests verify no blanket prune/delete calls and cleanup only of
recorded, owner-checked IDs. Its `clients` suite remains unknown without signed
GitHub jobs; read-only metadata alone always leaves the full gate incomplete.
Service recreation retained the external data and restored the exact checksum.
The service/data ownership boundary also passed normal/orphan cleanup tests.

### Remaining boundaries

- Only the pinned Linux amd64 inputs and the listed clients/providers passed.
  pnpm was not independently measured; legacy v1 and arbitrary third-party cache
  providers are not implicitly covered.
- `CACHE_MAX_SIZE_BYTES` takes precedence over filesystem percentage in this
  release. It drives eviction, not an admission limit or hard filesystem quota;
  temporary uploads require disk headroom. The bundle no longer advertises a
  simultaneous 80% limit that would be inactive.
- Local cache hits do not prove that every compiler output is reusable or that
  full application CI is faster. Tool installation and package-registry
  metadata traffic remain outside cache payload routing.
- Full assembled-image live acceptance, production filesystem measurements,
  operator-approved endpoint/budget and rollout remain per-host gates.
  No existing host scale set or business CI configuration was changed.

### Post-handler payload retest

A separate two-job canary on 2026-11-10 wrote real cache-v2 payloads from
Node, nested composite and container **post handlers**, then restored all three
on a fresh JIT runner and checked exact bytes. The standard actions/cache
post-save/restore and artifact upload/download also passed in these jobs.
The container fixture explicitly enables v2 and calls its write helper from
post-entrypoint; endpoint/authentication checks alone are not evidence of a save.

### Interrupted upload acceptance

The controlled-auth lifecycle suite also passed an actual partial HTTP upload
cancellation on the pinned amd64 service. It staged one complete block, wrote
4 KiB of a second declared 64-KiB block, and destroyed that request's socket
without ending the body, committing blocks or finalizing the cache entry. Exact
and prefix lookups both missed immediately and after service recreation on the
same data volume; the independently finalized payload still restored with its
original checksum. This verifies invisibility of the unfinished entry, not
immediate reclamation of temporary disk space or every action's cancellation
handler. The explicit probe lifecycle suite reproduces these assertions.
