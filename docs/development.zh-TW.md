# 開發

[English](development.md) | [繁體中文](development.zh-TW.md)

本文件為 [英文版](development.md) 的繁體中文版本；若內容有差異，以英文版為準。

<a id="building"></a>

## 建置

```bash
# Current platform
make build

# All platforms (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64)
make all
```

<a id="architecture"></a>

## 架構

建構於 GitHub Actions Runner 規模集的官方 Go 用戶端函式庫 [actions/scaleset](https://github.com/actions/scaleset) 之上。

主要元件：

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

runner 程序是**管理者**。每個已設定的 runner 規模集各有一個 `ScaleSetController`，負責將 GitHub 所需的 runner 數量與工作事件轉換成 `InstanceProvider` 生命週期操作：

- **`DockerProvider`** — 透過 Docker API 管理 runner 容器執行個體
- **`TartProvider`** — 透過 Tart CLI 管理 VM 執行個體（複製 → 執行 → 執行命令 → 停止 → 刪除）

每個 listener session 會把訊息交給 `MessageHandler`，由它實作上游的 `listener.Scaler` 介面：先認領訊息提供的工作，再把工作開始、工作完成與所需 runner 數量交給 controller。listener 只在 handler 成功處理後才確認訊息，因此工作認領失敗的訊息會重新送達，不會遺失。controller 的處理函式：

- `HandleDesiredRunnerCount` — 擴增 runner 以符合工作需求
- `HandleJobStarted` — 將 runner 標記為忙碌
- `HandleJobCompleted` — 移除已完成工作的 runner
