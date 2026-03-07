# runscaler

[![Release](https://img.shields.io/github/v/release/ysya/runscaler)](https://github.com/ysya/runscaler/releases)
[![Go Version](https://img.shields.io/github/go-mod/go-version/ysya/runscaler)](https://go.dev)
[![License](https://img.shields.io/github/license/ysya/runscaler)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/ysya/runscaler)](https://goreportcard.com/report/github.com/ysya/runscaler)

Auto-scale GitHub Actions self-hosted runners as Docker containers. Powered by [actions/scaleset](https://github.com/actions/scaleset).

Runners are **ephemeral** — each container handles exactly one job and is removed upon completion. No Kubernetes required.

## How It Works

```
GitHub Actions  ──long poll──▶  runscaler  ──Docker API──▶  Runner Containers
   (job queue)                    (this tool)                          (ephemeral)
```

1. Registers a [runner scale set](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners-with-actions-runner-controller/about-actions-runner-controller) with GitHub
2. Long-polls for job assignments via the scaleset API
3. Spins up Docker containers with JIT (just-in-time) runner configs
4. Removes containers automatically when jobs complete
5. Cleans up all containers and the scale set on shutdown

## Features

- **Zero Kubernetes** — runs directly on any Docker host
- **Ephemeral runners** — each job gets a fresh container, no state leakage
- **Auto-scaling** — scales from 0 to N based on job demand via long-poll (no cron, no polling delay)
- **Docker-in-Docker** — optional DinD support for workflows that build containers
- **Shared volumes** — cross-runner caching via named Docker volumes
- **Multi-org support** — manage multiple scale sets from a single process
- **Single binary** — no runtime dependencies beyond Docker
- **Config file or flags** — TOML config with CLI flag overrides

## Quick Start

### Prerequisites

- Docker running on the host
- A GitHub **Personal Access Token** with `admin:org` scope (for org runners) or `repo` scope (for repo runners)

### Install

**Shell script (Linux/macOS):**

```bash
curl -fsSL https://raw.githubusercontent.com/ysya/runscaler/main/install.sh | sh
```

You can set `INSTALL_DIR` to customize the install location, or `RUNSCALER_VERSION` to pin a version:

```bash
curl -fsSL https://raw.githubusercontent.com/ysya/runscaler/main/install.sh | INSTALL_DIR=./bin sh
```

**Go install:**

```bash
go install github.com/ysya/runscaler@latest
```

**Binary releases:**

Download from [Releases](https://github.com/ysya/runscaler/releases) and add to your `PATH`.

**Docker:**

```bash
docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /path/to/config.toml:/etc/runscaler/config.toml \
  ghcr.io/ysya/runscaler \
  --config /etc/runscaler/config.toml
```

### Run

```bash
# Using a config file (recommended)
runscaler --config config.toml

# Or using CLI flags
runscaler \
  --url https://github.com/your-org \
  --name my-runners \
  --token ghp_xxx \
  --max-runners 10
```

Then in your workflow:

```yaml
jobs:
  build:
    runs-on: my-runners  # matches --name
    steps:
      - uses: actions/checkout@v4
      - run: echo "Running on auto-scaled runner!"
```

## Configuration

Configuration can be provided via a TOML config file (`--config`) or CLI flags. When both are provided, CLI flags take priority over config file values.

### Config File (TOML)

**Single scale set:**

```toml
# config.toml
url = "https://github.com/your-org"
name = "my-runners"
token = "ghp_xxx"
max-runners = 10
min-runners = 0
labels = ["self-hosted", "linux"]
runner-image = "ghcr.io/actions/actions-runner:latest"
runner-group = "default"
docker-socket = "/var/run/docker.sock"
dind = true
shared-volume = "/shared"
log-level = "info"
log-format = "text"
```

**Multiple scale sets (multi-org):**

```toml
# Global settings
docker-socket = "/var/run/docker.sock"
dind = true
shared-volume = "/shared"
runner-image = "ghcr.io/actions/actions-runner:latest"
runner-group = "default"
max-runners = 10
log-level = "info"

# Each [[scaleset]] runs independently.
# runner-image, runner-group, and max-runners inherit from global if omitted.

[[scaleset]]
url = "https://github.com/org-a"
name = "runners-a"
token = "ghp_aaa"
max-runners = 5

[[scaleset]]
url = "https://github.com/org-b"
name = "runners-b"
token = "ghp_bbb"
runner-image = "custom-runner:latest"
labels = ["linux", "gpu"]
```

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config` | | Path to TOML config file |
| `--url` | (required) | Registration URL (org or repo) |
| `--name` | (required) | Scale set name (used as `runs-on` label) |
| `--token` | (required) | GitHub Personal Access Token |
| `--max-runners` | `10` | Maximum concurrent runners |
| `--min-runners` | `0` | Minimum runners to keep warm |
| `--labels` | `<name>` | Runner labels (comma-separated) |
| `--runner-group` | `default` | Runner group name |
| `--runner-image` | `ghcr.io/actions/actions-runner:latest` | Docker image |
| `--docker-socket` | `/var/run/docker.sock` | Docker socket path |
| `--dind` | `true` | Mount Docker socket into runners (Docker-in-Docker) |
| `--shared-volume` | | Shared Docker volume path in runners (e.g. `/shared`) |
| `--log-level` | `info` | Log level (debug/info/warn/error) |
| `--log-format` | `text` | Log format (text/json) |

## Deployment

### Systemd

```ini
[Unit]
Description=GitHub Actions Runner Auto-Scaler
After=docker.service
Requires=docker.service

[Service]
Type=simple
ExecStart=/usr/local/bin/runscaler --config /etc/runscaler/config.toml
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=multi-user.target
```

### Docker Compose

```yaml
services:
  scaler:
    image: ghcr.io/ysya/runscaler
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./config.toml:/etc/runscaler/config.toml:ro
    command: ["--config", "/etc/runscaler/config.toml"]
```

## Building

```bash
# Current platform
make build

# All platforms (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64)
make all

# Docker image
docker build -t runscaler .
```

## Architecture

Built on top of [actions/scaleset](https://github.com/actions/scaleset), the official Go client library for GitHub Actions Runner Scale Sets.

Key components:

- **`main.go`** — CLI entry point, initialization, and graceful shutdown
- **`config.go`** — Configuration management with Viper (flags + TOML config file)
- **`scaler.go`** — Implements `listener.Scaler` interface for Docker container lifecycle

The scaler implements three methods from the scaleset `Scaler` interface:

- `HandleDesiredRunnerCount` — Scales up containers to match job demand
- `HandleJobStarted` — Marks runners as busy
- `HandleJobCompleted` — Removes finished containers

## License

MIT
