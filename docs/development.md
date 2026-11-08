# Development

[English](development.md) | [繁體中文](development.zh-TW.md)

## Building

```bash
# Current platform
make build

# All platforms (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64)
make all
```

## Architecture

Built on top of [actions/scaleset](https://github.com/actions/scaleset), the official Go client library for GitHub Actions Runner Scale Sets.

Key components:

```
cmd/runner/          CLI entry point and commands (run, init, validate, status, doctor, cache, logs, service, update, scaleset, version)
internal/
  config/            Configuration management with Viper (flags + TOML) and logger setup
  layout/            Default config/log/state paths per identity (FHS, XDG, Apple)
  pathtrust/         "Only root can modify this path" checks
  lock/              Machine-wide process and destructive-maintenance lock
  provider/          InstanceProvider + Docker/Tart implementations
  controller/        ScaleSetController and the listener MessageHandler; instance lifecycle, readiness and retries
  capacity/          Host-wide concurrent-runner broker shared by all scale sets
  backoff/           Exponential backoff with jitter for startup retries
  cachestore/        CacheStore abstraction over reclaimable Docker/Tart storage
  diskguard/         Disk-pressure guard reclaiming through cache stores by tier
  bytesize/          Size and threshold parsing for disk settings
  diagnostics/       Failure reports written beside the log file
  metrics/           Per-scale-set job counters and runtime status snapshots
  health/            Health check HTTP server (/healthz, /readyz)
  versioncheck/      GitHub releases API client for update notifications and in-place binary updates
```

The runner process is the **manager**. Each configured scale set owns one
`ScaleSetController`, which translates GitHub's desired-runner/job events into
an `InstanceProvider` lifecycle:

- **`DockerProvider`** — manages runner container instances via Docker API
- **`TartProvider`** — manages VM instances via Tart CLI (clone → run → exec → stop → delete)

Each listener session passes its messages to a `MessageHandler`, which
implements the upstream `listener.Scaler` interface. It acquires the jobs a
message offers, then hands job starts, job completions and the desired runner
count to the controller. The listener acknowledges a message only after the
handler succeeds, so a message whose jobs could not be acquired is delivered
again instead of lost. The controller's handlers:

- `HandleDesiredRunnerCount` — Scales up runners to match job demand
- `HandleJobStarted` — Marks runners as busy
- `HandleJobCompleted` — Removes finished runners
