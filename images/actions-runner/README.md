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

### With runscaler

```toml
[[scaleset]]
runner-image = "ghcr.io/ysya/actions-runner:latest"
```

### Standalone (Docker)

```bash
docker pull ghcr.io/ysya/actions-runner:latest
```

## Architectures

`linux/amd64` and `linux/arm64` are both supported via multi-platform build.

## Environment variables

| Variable | Value |
|----------|-------|
| `JAVA_HOME` | `/usr/lib/jvm/java-17-openjdk` |
| `ANDROID_HOME` | `/opt/android-sdk` |
| `ANDROID_SDK_ROOT` | `/opt/android-sdk` |

## Installing additional Android SDK components

The image includes `sdkmanager` on `PATH`. You can install additional components in your workflow:

```yaml
- run: sdkmanager "ndk;27.0.12077973"
```

## Building locally

```bash
docker build -t actions-runner:latest images/actions-runner/
```
