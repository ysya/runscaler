# Configuration

[English](configuration.md) | [繁體中文](configuration.zh-TW.md)

Configuration can be provided via a TOML config file (`--config`) or CLI flags. When both are provided, CLI flags take priority over config file values.

Unknown config keys (typos, options from another version) are not silently
ignored: `runner run` logs a warning for each and keeps starting, while
`runner validate` fails on them. The same applies to single-mode keys
(`url`, `name`, `token`, `github-app`, `labels`, `min-runners`) left at the top level
when `[[scaleset]]` entries exist.

<a id="github-credentials-and-pat-permissions"></a>

## GitHub credentials and PAT permissions

Use a [GitHub App](#github-app-authentication) or Personal Access Token
(PAT) for the target organization or repository. PAT permissions depend on
the token type and runner level:

| Token type | Organization runners | Repository runners |
| --- | --- | --- |
| Classic PAT | `admin:org` | `repo` |
| Fine-grained PAT | Self-hosted runners: **Read and write** + Administration: **Read** | Administration: **Read and write** |

The token owner must be an organization owner for organization runners, or
have admin access to the repository for repository runners. Fine-grained PATs
targeting an organization may also require
[administrator approval](https://docs.github.com/en/organizations/managing-programmatic-access-to-your-organization/setting-a-personal-access-token-policy-for-your-organization),
depending on organization policy.

## Config File (TOML)

**Docker provider (default):**

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
log-level = "info"
log-format = "text"
concurrent = 10                          # host-wide across all scale sets; 0 = automatic sum
# log-file = "/var/log/runner/runner.log" # default: /var/log/runner/runner.log (root), ~/.local/state/runner/runner.log (Linux), ~/Library/Logs/runner/runner.log (macOS); "" disables
health-address = "127.0.0.1"              # localhost-only by default
health-port = 8080
# drain-timeout = "2h"                    # 0s disables graceful drain

[docker]
socket = "/var/run/docker.sock"
dind = true
# workspace-root = "/home/runscaler/runner-workspaces" # Linux: private host directory for runner Docker socket proxies
shared-volume = "/shared"
# shared-volume-max-age = "168h"        # delete shared-volume files older than this; unset = never reclaimed
# buildx-cleanup = true                 # opt in only on a runner-dedicated daemon
# buildx-cleanup-ttl = "24h"            # remove buildx builders older than this
# buildx-cleanup-interval = "6h"        # how often the buildx sweep runs
# prune = true                          # opt in only on a runner-dedicated daemon
# prune-interval = "6h"                 # how often the runtime prune sweep runs
# prune-ttl = "24h"                     # remove stopped containers and dangling images older than this
# build-cache-max-age = "168h"          # prune build cache entries not used within this window
# build-cache-budget = 0                # cap daemon build cache to N GB (0 = no cap)
```

On a Linux host using the Docker socket, a container action's workspace bind
otherwise resolves on the host instead of inside its runner container. To
support those actions, create `workspace-root` as a `0700` directory owned by
the manager user, then set it in the applicable `[docker]` or
`[scaleset.docker]` table. Each runner gets its own local Docker volume mounted
at `/home/runner/_work` and a Unix socket proxy under `workspace-root`. The
proxy translates container bind sources beneath `/home/runner/_work` to that
volume's daemon-side mountpoint. The path inside the runner stays stable for
cache clients. Runner cleanup removes workspace-bound child containers and the
volume after the job completes or is cancelled. After a manager crash, startup
reclaims unused workspace volumes and socket directories while retaining any
volume still mounted by a Docker container. This option requires `dind = true`.

When runners build images with `docker buildx` (e.g. via
`docker/setup-buildx-action`), each run can leave behind a BuildKit builder
container plus a multi-GB `buildx_buildkit_*_state` volume. On a persistent host
sharing one Docker daemon these accumulate until the disk fills. On a daemon
dedicated to runners, opt in with `buildx-cleanup = true`; runner then removes
builders older than `buildx-cleanup-ttl`. Do not enable it when persistent
builders or unrelated workloads share the daemon.

Beyond buildx builders, jobs mounting the host Docker socket leave dangling
images, stopped containers, and daemon build cache behind on every build. The
runtime prune sweep (opt in with `prune = true`) reclaims these on a timer while
runner is up: stopped containers and dangling images older than `prune-ttl`,
build cache not used within `build-cache-max-age`, and — with
`build-cache-budget` set — build cache above the GB cap. It assumes the daemon
is dedicated to runners: matching objects are treated as job garbage
regardless of what created them. Keep it disabled on a shared daemon.

`prune`/`buildx-cleanup` being off only turns off *this* daemon's routine
sweep — the host-wide disk guard (see [Caching](caching.md)) still reclaims
through both once a filesystem is actually under pressure, regardless of
these switches.

**Tart provider (macOS):**

```toml
# config.toml
provider = "tart"
url = "https://github.com/your-org"
name = "macos-runners"
token = "ghp_xxx"
max-runners = 2          # Apple limits 2 concurrent macOS VMs per host
runner-image = "ghcr.io/cirruslabs/macos-golden-gate-xcode:latest"
labels = ["self-hosted", "macOS"]
log-level = "info"

[tart]
cpu = 4                  # CPU cores per VM (0 = use image default)
memory = 8192            # Memory in MB per VM (0 = use image default)
runner-dir = "/Users/admin/actions-runner"  # default
pool-size = 2            # pre-warm 2 VMs for instant job pickup (~2s vs ~30s cold boot)
# home = "/Volumes/Data/tart"          # TART_HOME for the tart CLI ("" = ~/.tart)
# cache-budget = 80                    # cap OCI/IPSW cache to N GB via `tart prune` (0 = disabled)
# cache-cleanup-interval = "24h"       # how often the prune sweep runs
# mtu = 0                              # guest MTU: 0 = match a VPN tunnel (default), -1 = off, 1280–1500 = fixed
# dns = ["1.1.1.1", "8.8.8.8"]         # guest resolvers ([] = keep the one Tart's NAT hands out)
# net-bridged = "en0"                  # bridge VMs to this host interface instead of Tart's NAT
```

With `home` set, point your own tart commands at the same directory —
`TART_HOME=/Volumes/Data/tart tart list`. A bare `tart list` reads `~/.tart`
and shows none of runner's images or VMs.

Xcode VM images are huge (50–80 GB each) and `:latest` tags accumulate old
layers under `$TART_HOME/cache/` — set `cache-budget` to keep it bounded.
The sweeper only touches OCI/IPSW caches, never your local VMs. Before every
prune it marks each Tart scale set's `runner-image` as just used, so a week
without jobs no longer evicts the image the next job clones from, and under
`cache-budget` that image is evicted last.

**Hosts behind a VPN (Cloudflare WARP, Tailscale exit nodes, corporate
VPNs).** A tunnel that cannot carry 1500-byte packets breaks the VMs while the
host itself keeps working: the guest advertises a full-size TCP MSS, servers
answer with 1500-byte packets the tunnel drops, and every TLS handshake from
the VM stalls — the runner starts but never connects, so jobs stay queued. By
default (`mtu = 0`) runner looks up the host's route to the scale set's GitHub
host before each runner starts and, when that route's MTU is below 1500, sets
the guest's MTU to match (`sudo -n ifconfig`, which the Cirrus Labs images
allow). Set a fixed `mtu` if the tunnel only carries some destinations, or
`-1` to leave guests alone. Tart's NAT also proxies DNS through the host, and
that proxy can stop answering after the host's VPN disconnects; set `dns` to
give the guests resolvers of their own.

**Choosing Tart networking (v0.12.0+).** Shared NAT remains the default. If
your VMs already obtain an address and connect to GitHub, keep `net-bridged`
unset or empty; upgrading does not require a networking change. NAT keeps
the VMs on the host's private network rather than giving each VM an address
on your LAN.

**Hosts whose NAT does not serve guests.** Tart's shared NAT depends on the
host's `bootpd` to hand each guest a lease. When it does not, the guest comes
up on a link-local `169.254.x.x` address with no route out, the runner never
reaches GitHub, and jobs stay queued. Set `net-bridged` to a host interface
(`net-bridged = "en0"`, the BSD device name `ifconfig` shows for the active
Ethernet or Wi-Fi) and every VM boots with `tart run --net-bridged=<interface>`, taking
its address from the LAN's DHCP server instead. Bridging connects each VM
directly to that LAN; it does not create a private runner network. The image
must leave the guest's network service on DHCP. The CLI equivalent is
`runner run --config config.toml --tart-net-bridged en0`. In multi-scale-set
configs, `[tart] net-bridged` is inherited; set `[scaleset.tart] net-bridged = ""`
to return an individual scale set to NAT.

Bridged guests do not leave through the host's
routing table, so `mtu = 0` no longer matches a host tunnel for them; a fixed
`mtu` and `dns` still apply. Bridged VM MAC addresses are derived from the host
interface's hardware address and the shared host slot, rather than reusing
the same fixed slot addresses on every host. The address stays stable for
the same interface hardware address and slot, allowing DHCP lease reuse;
changing the host interface's hardware address changes the VM addresses.
Startup fails and the clone is cleaned up if its bridged MAC cannot be read or
written safely. The DHCP server still controls IP allocation: this MAC policy
does not guarantee a fixed IP or resolve duplicated DHCP client identifiers
inside cloned images. Before relying on a bridged deployment, verify that
two concurrent VMs obtain distinct addresses and complete jobs after repeated
VM creation and removal; include multiple Mac hosts if they share the LAN.

**Docker networking.** These Tart settings do not apply to Docker runners.
They use Docker's default bridge unless `[docker] network` selects another
existing network. Docker manages address allocation; a dedicated Docker
bridge can already be selected with that setting (see
[Security & Isolation](security.md)).

**Runner start-up check.** A runner that dies or never reaches GitHub leaves
its VM running, so runner hands a VM over only once the runner inside prints
`Listening for Jobs` and is still alive a few seconds later — the moment
GitHub refuses a retired runner version. If it exits first, or has not
connected within 5 minutes, runner deletes the VM, logs `Failed to start
runner` with the runner's own output (or the tail of its `_diag` log), and
retries.

**Custom VM images.** `runner-image` accepts a local VM name as well as an
OCI reference: startup checks `tart list` first and only pulls when the name
is absent. To bake project dependencies into the image (simulator runtimes,
CocoaPods/SPM caches, extra toolchains), clone a Cirrus Labs image, provision
it, stop it, and point `runner-image` at the result:

```bash
tart clone ghcr.io/cirruslabs/macos-golden-gate-xcode:latest ios-golden
tart run ios-golden --no-graphics &
tart exec -it ios-golden zsh      # runs as admin; install tools, warm caches
tart stop ios-golden
```

```toml
runner-image = "ios-golden"       # local VM — no registry needed
```

Every job still boots a fresh copy-on-write clone; runner never starts the
golden VM itself. Share it across hosts with `tart login ghcr.io` followed by
`tart push ios-golden ghcr.io/<org>/ios-golden:latest`, or make the build
reproducible with the
[Packer tart builder](https://github.com/openai/tart/blob/main/docs/integrations/packer.md)
and [macos-image-templates](https://github.com/cirruslabs/macos-image-templates).
Whatever the base, the image must keep the
[Tart Guest Agent](https://github.com/openai/tart/blob/main/docs/blog/posts/2025-06-01-tart-guest-agent.md)
running — runner drives VMs through `tart exec`, not SSH, and every
non-vanilla Cirrus Labs image already ships it — and must have the GitHub
Actions runner unpacked at `runner-dir` with an executable `run.sh`.
Provisioning the golden VM occupies one of the host's two macOS VM slots, so
do it while runner is not at capacity.

**Runner version retirement.** GitHub retires runner versions server-side:
each release gets a `runtime_deprecates_at` date some weeks after newer
releases ship, and a runner still on a retired version is refused on its first
poll for messages with
`Runner version vX.Y.Z is deprecated and cannot receive messages`, and exits.
The scale set still shows Online while every runner it starts dies, so jobs
stay queued; on Tart the start-up check above logs the refusal. Every
ephemeral container or VM starts at the runner version baked into its image,
and `disable-update` cannot change that: the update instruction arrives on the
same message poll GitHub refuses (see
[community discussion #206494](https://github.com/orgs/community/discussions/206494)),
so `disable-update = false` only makes a still-supported runner download the
latest release before its first job — it does not rescue a retired one. That
is why runner defaults to `disable-update = true` and the runner is refreshed
with the image instead: rebuild the container image, or replace
`actions-runner` in your golden VM. To see how long a version has left:

```bash
gh api repos/<owner>/<repo>/actions/runners/deprecations/<version>   # runtime_deprecates_at
```

The org-level endpoint (`orgs/<org>/actions/runners/deprecations/<version>`)
needs the `admin:org` scope.

## Host-local Actions cache (opt-in)

For the built-in Go receiver, set `[scaleset.docker] native-actions-cache = true`.
The manager uses `[actions-cache].advertise-url` for that scale set. The default
is false, and an inherited true value can be overridden with false. The existing
`actions-cache-url` remains available for an explicit endpoint, including an
external receiver. An inherited or explicit URL alongside `native-actions-cache = true` must
equal the global advertised URL. On Linux amd64, opt in per scale set with an
accepted cache-capable runner variant by immutable digest.
When an old explicit URL points to the built-in receiver, Runscaler emits an
advisory sunset warning. Remove that duplicate URL after enabling the new
switch; external receiver URLs are not deprecated.
A stock runner or the bare fork base image is insufficient: the final variant
must provide Runscaler's verified capability checker before accepting work.

The endpoint must be an absolute HTTP(S) URL with a host and literal trailing
slash. Credentials, queries and fragments are rejected without echoing their
values in validation errors. Runscaler does not repair or normalize an invalid URL.
The actual runner and sibling BuildKit must both reach the advertised endpoint.

```toml
[[scaleset]]
url = "https://github.com/<owner>"
name = "runscaler-local-cache-canary"
labels = ["runscaler-local-cache-canary"]
token = "env:RUNNER_TOKEN"
max-runners = 1
runner-image = "<verified-local-cache-image>@sha256:<accepted-digest>"

[scaleset.docker]
platform = "linux/amd64"
native-actions-cache = true
# cache-volumes = [] # only if overriding inherited mounts
# cache = []         # only if overriding inherited mounts
```

Keep the service/data outside runner-owned resources and generic wipeable cache
mounts. Read [service setup, supported clients and recovery](local-actions-cache.md).
For rollback, drain and disable affected opt-in sets before reverting settings;
simply disabling the native cache opt-in and resuming unchanged actions enables GitHub
caching again. An empty override is configuration behavior, not a local-only
rollback procedure.

`cache-volumes` and `[[docker.cache]]` are deprecated for new Docker
deployments. They continue to mount and enforce their existing budgets until
removed from the config. See the [migration steps](caching.md#migration-to-actions-cache)
before switching workflows or deleting old Docker volumes.

### Native Go receiver (opt-in)

The manager can host an opt-in cache-v2 receiver on a separate private listener.
Its isolated signed-job acceptance and a provider-managed Linux amd64 canary
have passed. Detailed run evidence is kept in private operator notes. Each new
host still needs its own image, endpoint, and workflow validation. Keep the
existing Rust service and data as a rollback endpoint, if present, until its
retention is decided. A new native deployment needs no Rust service. The global
table is not inherited by scale sets:

```toml
[actions-cache]
enabled = true
listen-address = "172.18.0.1:32500"
advertise-url = "http://cache.internal:32500/"
data-dir = "/var/lib/runner/native-actions-cache"
max-size-bytes = 53687091200
max-idle-days = 30
# audiences = ["an-exact-verified-runtime-audience"]
```

Use an actual private host IP for `listen-address` and a URL reachable from
both runner and sibling BuildKit containers. The directory must be new, private,
and separate from any existing external receiver volume. The receiver uses
GitHub's token issuer/JWKS and Results upstream by default. `token-issuer` and
`jwks-url` may be set together for a trusted HTTPS issuer;
`upstream-results-url` is a fixed HTTPS origin.
There is no token-validation bypass. `native-actions-cache = true` requires the
global receiver to be enabled; the scale set gets its advertised URL automatically.
Other scale sets retain their configured endpoint. The text and JSON forms of
`runner status` report native readiness, completed entries and bytes when enabled;
the text view also compares usage with the shared retention target. `runner cache`
provides more detailed storage information. Empty `cache-volumes = []` and
`cache = []` overrides are only needed when inherited mounts must be disabled.

## GitHub App Authentication

GitHub App authentication is supported for organization and repository runners.
Install the App on the organization or repository that owns the runners. For
organization runners, grant organization **Self-hosted runners: Read and write**.
For repository runners, grant repository **Administration: Read and write**;
repository **Metadata: Read-only** is included. Enterprise-level runners require
a PAT. See GitHub's [runner authentication guide](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/authenticate-to-the-api)
for App registration and permission details.

Configure the App's Client ID (preferred; a numeric App ID as a string also works),
Installation ID, and exactly one private-key source. These IDs are different;
the Installation ID identifies this App's installation on the target owner.
See [GitHub's JWT guide](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app).

```toml
url = "https://github.com/your-org"
name = "app-runners"
max-runners = 2

github-app.client-id = "Iv1.example"
github-app.installation-id = 123456
github-app.private-key-file = "/absolute/path/to/runner-app.pem"
# Alternatively, replace private-key-file with an environment variable name:
# github-app.private-key-env = "RUNNER_GITHUB_APP_KEY"
```

The key must be an unencrypted RSA PEM key (PKCS#1 or PKCS#8), at most 64 KiB.
Use an absolute file path and keep the file outside the repository with private
permissions, such as `0600`. For an environment source, the named variable must
contain the complete PEM value with real newlines; the config stores only its
name. Keys are read when a client is created, so restart the manager after
rotating a key. The SDK signs JWTs and exchanges credentials as needed.

Generated service definitions do not copy App key environment variables from
the installing shell. Use a key file for these services, or supply the variable
through the service manager explicitly.

When App configuration is present, the `RUNNER_TOKEN` fallback is ignored.
An explicit `token` or `--token` together
with App configuration is an error. Incomplete App settings also fail instead
of silently selecting PAT authentication.

App credentials, like PAT credentials, are never inherited by `[[scaleset]]`
entries. Configure each installation independently; App and PAT scale sets can
run in the same process:

```toml
[[scaleset]]
url = "https://github.com/org-a"
name = "app-runners"
github-app.client-id = "Iv1.example"
github-app.installation-id = 123456
github-app.private-key-env = "ORG_A_APP_KEY"

[[scaleset]]
url = "https://github.com/org-b"
name = "pat-runners"
token = "env:ORG_B_TOKEN"
```

For one scale set, the [config builder](https://ysya.github.io/runscaler/#configure)
can produce App references without receiving the private key. Save the copied
TOML locally, then put the key file at the configured absolute path.

`run`, `validate`, `doctor`, and `scaleset delete` use the same auth selection.
`doctor` performs read-only API probes for every configured scale set;
`validate` also checks provider connectivity and configuration.

## Token Security

Avoid passing tokens as CLI flags (visible in `ps` output). Two alternatives:

**Option 1: `RUNNER_TOKEN` environment variable** — automatically used when no `--token` flag or config value is set:

```bash
export RUNNER_TOKEN=ghp_xxx
runner run --url https://github.com/org --name my-runners
```

**Option 2: `env:` syntax in config file** — reference any environment variable by name:

```toml
token = "env:GITHUB_TOKEN"  # reads from $GITHUB_TOKEN at startup
```

Priority: `--token` flag > `RUNNER_TOKEN` env var > config file value (including `env:` resolution).

**Multiple scale sets (mixed Docker + Tart):**

```toml
# Global defaults (inherited by all scale sets)
runner-image = "ghcr.io/actions/actions-runner:latest"
runner-group = "default"
max-runners = 10
log-level = "info"

[docker]
socket = "/var/run/docker.sock"
dind = true

# Each [[scaleset]] runs independently.
# Inherits global settings if omitted.

[[scaleset]]
url = "https://github.com/your-org"
name = "linux-runners"
token = "ghp_aaa"

[[scaleset]]
provider = "tart"
url = "https://github.com/your-org"
name = "macos-runners"
token = "ghp_bbb"
max-runners = 2
runner-image = "ghcr.io/cirruslabs/macos-golden-gate-xcode:latest"
labels = ["self-hosted", "macOS"]
[scaleset.tart]
pool-size = 2
```

## CLI Flags

| Flag                | TOML key             | Default                                 | Description                                       |
| ------------------- | -------------------- | --------------------------------------- | ------------------------------------------------- |
| `--config`          |                      |                                         | Path to TOML config file                          |
| `--url`             | `url`                | (required)                              | Registration URL (org or repo)                    |
| `--name`            | `name`               | (required)                              | Scale set name (used as `runs-on` label)          |
| `--token`           | `token`              | (required for PAT mode)                 | GitHub Personal Access Token                      |
| `--github-app-client-id` | `github-app.client-id` | (unset)                              | GitHub App Client ID or numeric App ID as a string |
| `--github-app-installation-id` | `github-app.installation-id` | (unset)                  | GitHub App installation ID                        |
| `--github-app-private-key-file` | `github-app.private-key-file` | (unset)                | Absolute path to an RSA PEM key                   |
| `--github-app-private-key-env` | `github-app.private-key-env` | (unset)                  | Name of the environment variable containing the PEM key |
| `--provider`        | `provider`           | `docker`                                | Instance provider (`docker` or `tart`)             |
| `--max-runners`     | `max-runners`        | `10`                                    | Maximum concurrent runners                        |
| `--concurrent`      | `concurrent`         | `0` (sum of `max-runners`)              | Host-wide maximum across all scale sets           |
| `--min-runners`     | `min-runners`        | `0`                                     | Minimum runners to keep warm                      |
| `--labels`          | `labels`             | `<name>`                                | Runner labels (comma-separated)                   |
| `--runner-group`    | `runner-group`       | `default`                               | Runner group name                                 |
| `--runner-image`    | `runner-image`       | `ghcr.io/actions/actions-runner:latest` | Runner image (Docker image or Tart VM image)      |
| `--docker-socket`   | `[docker] socket`    | `/var/run/docker.sock`                  | Docker socket path (Docker provider)               |
| `--dind`            | `[docker] dind`      | `true`                                  | Mount Docker socket into runners (Docker provider) |
| —                   | `[docker] workspace-root` | unset                              | Private Linux host directory for per-runner Docker socket proxies |
| `--shared-volume`   | `[docker] shared-volume` |                                     | Shared Docker volume path (Docker provider)        |
| `--tart-cpu`        | `[tart] cpu`         | `0` (image default)                     | CPU cores per VM (Tart provider)                   |
| `--tart-memory`     | `[tart] memory`      | `0` (image default)                     | Memory in MB per VM (Tart provider)                |
| `--tart-runner-dir` | `[tart] runner-dir`  | `/Users/admin/actions-runner`           | Runner install directory inside Tart VM           |
| `--tart-pool-size`  | `[tart] pool-size`   | `0`                                     | Number of pre-warmed VMs for instant job pickup   |
| `--tart-net-bridged` | `[tart] net-bridged` | `""` (Tart's NAT)                      | Host interface to bridge each VM to               |
| `--log-level`       | `log-level`          | `info`                                  | Log level (debug/info/warn/error)                 |
| `--log-format`      | `log-format`         | `text`                                  | Log format (text/json)                            |
| `--dry-run`         | `dry-run`            | `false`                                 | Validate everything without starting listeners    |
| `--health-address`  | `health-address`      | `127.0.0.1`                             | Health check listen address                       |
| `--health-port`     | `health-port`        | `8080`                                  | Health check HTTP port (0 to disable)             |

Process-wide `drain-timeout` and advanced tuning keys (cleanup, cache volumes,
isolation) are config-file only by design — see `config.example.toml` for the
full list.

Like GitLab Runner, `concurrent` is the manager-wide ceiling while each scale
set's `max-runners` is its local ceiling. The controller reserves global
capacity before JIT/provider startup, so multiple organizations and mixed
Docker/Tart scale sets cannot collectively oversubscribe the host. Capacity for
every scale set's `min-runners` floor is reserved before FIFO surplus capacity
is handed out. Leaving `concurrent` at `0` keeps backward-compatible behavior
by using the sum of all configured `max-runners` values.

Use the `provider` TOML key and `--provider` flag to select Docker or Tart.
