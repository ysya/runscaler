# actions-runner

The `ghcr.io/ysya/actions-runner:latest` image provides one Linux amd64/arm64 runner for GitHub's official Actions cache and Runscaler's native local Actions cache. The local cache is selected per scale set with `native-actions-cache = true`; otherwise the runner uses GitHub's cache URLs. A selected local cache fails closed if its endpoint is unavailable. Artifact traffic still uses GitHub Results passthrough.

The image starts from GitHub's pinned official runner 2.338.2 image and builds its Worker and Listener from the matching official source commit. The reviewed patch overrides cache and Results URLs only when Runscaler selects local cache, and prevents a runner self-update from replacing the patched binaries in that mode. With no local endpoint, GitHub's cache URLs and normal update behavior remain in use. The [earlier fork compatibility evidence](../../docs/local-actions-cache-compatibility.md#runner-fork) describes the previously published image; it does not validate this official-base build.

## Installed tools

| Category | Contents |
|---|---|
| Runner | Pinned GitHub runner 2.338.2, reviewed local-cache Worker and Listener patch, GitHub CLI `gh` |
| Android | Command-line tools, platform-tools, Android 36, build-tools 36.0.0, NDK 27.1.12297006, CMake 3.22.1 |
| Java | OpenJDK 17 |
| JavaScript | Node.js 24, pnpm, runner-bundled Node 20 and 24 |
| Native and scripting | GCC, G++, gfortran, Clang, Ruby, Git, Docker CLI, Buildx |

The arm64 image also includes amd64 glibc, libstdc++, and zlib for Android tools distributed only as x86-64 binaries.

The Android SDK and NDK dominate image size. On the previous amd64 image, the SDK directory was about 2.56 GB uncompressed, including about 2.05 GB of NDK. The earlier fork-based unified image had 2,074.1 MB of compressed OCI layers on amd64 and 1,958.8 MB on arm64. Clearing the npm install cache in its build layer removed about 28.7 MB and 26.3 MB, respectively. These figures are historical; measure the official-base image separately. Removing the NDK or other build toolchains would change supported workflows; this build keeps them.

## Use

```toml
[[scaleset]]
runner-image = "ghcr.io/ysya/actions-runner:latest"
# Enable this only for a scale set with a configured native local cache.
# native-actions-cache = true
```

Runscaler injects `CUSTOM_ACTIONS_RESULTS_URL` only for local-cache scale sets. The image contains `/usr/local/bin/runner-local-cache-check` and a platform-specific capability manifest. Runscaler checks both before starting the listener when local cache is enabled. Do not set `CUSTOM_ACTIONS_RESULTS_URL` manually for ordinary GitHub cache jobs.

## Build and verification

```bash
docker build --platform linux/arm64 -t actions-runner:check images/actions-runner
```

The Dockerfile pins the multiarchitecture official base and .NET SDK build stage. It fetches the exact official source commit, verifies and applies `official-local-cache.patch`, and updates the vulnerable transitive `Microsoft.AspNetCore.WebUtilities` dependency to 8.0.34 with `webutilities-audit.patch`. Only the resulting Worker and Listener assemblies, the Worker's dependency manifest, and the updated WebUtilities assembly enter the final image; the SDK build stage is discarded. The installed checker pins those binaries, patches, platform, and base identity.

The `Build Runner Image` workflow builds both architectures on pull requests and publishes the multiarchitecture `latest` tag from `main`. The previous separate local-cache package has been removed; use `ghcr.io/ysya/actions-runner:latest` for both cache modes, or pin a verified digest of that package for a canary. The [unified runner compatibility report](../../docs/unified-runner-compatibility.md) records signed-job acceptance for the earlier fork-based image; the official-base image requires its own runtime acceptance before a host switches to it.

See the [original amd64 compatibility evidence](../../docs/local-actions-cache-compatibility.md) and [operator setup](../../docs/local-actions-cache.md) for local-cache behavior.
