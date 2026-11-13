# Custom GitHub Actions runner image with Android SDK, Java, Node.js, Ruby, and C/C++ toolchains.
# Based on the pinned cache-capable runner fork (Ubuntu 24.04 "noble").

# Both platforms use the same reviewed fork source and fail-closed cache patch.
# The SDK is a build stage only; it is not included in the published image.
ARG RUNNER_BASE_IMAGE=ghcr.io/falcondev-oss/actions-runner@sha256:462ebdbc95fabf0bdf264ffe02ff5182665c6d99ac0f870b67d7f649c1ef1746
ARG WORKER_SDK_IMAGE=mcr.microsoft.com/dotnet/sdk:8.0.428-noble@sha256:3d5cad4bb76fd0130aefd6c629f7267c4aad40cccc86f7fbd1c81e277b58493b

FROM ${WORKER_SDK_IMAGE} AS cache-worker
ARG TARGETARCH
WORKDIR /src
RUN test "$TARGETARCH" = amd64 -o "$TARGETARCH" = arm64 \
    && git init -q . \
    && git remote add origin https://github.com/falcondev-oss/github-actions-runner.git \
    && git fetch --depth=1 origin 3efdc9c0d8a5b7a441cba3069b0fdc33d1931adb \
    && git checkout --detach FETCH_HEAD \
    && test "$(git rev-parse HEAD)" = 3efdc9c0d8a5b7a441cba3069b0fdc33d1931adb
COPY fail-closed-v1.patch webutilities-audit.patch /tmp/
RUN printf '%s  %s\n' \
        5c801dea0cbed7265cfa0e716d2f126ae1ed934c96000046cf89c89b701ccde0 /tmp/fail-closed-v1.patch \
        f258207452fd3a3106d9fa0e2cac397ba76ea8c40e88206c84ff3ad8ff2013cc /tmp/webutilities-audit.patch \
        | sha256sum --check \
    && git apply --unidiff-zero /tmp/fail-closed-v1.patch \
    && git apply --unidiff-zero /tmp/webutilities-audit.patch \
    && git diff --check \
    && dotnet publish src/Runner.Worker/Runner.Worker.csproj \
        -c Release -r "linux-${TARGETARCH}" \
        -p:PackageRuntime="linux-${TARGETARCH}" -p:Version=2.338.2 \
        -o /worker --verbosity minimal

FROM ${RUNNER_BASE_IMAGE} AS runner

