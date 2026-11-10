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

| Package | Runner source | Supported execution platforms |
|---|---|---|
| `ghcr.io/ysya/actions-runner` | Official GitHub runner plus toolchains | Linux amd64, arm64 |
| `ghcr.io/ysya/actions-runner-local-cache` | Accepted runner fork plus the same toolchains and capability check | Linux amd64 only |

Both variants share this Dockerfile. The cache variant requires a separately
operated third-party Rust cache service; it is not a GitHub-provided cache server.
Accepted existing cache-v2 workflow steps can stay unchanged when the endpoint
is configured centrally. See the [service setup and semantic limits](../../docs/local-actions-cache.md).

The published v0.17.0 cache image is:

```text
ghcr.io/ysya/actions-runner-local-cache@sha256:ce94d981fbd73f15788bf98af0055d8ccea29a50932484e50c962c67cd45d577
```

Use the digest for deployment. The workflow's source/run/attempt tag is available
for build identification; release-version and stable aliases are not currently
published. The standard package's `latest` tag continues to select the ordinary
image.

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
