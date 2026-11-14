# Security & Isolation

[English](security.md) | [繁體中文](security.zh-TW.md)

**`dind = true` is Docker-socket sharing, not sandboxed Docker-in-Docker.** Runner containers mount the host's Docker socket, so any job can control the host daemon — start privileged containers, mount the host filesystem, inspect other jobs' containers. That is root-equivalent access to the host. Only run code you trust: **never expose these runners to pull requests from public forks**. Set `dind = false` for scale sets that don't need to build images. Daemon-wide buildx/prune sweepers are off by default and should only be enabled on a Docker daemon dedicated to runners.

Isolation knobs, all per scale set under `[docker]`:

| Key | Effect |
| --- | ------ |
| `memory`, `cpu` | cgroup limits per runner container |
| `pids-limit` | caps processes+threads per container (e.g. `4096`) so a fork bomb can't take down the host |
| `network` | attach runners to a pre-created Docker network instead of the default bridge, e.g. `docker network create --opt com.docker.network.bridge.enable_icc=false runners` |
| `shared-volume-name` | back each scale set's shared volume with a different named volume so scale sets of different trust levels don't share files |
| `dind = false` | no socket mount at all — strongest container-level isolation, no image builds |

The Tart provider isolates at the hypervisor boundary — each job gets a fresh VM cloned from the base image and deleted afterwards.