# Avoid interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# ── System packages: C/C++ toolchains, Ruby, utilities ───────────────────────
RUN sudo apt-get update && sudo apt-get install -y --no-install-recommends \
        build-essential \
        gcc \
        g++ \
        gfortran \
        clang \
        ruby-full \
        curl \
        wget \
        unzip \
        zip \
        git \
        ca-certificates \
        gnupg \
        software-properties-common \
        libssl-dev \
        zlib1g-dev \
    && sudo rm -rf /var/lib/apt/lists/*

# ── Java 17 LTS (OpenJDK) ─────────────────────────────────────────────────────
# Must precede the Android SDK layer: sdkmanager requires a JDK.
RUN sudo apt-get update \
    && sudo apt-get install -y --no-install-recommends openjdk-17-jdk \
    && sudo rm -rf /var/lib/apt/lists/* \
    && ARCH=$(dpkg --print-architecture) \
    && sudo ln -s /usr/lib/jvm/java-17-openjdk-${ARCH} /usr/lib/jvm/java-17-openjdk
ENV JAVA_HOME=/usr/lib/jvm/java-17-openjdk
ENV PATH="${JAVA_HOME}/bin:${PATH}"

# ── Android SDK (command-line tools only) ─────────────────────────────────────
# Install under runner's home so all runtime operations (Gradle auto-provisioning,
# sdkmanager updates, license acceptance) work without sudo or chown.
# Ordered before Node.js/pnpm so bumping NODE_MAJOR does not invalidate this
# multi-GB layer in the build cache.
ENV ANDROID_HOME=/home/runner/android-sdk
ENV PATH="${ANDROID_HOME}/cmdline-tools/latest/bin:${ANDROID_HOME}/platform-tools:${PATH}"

ARG ANDROID_CMDLINE_TOOLS_VERSION=14742923
RUN mkdir -p ${ANDROID_HOME}/cmdline-tools \
    && wget -qO /tmp/cmdline-tools.zip \
       "https://dl.google.com/android/repository/commandlinetools-linux-${ANDROID_CMDLINE_TOOLS_VERSION}_latest.zip" \
    && unzip -q /tmp/cmdline-tools.zip -d ${ANDROID_HOME}/cmdline-tools \
    && mv ${ANDROID_HOME}/cmdline-tools/cmdline-tools ${ANDROID_HOME}/cmdline-tools/latest \
    && rm /tmp/cmdline-tools.zip \
    && yes | ${ANDROID_HOME}/cmdline-tools/latest/bin/sdkmanager --licenses 2>&1 | tail -1 \
    && ${ANDROID_HOME}/cmdline-tools/latest/bin/sdkmanager \
       "platform-tools" \
       "platforms;android-36" \
       "build-tools;36.0.0" \
       "ndk;27.1.12297006" \
       "cmake;3.22.1"

# ── Multi-arch: amd64 libs for x86_64-only tools (e.g. Android AGP aapt2) ────
# Google still ships aapt2 as a single linux-x64 binary in maven (artifact
# com.android.tools.build:aapt2:<ver>:linux). On arm64 hosts the dynamic loader
# /lib64/ld-linux-x86-64.so.2 is missing, so AGP processDebugResources fails.
# Install amd64 glibc/libstdc++/zlib so OrbStack Rosetta (or qemu) can run them.
ARG TARGETARCH
RUN if [ "$TARGETARCH" = "arm64" ]; then \
        sudo sed -i 's|^URIs:|Architectures: arm64\nURIs:|' /etc/apt/sources.list.d/ubuntu.sources \
        && printf 'Types: deb\nURIs: http://archive.ubuntu.com/ubuntu\nSuites: noble noble-updates noble-security\nComponents: main restricted universe multiverse\nArchitectures: amd64\nSigned-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\n' | sudo tee /etc/apt/sources.list.d/amd64.sources > /dev/null \
        && sudo dpkg --add-architecture amd64 \
        && sudo apt-get update \
        && sudo apt-get install -y --no-install-recommends \
            libc6:amd64 \
            libstdc++6:amd64 \
            zlib1g:amd64 \
        && sudo rm -rf /var/lib/apt/lists/*; \
    fi

# ── Node.js LTS (via NodeSource) ─────────────────────────────────────────────
ARG NODE_MAJOR=24
RUN curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | sudo -E bash - \
    && sudo apt-get install -y --no-install-recommends nodejs \
    && sudo rm -rf /var/lib/apt/lists/*

# ── pnpm ──────────────────────────────────────────────────────────────────────
RUN sudo npm install -g pnpm \
    && sudo npm cache clean --force

# ── Temp directory owned by runner ─────────────────────────────────────────
# Redirect all temp file creation to a runner-owned directory so that any
# tool (Gradle, EAS CLI, npx, etc.) can freely create subdirectories
# without hitting permission errors on /tmp.
RUN mkdir -p /home/runner/tmp
ENV TMPDIR=/home/runner/tmp
ENV TEMP=/home/runner/tmp
ENV TMP=/home/runner/tmp

# ── Verify installations ─────────────────────────────────────────────────────
RUN set -x \
    && gcc --version | head -1 \
    && g++ --version | head -1 \
    && gfortran --version | head -1 \
    && clang --version | head -1 \
    && java -version 2>&1 | head -1 \
    && node --version \
    && pnpm --version \
    && ruby --version \
    && sdkmanager --version 2>&1 | head -1

# Install GitHub CLI and the audited Worker overlay. The fork's original
# Listener keeps its upstream cache endpoint behavior when local cache is off.
USER root
RUN mkdir -p -m 0755 /etc/apt/keyrings \
    && curl -fsSL -o /tmp/githubcli-archive-keyring.gpg https://cli.github.com/packages/githubcli-archive-keyring.gpg \
    && echo '6084d5d7bd8e288441e0e94fc6275570895da18e6751f70f057485dc2d1a811b  /tmp/githubcli-archive-keyring.gpg' | sha256sum --check \
    && install -m 0644 /tmp/githubcli-archive-keyring.gpg /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && printf 'deb [arch=%s signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main\n' "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/* /tmp/githubcli-archive-keyring.gpg
COPY --chown=1001:123 --from=cache-worker /worker/Runner.Worker.dll /worker/Runner.Worker.deps.json /worker/Microsoft.AspNetCore.WebUtilities.dll /home/runner/bin/
COPY --chmod=0755 check-unified-cache.sh /usr/local/bin/runner-local-cache-check
RUN mkdir -p /usr/local/share/runscaler \
    && /usr/local/bin/runner-local-cache-check --emit-manifest > /usr/local/share/runscaler/local-actions-cache.manifest \
    && chmod 0644 /usr/local/share/runscaler/local-actions-cache.manifest
USER runner
RUN gh --version \
    && gh api --help | grep -q -- '--slurp' \
    && CUSTOM_ACTIONS_RESULTS_URL=http://offline-cache.invalid:9/ /usr/local/bin/runner-local-cache-check \
    && /home/runner/run.sh --version | grep -x 2.338.2 >/dev/null
