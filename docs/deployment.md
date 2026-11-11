# Deployment

[English](deployment.md) | [繁體中文](deployment.zh-TW.md)

## Graceful Shutdown

The default `drain-timeout` is `2h`. Shutdown signals have distinct meanings:

| Signal | Behavior |
| --- | --- |
| `SIGTERM` or `SIGQUIT` | Stop scaling up, remove idle runners, and wait for busy jobs to finish |
| Second signal during drain | Stop immediately and remove remaining runners |
| Third signal | Exit without waiting for cleanup |
| `SIGINT` / Ctrl-C | Stop immediately; never drain |

Before removing an apparently idle runner during drain or scale-down, runner
asks GitHub to deregister it. If GitHub has already assigned a job, the runner
is kept until that job finishes, even when `JobStarted` has not arrived yet.
Deregistration and cleanup failures are retried during drain until they succeed
or the configured drain timeout expires.

Interactive Ctrl-C intentionally keeps its conventional "stop now" meaning.
To drain a foreground process, send TERM explicitly:

```bash
kill -TERM <pid>
```

`runner service install` generates a systemd `TimeoutStopSec` or launchd
`ExitTimeOut` one minute longer than the configured drain budget and records
it, so runner warns when a later `drain-timeout` outgrows it. A binary update
does not rewrite an installed service definition; regenerate it in place:

```bash
sudo runner service install --scope system --force --config-path /etc/runner/config.toml   # Linux system service
runner service install --scope user --force --config-path ~/.config/runner/config.toml           # user service
```

`--force` validates the config and binary and renders the new definition before
touching the installed one. When a definition is outdated, runner logs the
command that fixes it, with the exact config and binary paths it is running
with: this `--force` reinstall, or for a root service whose binary is not root-owned, a
copy to `/usr/local/bin` followed by the reinstall.

After that, the normal upgrade flow is:

```bash
runner update              # add sudo when the binary is in /usr/local/bin
runner service restart     # waits for active jobs automatically
```

## Service layout

- **New Linux system services** are installed by root and run as the dedicated
  non-login `runscaler` account. The binary remains root-owned; config/App key
  files are `root:runscaler` with mode `0640`, and `/etc/runner` is `0750`.
  HOME and working directory are `/var/lib/runner`; state and logs are owned
  by the service account with mode `0750`. `ProtectSystem=strict` keeps config
  read-only; explicit writable paths cover `/tmp`, state and the configured
  log directory below `/var/log`. Installation refuses conflicting accounts,
  linked/shared credential files and populated directories owned by others.
  Ancestor directories, including the log parent, must be root-owned without
  group/other write permission. Administrators must prepare these host paths;
  the installer does not change ancestor permissions.
- **Linux user services** use `runner service install --scope user` (or
  `--user`), XDG config/state paths and the invoking user's credentials and
  Docker socket. No sudo is needed. Root user-services are rejected.
- **macOS** supports only a LaunchAgent for the logged-in user. It starts at
  login, so enable automatic login on unattended hosts, and install it from a
  local GUI session rather than SSH. runner refuses to run, init, install or
  start as root on macOS. launchd starts agents with a minimal `PATH`; runner
  appends `/opt/homebrew/bin` and `/usr/local/bin` when present so a
  Homebrew-installed `tart` is found. The agent discards stdout (runner writes
  its own rotated log) and keeps stderr in `~/Library/Logs/runner/stderr.log`,
  which is not rotated.
- The run lock at `/tmp/runner.lock` prevents accidentally starting a second
  runner; it is not a security boundary against local users.

```bash
# New Linux system installation: configure first, then prepare and start
sudo /usr/local/bin/runner init --output /etc/runner/config.toml
sudo /usr/local/bin/runner service install --config-path /etc/runner/config.toml

# macOS, or Linux with a user-owned binary and provider
runner init
runner service install --scope user
```

