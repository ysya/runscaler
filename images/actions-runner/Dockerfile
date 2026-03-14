# Custom GitHub Actions runner image with Android SDK, Java, Node.js, Ruby, and C/C++ toolchains.
# Based on the official actions-runner image (Ubuntu 22.04).

FROM ghcr.io/actions/actions-runner:latest

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
        lsb-release \
        software-properties-common \
        libssl-dev \
        zlib1g-dev \
    && sudo rm -rf /var/lib/apt/lists/*

# ── Java 21 LTS (OpenJDK) ─────────────────────────────────────────────────────
RUN sudo apt-get update \
    && sudo apt-get install -y --no-install-recommends openjdk-21-jdk \
    && sudo rm -rf /var/lib/apt/lists/* \
    && ARCH=$(dpkg --print-architecture) \
    && sudo ln -s /usr/lib/jvm/java-21-openjdk-${ARCH} /usr/lib/jvm/java-21-openjdk
ENV JAVA_HOME=/usr/lib/jvm/java-21-openjdk
ENV PATH="${JAVA_HOME}/bin:${PATH}"

# ── Node.js LTS (via NodeSource) ─────────────────────────────────────────────
ARG NODE_MAJOR=24
RUN curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | sudo -E bash - \
    && sudo apt-get install -y --no-install-recommends nodejs \
    && sudo rm -rf /var/lib/apt/lists/*

# ── pnpm ──────────────────────────────────────────────────────────────────────
RUN sudo npm install -g pnpm

# ── Android SDK (command-line tools only) ─────────────────────────────────────
ENV ANDROID_HOME=/opt/android-sdk
ENV ANDROID_SDK_ROOT=${ANDROID_HOME}
ENV PATH="${ANDROID_HOME}/cmdline-tools/latest/bin:${ANDROID_HOME}/platform-tools:${PATH}"

ARG ANDROID_CMDLINE_TOOLS_VERSION=14742923
RUN sudo mkdir -p ${ANDROID_HOME}/cmdline-tools \
    && wget -qO /tmp/cmdline-tools.zip \
       "https://dl.google.com/android/repository/commandlinetools-linux-${ANDROID_CMDLINE_TOOLS_VERSION}_latest.zip" \
    && sudo unzip -q /tmp/cmdline-tools.zip -d ${ANDROID_HOME}/cmdline-tools \
    && sudo mv ${ANDROID_HOME}/cmdline-tools/cmdline-tools ${ANDROID_HOME}/cmdline-tools/latest \
    && rm /tmp/cmdline-tools.zip \
    && yes | sudo ${ANDROID_HOME}/cmdline-tools/latest/bin/sdkmanager --licenses > /dev/null 2>&1 \
    && sudo ${ANDROID_HOME}/cmdline-tools/latest/bin/sdkmanager \
       "platform-tools" \
       "platforms;android-36" \
       "build-tools;36.0.0" \
    && sudo chmod -R a+rX ${ANDROID_HOME}

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
