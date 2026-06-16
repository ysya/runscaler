# 設計:`runscaler` → `runner`,啟動改為 `runner run`

- 日期:2026-06-16
- 狀態:草案(待 review)
- 範圍:CLI 改名 + 啟動子指令化(破壞性變更)

## 背景與動機

目前 `runscaler` 的根指令直接帶有「啟動長駐 listener」的副作用:`runscaler --config config.toml` 一執行就開始 scaling([main.go:139-159](../../../cmd/runscaler/main.go#L139))。這在 cobra 是反模式——根指令的 `RunE` 不該產生長駐副作用,容易誤觸,且 `runscaler`(裸跑)無法像 `git` / `docker` 那樣顯示說明。

本案做兩件事:

1. 把 binary 從 `runscaler` 改名為 `runner`。
2. 把「啟動 scaling」從根指令的副作用,改成第一層子指令 `runner run`;`runner`(裸跑)改為顯示 help。

## 目標

| 現在 | 改後 |
|------|------|
| `runscaler --config x.toml` 直接啟動 | `runner run --config x.toml` 啟動 |
| `runscaler`(裸跑)啟動 listener | `runner`(裸跑)顯示 help |
| binary 名 `runscaler` | binary 名 `runner` |

其餘子指令(`init` / `validate` / `status` / `doctor` / `version` / `update` / `service`)結構不變,僅 help / Example 文字中的 `runscaler` → `runner`。

## 非目標

- **不改** Go module path `github.com/ysya/runscaler` 與所有 internal import。
- **不改** GitHub repo `ysya/runscaler`(`versioncheck.githubRepo` 維持指向同一 repo)。
- **不改** config schema 或 TOML 欄位。
- **不改** scaling 的行為邏輯本身(僅把進入點從 root `RunE` 搬到 `run` 子指令)。

## 決策摘要

| 決策點 | 結論 |
|--------|------|
| 改名範圍 | binary、系統識別碼、self-update/release 全改;保留 module path 與 repo |
| 啟動子指令名 | `run`(`runner run`) |
| 原始碼目錄 | `cmd/runscaler` → `cmd/runner`(不影響 import path,main package 不被引用) |
| docker shared volume | 改名 `runner-shared`,doctor 需向後相容清理舊 `runscaler-shared` 孤兒 |
| self-update 斷點 | 過渡期雙資產名:goreleaser 同時產出 `runner-*` 與 `runscaler-*` |
| launchd label | 用反向 DNS + 帳號 `io.github.ysya.runner`(label 是全域命名空間,避免與其他服務衝突);binary、systemd unit、路徑、volume 維持簡短 `runner` |

## 詳細設計

### 1. CLI 結構

```
runner                         # 顯示 help(不啟動)
runner run [啟動 flags]        # 啟動 scaling(原 root RunE 的邏輯)
runner init | validate | status | doctor | version | update
runner service install | uninstall | start | stop | restart | status | logs
```

- 根 command 移除 `RunE`。cobra 對沒有 `Run`/`RunE` 的 parent command,執行時預設顯示 usage/help —— 正是我們要的「裸跑印 help」。
- 新增 `runCmd`(`Use: "run"`),`RunE` 即原 [main.go:139-159](../../../cmd/runscaler/main.go#L139) 的內容(signal 處理 + `run(ctx, cfg)`)。

### 2. 啟動 flag 遷移

目前啟動用 flag 綁在根的 local `cmd.Flags()`([main.go:45-106](../../../cmd/runscaler/main.go#L45)):`url`、`name`、`token`、`max-runners`、`min-runners`、`labels`、`runner-group`、`runner-image`、`backend`、`docker-*`、`tart-*`、`log-level`、`log-format`、`dry-run`、`health-port`。

改動:

- 上述 flag 定義與對應的 `viper.BindPFlag` 全部移到 `runCmd`。
- `--config` 維持為**根的 PersistentFlag**(`validate` / `status` 等子指令共用)。
- `loadConfig(runCmd)` 透過 `cmd.Flags().GetString("config")` 取得繼承來的 `--config`(cobra 子指令會合併父層 persistent flags,`validate` 目前即如此運作 → [loadconfig.go:16](../../../cmd/runscaler/loadconfig.go#L16))。
- `viper.BindPFlag` 綁定的是 flag 指標,flag 物件搬到 `runCmd.Flags()` 後綁定依然有效。

### 3. service 模板必須加入 `run`(易遺漏)

啟動變成子指令後,service 產生的啟動命令要對應加上 `run`:

- systemd([cmd_service.go:283](../../../cmd/runscaler/cmd_service.go#L283)):`ExecStart={{.BinaryPath}} run --config {{.ConfigPath}}`
- launchd([cmd_service.go:443-448](../../../cmd/runscaler/cmd_service.go#L443)):`ProgramArguments` 在 binary 之後插入 `<string>run</string>`
- [cmd_service_test.go](../../../cmd/runscaler/cmd_service_test.go) 模板斷言同步更新(驗證含 `run` 與新路徑)

### 4. 系統識別碼改名

於 [cmd_service.go:19-33](../../../cmd/runscaler/cmd_service.go#L19) 與相關處:

| 項目 | 舊 | 新 |
|------|----|----|
| serviceName | `runscaler` | `runner` |
| defaultConfigPath | `/etc/runscaler/config.toml` | `/etc/runner/config.toml` |
| systemdUnitFile | `runscaler.service` | `runner.service` |
| launchdLabel | `com.runscaler.agent` | `io.github.ysya.runner` |
| launchdPlistFile | `com.runscaler.agent.plist` | `io.github.ysya.runner.plist` |
| log path(system) | `/var/log/runscaler.log` | `/var/log/runner.log` |
| log path(user) | `runscaler.log` | `runner.log` |
| viper config 搜尋路徑 | `/etc/runscaler` | `/etc/runner`([loadconfig.go:25](../../../cmd/runscaler/loadconfig.go#L25)) |

**命名衝突考量:** 因為本工具部署的機器本身就是 runner 主機,`runner` 屬通用名。launchd label 是全域命名空間且應遵循反向 DNS 慣例(原本的 `com.runscaler.agent` 也是借用),故改用 `io.github.ysya.runner`(對應 `github.com/ysya`)。其餘識別碼(binary、`runner.service`、`/etc/runner`、`runner-shared`)維持簡短 `runner`:binary 短名便於日常使用且 PATH 由使用者自管;GitHub 官方 self-hosted runner 的 systemd unit 通常是 `actions.runner.*` 開頭,不會與 `runner.service` 直接衝突。此即「binary 用短名、安裝識別碼用反向 DNS」的業界慣例(如 Docker:binary `docker` / label `com.docker.*`)。

### 5. docker shared volume 改名 + 向後相容

- 正常運作改用 `runner-shared`([docker.go](../../../internal/backend/docker.go) 的建立 / 掛載 / 移除;[main.go](../../../cmd/runscaler/main.go) 註解)。
- `doctor` 的孤兒 volume 檢查([cmd_doctor.go:239-261](../../../cmd/runscaler/cmd_doctor.go#L239))需**同時**處理:
  - `runner-shared`(目前使用中的孤兒)
  - `runscaler-shared`(改名前遺留,升級後必為孤兒 → 應提示並清理)
- [docker_test.go](../../../internal/backend/docker_test.go) 斷言改為 `runner-shared`;建議新增「舊 `runscaler-shared` 也會被 doctor 清理」的測試。

### 6. self-update / release 雙資產(過渡相容)

**新版 self-update**([update.go](../../../internal/versioncheck/update.go)):

- `DownloadURL` / `Update` 的資產名 `runscaler-%s-%s.tar.gz` → `runner-%s-%s.tar.gz`
- `extractBinary` 尋找的 binary 名 `runscaler` → `runner`([update.go:77-78](../../../internal/versioncheck/update.go#L77))
- 暫存檔 prefix 一併改為 `runner`(純內部,求一致)
- `githubRepo` 維持 `ysya/runscaler`([check.go:13](../../../internal/versioncheck/check.go#L13)),**不改**。

**goreleaser 雙資產**([.goreleaser.yaml](../../../.goreleaser.yaml)):過渡期同時產出兩組 archive,讓舊版 `runscaler update` 仍能抓到最後一版。

```yaml
builds:
  - id: runner
    main: ./cmd/runner
    binary: runner
    # ...ldflags / goos / goarch 同現狀
  - id: runscaler-compat
    main: ./cmd/runner
    binary: runscaler        # 同一份程式,檔名沿用 runscaler 供舊版抽取
    # ...
archives:
  - id: runner
    builds: [runner]
    name_template: "runner-{{ .Os }}-{{ .Arch }}"
  - id: runscaler-compat
    builds: [runscaler-compat]
    name_template: "runscaler-{{ .Os }}-{{ .Arch }}"
checksum:
  name_template: "checksums.txt"   # 含兩組 archive 的 checksum
```

**已知限制(務必寫進 release notes):** 雙資產只保證舊版 `runscaler update` 能**下載並替換** binary;但因啟動方式已改(裸跑不再啟動、service 的 `ExecStart` 缺少 `run`),升級後仍需更新 service 設定。新版可在偵測到疑似舊式呼叫時印出遷移提示。建議過渡維持數個版本後,移除 `runscaler-compat` 資產。

### 7. build / 文件

- [Makefile](../../../Makefile):`BINARY_NAME := runner`;build / `go run` 路徑 `./cmd/runner`。
- [Dockerfile](../../../Dockerfile):binary 名、build 路徑、ENTRYPOINT。
- [install.sh](../../../install.sh):binary 名 / 安裝路徑。
- [README.md](../../../README.md):指令範例、systemd 範例([README.md:351](../../../README.md#L351))、`/etc/runner` 路徑。**注意**區分:repo URL `github.com/ysya/runscaler` 保留;指令與系統路徑改。
- [config.example.toml](../../../config.example.toml)、[images/actions-runner/README.md](../../../images/actions-runner/README.md)、[.gitignore](../../../.gitignore)(build 產物名)。
- `.github/workflows/`:`release.yml`(goreleaser)、`ci.yml`(build/test 路徑)。

## 受影響檔案清單(約 30 檔)

**A. CLI 核心(`cmd/runner/`,目錄改名後)**
`main.go`、`cmd_service.go`、`cmd_service_test.go`、`cmd_doctor.go`、`cmd_init.go`、`cmd_status.go`、`cmd_update.go`、`cmd_version.go`、`cmd_validate.go`、`loadconfig.go`

**B. internal(保留 import path,只改字串 / 識別碼)**
`backend/docker.go`、`backend/docker_test.go`、`backend/tart.go`、`config/config.go`、`versioncheck/update.go`、`versioncheck/check_test.go`、`scaler/scaler.go`、`health/health.go`、`health/health_test.go`
（`versioncheck/check.go` 的 `githubRepo` 與 `go.mod` 的 module 行 → **保留不動**）

**C. build / release / 文件**
`Makefile`、`.goreleaser.yaml`、`Dockerfile`、`install.sh`、`config.example.toml`、`.gitignore`、`README.md`、`images/actions-runner/README.md`、`.github/workflows/release.yml`、`.github/workflows/ci.yml`

## 相容性 / 遷移(破壞性)

1. **既有 service 安裝**:unit file / label / config 路徑全變。升級步驟:用舊 binary `runscaler service uninstall` → 安裝新 binary → `runner service install`。
2. **config 路徑** `/etc/runscaler` → `/etc/runner`:既有部署需搬移 config,或以 `--config` 明確指定。
3. **self-update**:見 §6,雙資產可下載成功但需更新 service 設定。
4. **shared volume**:舊 `runscaler-shared` 升級後成孤兒,由 `runner doctor` 清理。

## 測試計畫

- `cmd_service_test.go`:模板含 `run` 子指令與新路徑(`/etc/runner` 等)。
- 新增 CLI 結構測試:根 command 無 `RunE`(裸跑顯示 help、不啟動);`run` 子指令存在且具備 `url` / `config` 等啟動 flag。
- `doctor` 測試:`runner-shared` 與舊 `runscaler-shared` 皆能被辨識為孤兒並清理。
- `docker_test.go`:volume 斷言改為 `runner-shared`。
- 全量 `go test ./...` 通過。

## 推出順序建議

1. 程式碼:`git mv cmd/runscaler cmd/runner` → CLI 結構(root 去 RunE、新增 `run`、flag 遷移)→ service 模板 → 系統識別碼 → volume 改名 + doctor 相容 → self-update。
2. build / release:Makefile、goreleaser 雙資產、Dockerfile、install.sh、workflows。
3. 文件:README、config.example、images README、.gitignore。
4. 測試補齊並全量通過。
5. release notes 撰寫遷移說明。
