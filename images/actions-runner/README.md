# actions-runner

Custom GitHub Actions self-hosted runner image based on [`ghcr.io/actions/actions-runner`](https://github.com/actions/runner/pkgs/container/actions-runner), with Android SDK, Java, Node.js, and C/C++ toolchains pre-installed.

## Pre-installed tools

| Category | Tools |
|----------|-------|
| **Java** | OpenJDK 17 LTS |
| **Android** | SDK command-line tools, platform-tools, Android 36 (build-tools 36.0.0) |
| **JavaScript** | Node.js 24 LTS, pnpm |
| **Ruby** | ruby-full |
| **C/C++** | GCC, G++, Clang |
| **Fortran** | GNU Fortran |

## Usage

### With runner

```toml
[[scaleset]]
runner-image = "ghcr.io/ysya/actions-runner:latest"
```

### Standalone (Docker)

```bash
docker pull ghcr.io/ysya/actions-runner:latest
```

## Architectures

`linux/amd64` and `linux/arm64` are both supported by the standard image.
The opt-in local Actions cache variant currently supports **linux/amd64 only**.

## Environment variables

| Variable | Value |
|----------|-------|
| `JAVA_HOME` | `/usr/lib/jvm/java-17-openjdk` |
| `ANDROID_HOME` | `/home/runner/android-sdk` |
| `TMPDIR` | `/home/runner/tmp` |

## Installing additional Android SDK components

The image includes `sdkmanager` on `PATH`. You can install additional components in your workflow:

```yaml
- run: sdkmanager "ndk;27.0.12077973"
```

## Building locally

```bash
docker build -t actions-runner:latest images/actions-runner/
```

## Opt-in host-local Actions cache variant

The standard build still defaults to the official digest-pinned runner and
`LOCAL_ACTIONS_CACHE=0`. It does not contain a valid local-cache capability
manifest or installed checker. The optional cache variant uses the separately
accepted fork digest; [compatibility evidence](../../docs/local-actions-cache-compatibility.md)
records the actual source/binary differences and supported client revisions.

`RUNNER_BASE_IMAGE` selects the base. `LOCAL_ACTIONS_CACHE=1` accepts only the
specific gate-approved base and assembly checksums; a stock/arbitrary base fails
the independent `local-cache-contract` stage. Its manifest and checker are copied
into the final image after the existing toolchain layers. Modified assemblies,
missing metadata or a missing endpoint fail before the listener starts. The
checker makes no network request and does not require a healthy server.

The manual **Build Local Cache Runner Image** workflow validates without
publishing by default. Explicit `publish=true` writes only the separate
`actions-runner-local-cache` package, a unique source/run/attempt/platform tag,
and its own registry build cache. It never replaces the standard `latest` tag.
Image construction remains GitHub-hosted; the registry build cache is separate
from self-hosted job caches. Binary caching in setup-buildx and build-record
artifact upload are disabled in this workflow.

Inspect the published digest/attestation before using it in `runner-image`.
Do not configure the bare fork base as the final Runscaler cache image. Only the
verified variant installs `/usr/local/bin/runner-local-cache-check` and
`/usr/local/share/runscaler/local-actions-cache.manifest`.

The fork blocks runner self-update while `CUSTOM_ACTIONS_RESULTS_URL` is set.
For runner upgrades, repeat the compatibility gate and update the base, accepted
assembly hashes and manifest together. Do not bless a new version by generating
fresh hashes from unreviewed binaries. Linux arm64 remains gated independently.
See [operator setup and rollback](../../docs/local-actions-cache.md).

## Native v2 fail-closed variant

`Dockerfile.fail-closed` adds a reviewed Worker assembly to the previously
verified amd64 local-cache image, without rebuilding the toolchain. The Worker
comes from the exact fork source revision and
[`fail-closed-v1.patch`](fail-closed-v1.patch), which changes both Node and
container action handlers to set `ACTIONS_CACHE_URL` to the local Results URL
when `CUSTOM_ACTIONS_RESULTS_URL` is present. The native receiver rejects the
legacy `/_apis/artifactcache/` API locally. The original GitHub cache URL is
retained only for runners without a custom local endpoint.

The dedicated [CI workflow](../../.github/workflows/runner-worker-fail-closed.yml)
builds and tests the pinned source, checks the patch and Worker SHA-256, then
publishes a run-specific `actions-runner-local-cache` tag and registry
attestation. Its image checker pins the new Worker, the existing Listener and
the parent image. Use only the inspected immutable image digest with native v2
mode; the earlier image does not enforce local-only legacy cache routing.
Review the private signed-job acceptance record for the verified digest and
scoped client results before rollout.

## GitHub CLI overlay

`Dockerfile.github-cli` adds the Ubuntu `gh` package to the verified native v2
fail-closed image without rebuilding its Worker. The
[GitHub CLI image workflow](../../.github/workflows/runner-image-github-cli.yml)
builds and checks the image on any manual dispatch. Dispatch from `main` with
`publish=true` to publish an immutable digest and attestation. Pin the inspected
digest in the Runscaler scale set configuration.
