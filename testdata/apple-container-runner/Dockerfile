FROM ubuntu:24.04

ARG ACTIONS_RUNNER_VERSION
ARG ACTIONS_RUNNER_SHA256

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends bash ca-certificates curl git libicu74 libunwind8 libssl3 zlib1g \
    && rm -rf /var/lib/apt/lists/* \
    && test -n "$ACTIONS_RUNNER_VERSION" \
    && test -n "$ACTIONS_RUNNER_SHA256" \
    && mkdir -p /opt/actions-runner \
    && curl -fsSL "https://github.com/actions/runner/releases/download/v${ACTIONS_RUNNER_VERSION}/actions-runner-linux-arm64-${ACTIONS_RUNNER_VERSION}.tar.gz" -o /tmp/actions-runner.tar.gz \
    && echo "${ACTIONS_RUNNER_SHA256}  /tmp/actions-runner.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/actions-runner.tar.gz -C /opt/actions-runner \
    && rm /tmp/actions-runner.tar.gz \
    && /opt/actions-runner/bin/installdependencies.sh

COPY apple-runner /usr/local/bin/apple-runner
RUN chmod 755 /usr/local/bin/apple-runner
