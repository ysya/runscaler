# Commands

[English](commands.md) | [繁體中文](commands.zh-TW.md)

For a one-line summary of every command, see the [command table](../README.md#commands).

## Status dashboard

`runner status` turns the local `/healthz` snapshot into an operator-friendly
dashboard. The summary rolls up ready scale sets, runner lifecycle
(`provisioning`, `idle`, `busy`, `removing`), desired runners, queue depth, and
process-lifetime job counters. It also shows host-wide `concurrent` capacity as
`in_use / limit`. Each scale set then shows its connection state, last
successful connection, and last error. The disk section shows cheap
filesystem-level free-space readings; use `runner cache` when you need the
slower per-store usage measurements.
When the native Actions cache is enabled, a separate cache section shows its
readiness, completed-entry count, and used bytes. The usage bar compares bytes
with the shared retention target; it is not a hard filesystem quota. A degraded
receiver shows its state without stale-looking zero counts.

```bash
# Human-readable dashboard; color is enabled automatically in a terminal
runner status

# Stable machine-readable health response
runner status --json

# Plain dashboard for logs or terminals where color is undesirable
runner status --no-color
```

Each scale set's optional `runtime` object in `/healthz` (and `status --json`)
reports startup attempts, failures, consecutive failures, the last attempt and
successful start, global-capacity waiting, and draining. Counters last for the
manager process's lifetime. Missing startup timestamps mean no such event has
been recorded. Failure reasons are safe categories (`jit_config_failed` or
`provider_start_failed`), not raw provider errors or credential values.

`/healthz` remains a liveness endpoint returning HTTP 200. Its JSON `status`
becomes `degraded`, and `/readyz` returns HTTP 503, when a listener is disconnected,
a controller is draining, or a failed startup still leaves demand unmet. An
unfinished retry does not restore readiness; a successful start does. Normal
waits for global host capacity, scale-to-zero, and historical failures after
demand disappears are not faults. Runtime snapshots perform no provider or
GitHub requests. The dashboard explains these states as `ERROR`,
`WAITING FOR CAPACITY`, or `DRAINING`, with startup counts and timestamps.

## Updating

Read the [upgrade requirements](../README.md#upgrade-requirements) and the target release's
notes before replacing a working installation. Older commands, settings and
credential environment variables may require manual preparation.

```bash
# Check for a release and review its upgrade requirements without installing
runner update --check

# After preparing the installation, update its selected binary
runner update

# Or update, validate the config with the new binary and restart the service
runner update --restart
```

`runner update` displays the selected service scope, runtime user, binary and
config, downloads the archive for your platform, verifies its SHA-256 checksum
against `checksums.txt`, then atomically replaces the binary. Without
`--restart` it does not validate configuration/provider compatibility or
restart services, and it never rewrites service definitions. Existing system/user scope and runtime identity are retained;
an existing root service is not converted to a non-root account.

After installation, the command lists the steps that finish the upgrade:
validate the selected config with the new binary, then restart the service. It
also says how long a restart waits for running jobs, from the configured
`drain-timeout`. For a non-root system service, the validation command runs
through `systemd-run` with the service's identity, groups and environment file,
as `doctor` does. Hints name the selected binary and `--scope user|system`
unless those are redundant: a hint without sudo calls the binary `runner` when
that is what your PATH finds, and leaves `--scope` out when the selected
installation is the only one. System service management needs an
administrative shell.
A matching health version is not proof of configuration compatibility or
readiness. If health is disabled or unavailable, the running version is reported
as unverified rather than assumed stopped. When the binary is already current
but the service still reports an older version, `runner update` repeats the
steps. With `--binary-path` or no installed service, no service is selected:
restart processes using that binary manually after validation.

`runner update --restart` runs those steps itself. It validates the config with
the new binary as the service will run it and restarts the service only if that
passes, so a failed validation leaves the running service alone. When health
already reports the new version there is nothing to restart; when health cannot
report a version, it restarts only after installing a new binary. `--restart`
cannot be combined with `--check` or `--binary-path`.

SIGTERM normally begins a drain and waits for active jobs, bounded by
`drain-timeout` and the service manager's stop timeout. Jobs may be terminated
when drain is disabled or either deadline expires; check these settings before
restarting.

The messages above come from the updater already installed. An older updater
cannot show newly added warnings during its first upgrade. For that transition,
keep the previous binary and config backed up, download/extract the new binary
to a separate path, and validate the existing config with that new binary in
the intended runtime environment before replacing the service binary:

```bash
/path/to/new/runner validate --config /absolute/path/config.toml
```

The scale set itself stays registered across restarts, so a job queued while
runner is stopped — for an upgrade, a reboot, or after a crash — is picked up
as soon as runner reconnects. runner never deletes a scale set on its own: when
you retire a `[[scaleset]]`, stop runner and remove it from GitHub with
`runner scaleset delete <name>`.

When a listener disconnects, runner reconnects after five seconds, doubling the
wait up to a minute. After a GitHub rate limit it waits as long as GitHub asked,
or a minute when GitHub gave no wait, and never more than an hour.

## Troubleshooting with `doctor`

If runner is killed unexpectedly (e.g. `kill -9`, crash, power loss), Docker
containers or Tart VMs may be left behind. The next `runner run` reconciles
them automatically after acquiring the machine-wide lock. Use `doctor` for a
manual check. While a runner holds that lock, doctor reports the containers and
VMs it owns as in use rather than orphaned:

```bash
# Check GitHub API connectivity and orphaned resources
runner doctor

# Auto-remove orphaned containers and VMs
runner doctor --fix
```

Doctor probes every configured scale set's GitHub API using its App or PAT
credentials, resolves its runner group, and queries its registration. Each
probe has a five-second timeout; failures do not prevent later probes or local
resource checks. Cancellation stops further probes. A scale set that is not
registered yet is reported without creating or modifying it.

Doctor then lists each runner group it reached and reports the scale sets
registered there that no `[[scaleset]]` with the same registration URL names.
Another host may run them. If none does, jobs that target one stay queued,
because runner never deletes a scale set on its own: add it to the config as a
`[[scaleset]]`, stop runner, and run `runner scaleset delete <name>`. Finding
such scale sets does not fail doctor; a failed listing does.

Without a config file, doctor skips the GitHub API checks and lists where it
looked; `runner validate` fails with the same list. Failed API checks make the
command exit unsuccessfully even when there are no orphaned resources. Reports contain safe cancellation, timeout, DNS, TLS,
rate-limit, or numeric HTTP-status categories, and a rate limit includes the
wait GitHub asked for; raw SDK response bodies are not printed.

The `--fix` flag takes the same machine-wide lock as `runner run`, so it refuses
to remove resources while any current runner instance is active. Shared and
cache volumes are never removed by startup reconciliation or `doctor --fix`:
they intentionally outlive the process and may still contain data needed by a
later job in an in-flight workflow.

Normal runner removal and these fix paths stop the root runner, then remove
containers explicitly linked by `--network container:<runner-id-or-name>` or
`runscaler.runner-id=<runner-id-or-name>`. ID prefixes must be unambiguous and
at least 12 characters. Other managed runners are excluded. Anonymous volumes
are removed with their containers. Temporary named volumes are reclaimed only
with an explicit runner-owner label, or when an owned Buildx child mounts its
exact `<buildx-container-name>_state` volume. Volumes still referenced by the
root, another container or a failed child are retained; named-volume failures
are logged without forcing removal.

The independently owned `runscaler-actions-cache` service and its external data
volume are preserved. Discovery starts from a recognized runner parent;
unattributed containers and volumes with an already-missing parent are not a
global prune target. `doctor` without `--fix` remains read-only.

Startup reconciliation and `doctor` recognise runner's resources by the same
rules: a container carries the `managed-by=runner` label or an exact
`runner-<8 hex>` name, and a local Tart VM carries an exact `runner-`, `pool-`
or `runner-keep-` name of the form runner generates. Only those parent runners and their explicitly associated child resources
are removed; unrelated daemon resources are retained.

## Logs

`runner run` writes to stdout and a rotating file (10 MB plus one backup). The
file's default location depends on who runs it:

| Runs as | Log file |
| --- | --- |
| Linux system service (dedicated account or root), or `sudo runner run` | `/var/log/runner/runner.log` |
| a user on Linux | `$XDG_STATE_HOME/runner/runner.log` (`~/.local/state/runner/runner.log`) |
| a user on macOS | `~/Library/Logs/runner/runner.log` |

Set `log-file` to an explicit path, or `log-file = ""` to disable file logging.
As root, `log-file` must be in a directory under `/var/log` whose name uses only
letters, digits, `.`, `_` or `-`; anything else falls back to the default with a
warning. Releases before v0.10 wrote `runner.log` beside the config; that file is
left in place.

Log output on stdout is colored only when stdout is a terminal; under systemd,
launchd, `docker run` without `-t`, CI or a pipe it stays plain. The log file
never contains color codes. Set `NO_COLOR=1` to turn color off or
`CLICOLOR_FORCE=1` to force it on.

```bash
runner logs                 # last 100 lines (add sudo for a system service)
runner logs -n 500 -f
```

Only one `runner run` process may be active on a host. Put every organization
or repository in that process using multiple `[[scaleset]]` entries.

## Runner startup and failure recovery

Docker runners remain provisioning until their logs report `Listening for Jobs`
and the container stays alive for five more seconds. GitHub work assignment or
completion also proves startup. The default deadline is five minutes. Images
with a custom entrypoint that cannot emit this marker can set
`docker.readiness = false`; status then reports `unverified` and a warning is
logged. `docker.ready-timeout` changes the deadline. A non-root Docker client
does not imply that its Docker daemon is rootless.

Failed starts retry with jitter: 2.5–5 seconds initially, doubling up to
`retry.max` (default one minute). After five consecutive failures, retries enter
a five-minute cooldown. Configure `[retry]` with `initial`, `max`,
`cooldown-after`, and `cooldown`, globally or per scale set. Fresh demand cannot
bypass the deadline, and minimum runners still apply with an empty queue.
Verified recovery resets the relevant failure generation. Tart warm-pool slots
use independent retry histories; job execution exits and cleanup retries do not
increase startup failures. `runner status` and health JSON expose readiness,
retry state/deadline, and safe failure metadata.

Failure diagnostics are enabled by default under `diagnostics/` beside the log
file actually opened, including its fallback. With file logging disabled, the
scope's default log directory is used. Reports use a private directory (0700)
and files (0600), retain at most 20 files / 2 MiB / seven days, and include known
exit code, OOM and finish time without inventing unknown values. Failed cleanup
retains capacity until removal succeeds. Unknown GitHub identity is preserved
until work evidence permits cleanup or explicit force shutdown.

Raw logs are off by default. `diagnostics.capture-logs = true` opts into bounded
excerpts (100 lines / 64 KiB), with configured PAT/JIT values, Authorization
headers and PEM blocks redacted. This cannot identify every workflow secret;
keep capture off when jobs may print sensitive data. Set
`diagnostics.enabled = false` to disable reports. Diagnostics never include raw
logs in health JSON and a collection/write failure does not prevent cleanup.
