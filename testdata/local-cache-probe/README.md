# Local cache client fixtures

Run `.github/workflows/local-cache-acceptance.yml` manually only on an explicitly
approved dedicated `runscaler-local-cache-canary` runner and isolated cache data.
Every cold/warm job needs a new ephemeral runner with the same workspace path.
Never point this acceptance workflow at ordinary production cache data.

For each of three pairs: provision fresh service data, run `phase=cold`, recreate
only the service container against that data, then run `phase=warm`. The pair
number labels measurements; it deliberately does not alter normal helper keys.
Use the exact same workflow/fixture commit for the pair. Rust explicitly shares
the job key while retaining environment, toolchain and lockfile hashes. Gradle
uses the basic provider and permits writes on the dedicated test branch.

These are public synthetic dependencies, not application benchmarks. Node caches
npm's package store, not `node_modules`. Rust verifies the restored crate archive
checksum and uses normal locked Cargo resolution: upstream cleanup of registry
metadata means this is not a guarantee of completely offline Cargo operation.
BuildKit exports layer/cache records; mutable `RUN --mount=type=cache` directories
are not made persistent by `type=gha`. Toolchain installation and registry
metadata traffic are outside the local cache-payload guarantee.

`observer.cjs` is a test-only Node preload passed directly to caching action
steps (including their post-steps), never globally installed in runner images.
It refuses foreign cache RPC/payload origins, records streamed payload bytes and
cache archive/extraction timings, and lets normal artifacts use their Results
passthrough. Capability paths and headers are never written to its metrics.
`proxy.mjs` provides equivalent transfer counts for BuildKit in isolated tests;
it is not part of the operator's production deployment. Test both with:

```sh
node --test testdata/local-cache-probe/observer.test.cjs testdata/local-cache-probe/proxy.test.mjs
```

`RUNSCALER_CACHE_REPORT` schema 1 contains workload, phase, pair, events and
`total_ms`: the window from the report action's main step through cache post-save
completion. It includes tool setup and excludes checkout/JIT startup; it is not
full CI duration. Events separately measure cache RPC, payload bytes/HTTP time,
archive/extraction and fixture build/output checksum. Byte counts cover HTTP
bodies, including upload block-list records; headers and transport overhead are
not measured. Absence of a timing is
unknown, never zero. No historical cloud baseline is manufactured.

The explicit `scenario=outage` requires an unavailable dedicated test endpoint
and `phase=cold`; normal warm-hit assertions and the online token lookup are
omitted. Offline runtime checks still verify the custom Results URL and v2 mode.
The direct workload expects artifact upload to fail when Results passthrough is
unavailable. Docker omits cache imports in outage mode to isolate export failure;
import failures are not assumed optional. Docker opts into `ignore-error=true` only for cache export, and a
separate invalid Dockerfile must still fail. Do not interpret these scoped
outcomes as a guarantee that every cache helper treats all failures identically.

`failure-client.mjs` runs the pinned real cache save/restore bundles with a
controlled test issuer: invalid token, a separate tiny payload filesystem that
returns ENOSPC, and an unavailable service. Supply `PROBE_TOKEN_URL`,
`PROBE_OBSERVER_PATH`, and `PROBE_FAILURE_URLS` JSON (`name`, `url`, optional
`invalid_token`). It must run only in the isolated disposable environment.
