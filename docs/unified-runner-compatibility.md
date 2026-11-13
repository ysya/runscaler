# Unified Runner Image Compatibility

Date: 2026-11-13

## Accepted image

The candidate `ghcr.io/ysya/actions-runner:985f2a1` has OCI index digest
`sha256:08b41533e3b03ef5015f17b1412f72436875d7d99d5a352e264892f4379d32c2`.
The index includes Linux amd64 and arm64 execution manifests. Both complete
image builds and the manifest publication passed. The final ARM64 image passed
the local-cache capability check, runner version check and GitHub CLI smoke test.

| Platform | Execution manifest | Compressed OCI layers | Increase over previous standard image |
|---|---|---:|---:|
| Linux amd64 | `sha256:c57f920fd6a84f8f358fb3802b31a31cd61895f33766e2c9ffb8f458cb6d07ea` | 2,074.1 MB | 6.0 MB |
| Linux arm64 | `sha256:a0d501e8304573707638233ddcb1c26fb720bb71308ca89f07a3faf7978e2729` | 1,958.8 MB | 7.0 MB |

Compressed OCI layer sums compare the platform manifests, not registry storage
or a downloaded image's expanded disk use. The npm install cache cleanup reduced
the candidate by 28.7 MB on amd64 and 26.3 MB on arm64. The largest remaining
content is the Android SDK/NDK; removing it would change Android build support.

## Runtime acceptance

The preceding candidate at commit `6079e03` passed real GitHub-signed jobs on
fresh Linux ARM64 JIT runners in an isolated canary. The final candidate changes
only npm's build-layer cache cleanup; its runner and cache capability files have
the same pinned checksums. Tests covered:

- `actions/cache` cold save and warm restore, `setup-go`, `setup-node` with npm,
  a basic Gradle cache, and the Rust GitHub cache provider, each on cold and
  warm runs against an isolated local cache service.
- BuildKit GitHub Actions cache v2 export and fresh-builder warm import, with
  the warm run retrieving the layer exported by the cold run.
- A two-job workflow with nested composite and container actions, cache
  post-save/restore, and artifact upload/download with a matching payload hash.
- GitHub's official cache path on fresh ARM64 runners with no local endpoint:
  cloud save and restore succeeded. Local-mode keys did not appear in the
  GitHub cache listing.
- The native Go receiver's ARM64 protocol fixture under the race detector.
  This fixture does not represent a signed job against a native receiver on
  an ARM64 host; signed client jobs used an isolated external cache service.

Local-cache startup rejects a missing endpoint or a failed pinned capability
check. With no local endpoint in official-cache mode, the runner retains
GitHub's official cache routing. The acceptance above supports publishing both
architectures of the unified runner image. Production host rollout, its cache
address and disk budget remain separate operational decisions.
