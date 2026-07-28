# Custom GitHub Actions runner image with Android SDK, Java, Node.js, Ruby, and C/C++ toolchains.
# Based on the official actions-runner image (Ubuntu 24.04 "noble").

# Pinned to the multi-arch index digest for reproducible builds and stable layer caching
# (covers both linux/amd64 and linux/arm64). Dependabot keeps the tag+digest fresh.
# Refresh manually with: docker buildx imagetools inspect ghcr.io/actions/actions-runner:latest
FROM ghcr.io/actions/actions-runner:2.336.0@sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda

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
RUN sudo npm install -g pnpm

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
