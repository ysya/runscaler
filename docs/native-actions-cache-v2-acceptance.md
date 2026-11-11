# Native Actions cache v2 acceptance

Date: 2026-11-11. Scope: Linux amd64, the Go 1.27.4 receiver,
and the fail-closed runner image below. This is isolated signed-job evidence;
the production scale set requires its own provider-managed canary and drain.

```text
ghcr.io/ysya/actions-runner-local-cache@sha256:0fd6bdb1342bd3cdb63e0d3b4ee4da7a4fae68f7b61e39bd1153d8181f76e2a5
```

The [image build](https://github.com/ysya/runscaler/actions/runs/42972487557)
compiled and tested the pinned Worker source plus
[`fail-closed-v1.patch`](../images/actions-runner/fail-closed-v1.patch), checked
the Worker and Listener hashes, and published a digest with a verified
repository/workflow attestation. The checker rejects a missing local Results
URL before the runner accepts work. Both Node and container handlers route the
legacy `ACTIONS_CACHE_URL` to the local receiver, where the v1 API is rejected.

The private operator canary repository is `ysya/runscaler-native-cache-canary`.
Its [signed save/restore gate](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42984870563)
passed on two disposable JIT runners after a receiver restart. It exercised
Node, nested composite, and container main/post handlers, actual
`actions/cache` post-save and fresh-runner restore with exact bytes, local v1
rejection, and artifact upload/download through the fixed Results upstream.
GitHub's cloud-cache count and temporary runner count were both zero afterward.

## Fresh-data client matrix

Each cold job below used a fresh receiver data directory. Each warm job used a
new disposable runner and the same data directory and cache key. The runner's
workspace path stayed stable across the pair. Every workflow completed
successfully; the direct and language helpers checked hit/miss outcomes and
fixture bytes, while BuildKit wrote and imported the same layer digest.

| Client | Cold run | Warm run |
|---|---:|---:|
| `actions/cache` | [42981630071](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42981630071) | [42981837281](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42981837281) |
| `setup-go` | [42982001074](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42982001074) | [42982173185](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42982173185) |
| `setup-node` / npm | [42982315460](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42982315460) | [42982470635](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42982470635) |
| Gradle basic | [42983345964](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42983345964) | [42983539947](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42983539947) |
| Rust GitHub provider | [42981239165](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42981239165) | [42981393068](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42981393068) |
| BuildKit gha v2 | [42983750118](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42983750118) | [42983931921](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42983931921) |

The cold BuildKit job wrote layer
`sha256:a92c1937f65ffdf650de780871820e91ba5b2f0be1bd7e20a30f5b0a8b4f08d0`;
the warm job imported/extracted that digest. After another receiver restart,
[direct restore](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42984345667)
and [BuildKit import](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42984554655)
passed against the retained data. The store reopened with eight completed
entries.

An earlier Rust warm attempt missed despite an identical helper key because
the test harness gave each JIT runner a different absolute workspace path.
The [pinned Rust helper](https://github.com/Swatinem/rust-cache/blob/d1527c8f67ef9af357290475d4af4b0fab8f12e0/src/config.ts#L144-L150)
resolves its target path against that workspace, and the cache toolkit hashes
those paths into its version. Retrying with the cold runner's path
[passed](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42980790667);
the fresh-data pair above also passed with a stable `_work` path.

The isolated DinD container had a two-CPU, 16 GiB memory, and 12 GiB Docker
tmpfs limit, with no host mounts or published ports. Those limits were raised
from the initial eight-GiB values after OOM and full-tmpfs failures during
image/BuildKit setup. These are compatibility tests, not production disk or
speed benchmarks. Only the pinned clients and Linux amd64 were exercised;
legacy v1 remains rejected locally. Artifact traffic still uses GitHub through
the Results passthrough.

## Endpoint outage

The isolated receiver was stopped while six new signed jobs ran:
[direct cache](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42985269588),
[Go](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42985482739),
[Node/npm](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42985714362),
[Gradle](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42985902466),
[Rust](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42986165367),
and [BuildKit](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42986359344).
All six workflows passed their explicit outcome checks. The cache helpers
warned and the fixture builds continued. The direct job asserted that artifact
upload failed while Results passthrough was unavailable. BuildKit omitted
import to isolate export behavior: `ignore-error=true` allowed that export to
fail without masking an intentionally invalid Dockerfile. No cloud-cache
fallback was observed or configured. These results do not make arbitrary
cache imports or artifact-dependent workflows nonfatal.
After the receiver restarted on the same data directory, a
[fresh-runner direct warm restore](https://github.com/ysya/runscaler-native-cache-canary/actions/runs/42986654794)
passed with the original exact-byte fixture.