System credentials must be explicit in config. For `token="env:NAME"` or an
App `private-key-env`, pass a root-protected systemd environment file with
`--environment-file /etc/runner/credentials.env`; the installing shell's
credentials are never persisted. Before writing its unit, installation runs
`runner validate` as the service identity, so the Docker socket, credentials
and GitHub access are checked exactly as the service will use them.
`--no-start` skips that live probe for offline preparation. Rootful Docker socket access is granted
only through unit supplementary groups and remains root-equivalent; it is
distinct from a rootless daemon. Another user's rootless socket is refused.

Management commands (`status`, `logs`, `doctor`, `service …`, `update`) use a
unique installed target's binary/config. `--scope user|system` selects one
explicitly; `--user` and `--user=false` remain aliases, hidden from help. Auto mode refuses two
installations or unverifiable metadata. Installation refuses a second scope.
Discovery sees system definitions but only the invoking account's user
definition, so a fresh installation also refuses while another runner holds
the run lock, such as another account's user service or a foreground
`runner run`.
Linux discovery requires systemd's `busctl` JSON support and the relevant
service-manager bus. Explicit scopes never fall back to another installation.
Without any installed service, normal standalone config discovery still works.
A system unit a package installed under `/usr/lib/systemd/system` or
`/lib/systemd/system` is managed like runner's own, but `uninstall` never
deletes it and instead says how to disable it; `install --force` writes
runner's unit to `/etc/systemd/system`, which takes precedence, and restarts
the service so it applies.

`runner update` replaces the selected service's binary and restarts it only with `--restart`.
A system update requires root and a root-protected current updater; a writable user binary does not. To update a
specific binary, including a root-owned shared binary on macOS, use
`sudo /absolute/path/current-runner update --binary-path /absolute/path/target-runner`.
This binary-only operation does not select or operate a LaunchAgent.
For system `doctor`/`doctor --fix`, follow the printed `sudo systemd-run …`
command to reproduce the service UID, unit groups and credential sources;
plain `sudo -u` may omit its Docker group. `--fix` still requires a stopped
manager and the shared machine lock. Inline private environment is not copied
into command arguments; move it into a protected EnvironmentFile first.

Existing Linux root services remain a supported installation mode and retain
their root runtime under `update` and `--force`. Upgrades do not require identity conversion; the non-root
default applies to new system installations. No root-to-non-root conversion
command is planned in this work. Uninstall/reinstall does not automatically
take over existing root-owned data. Existing config and backup files are never
moved or deleted by an upgrade.

## Systemd

This is the unit `sudo runner service install --scope system` generates for a
Docker configuration with the default drain timeout. Prefer the command, so
the unit stays in sync with the binary.

```ini
[Unit]
Description=GitHub Actions Runner Manager
After=docker.service
Requires=docker.service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=runscaler
Group=runscaler
# SupplementaryGroups=<configured socket GID> is added when required.
WorkingDirectory=/var/lib/runner
ExecStart="/usr/local/bin/runner" run --config "/etc/runner/config.toml"
Restart=on-failure
RestartSec=10s
# Must exceed runner's drain budget so systemd does not SIGKILL an in-flight job.
TimeoutStopSec=7260
Environment="RUNNER_SERVICE_VERSION=3"
Environment="RUNNER_SERVICE_STOP_TIMEOUT=7260"
Environment="RUNNER_SERVICE_SCOPE=system"
Environment="HOME=/var/lib/runner"
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/tmp /var/lib/runner /var/log/runner

[Install]
WantedBy=multi-user.target
```

## Host-local Actions cache rollout

The optional [local-cache runbook](local-actions-cache.md) covers the separate
service/data volume, immutable runner variant, approved canary and rollback.
A manager configuration change needs a maintenance restart; there is no hot
reload, per-set disable switch, or second-manager exception to the host lock.
Preserve unrelated entries and check the running version's drain behavior and
service stop deadline before any restart. Runner/doctor cleanup preserves the
external cache data volume; it is not a generic wipeable job cache.

The native Go receiver is being developed behind separate acceptance gates.
Its `[actions-cache]` listener starts before runner listeners and stays available
through normal drain; a manager restart also interrupts cache and artifact
passthrough. Test it with a fresh directory and separate port in an isolated
environment. Preserve the Rust service, external volume, and backups for
rollback. A host cutover needs its own reviewed maintenance approval.
