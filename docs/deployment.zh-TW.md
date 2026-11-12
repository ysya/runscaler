<a id="deployment"></a>

# 部署

[English](deployment.md) | [繁體中文](deployment.zh-TW.md)

本文件為 [英文版](deployment.md) 的繁體中文版本；若內容有差異，以英文版為準。

<a id="tart-prerequisites-macos"></a>

## Tart 必要條件（macOS）

在 Apple Silicon Mac 安裝 Tart。Homebrew 可能要求先信任 Tart 與 Softnet 的
formula，才能載入：

```bash
brew trust --formula openai/tools/tart openai/tools/softnet
brew install openai/tools/tart
```

拉取與主機 macOS 版本相容、已預裝 Xcode 的 runner 映像檔。例如 macOS 26
主機可使用：

```bash
tart pull ghcr.io/cirruslabs/macos-tahoe-xcode:latest
```

[Cirrus Labs 映像檔](https://github.com/orgs/cirruslabs/packages?tab=packages&q=macos-)
也有 `macos-sequoia-xcode`（macOS 15）與
`macos-golden-gate-xcode`（macOS 27）。VM 的 macOS 版本不能比主機新。
映像檔標籤選擇的是 Xcode 版本；需要可重現的建置時，請固定標籤，不要使用
`latest`。調整 `[tart]` 下的 CPU 和記憶體前，請先查看映像檔公布的
需求。[設定產生器](https://runscaler.ysya.dev/configure/zh-TW.html)會將 Tart 上限設為每台主機兩台 VM。

若從舊的 `cirruslabs/cli` tap 升級，先解除安裝該來源的 `tart` 與
`softnet`，再從 `openai/tools` 安裝。檢查舊 tap 中仍安裝的 formula 後，
即可移除舊 tap。詳見
[Tart 安裝指南](https://github.com/openai/tart/blob/main/docs/quick-start.md)與
[Homebrew tap 信任指南](https://docs.brew.sh/Tap-Trust)。

<a id="other-binary-installation-methods"></a>

## 其他執行檔安裝方式

安裝指令碼只下載執行檔。Linux 系統安裝使用 `/usr/local/bin`；
macOS 與 Linux 使用者安裝使用 `~/.local/bin`。可用 `INSTALL_DIR`
選擇其他絕對路徑，或用 `RUNNER_VERSION` 固定發行版本。系統執行檔
及其所有上層目錄都必須防止非 root 使用者修改。

```bash
curl -fsSL https://raw.githubusercontent.com/ysya/runscaler/main/install.sh | sudo env INSTALL_DIR=/opt/runner/bin sh
curl -fsSL https://raw.githubusercontent.com/ysya/runscaler/main/install.sh | RUNNER_VERSION=v0.20.1 sh -s -- --user
```

也可執行 `go install github.com/ysya/runscaler/cmd/runner@latest`，
或從[發行版本](https://github.com/ysya/runscaler/releases)下載預先編譯的
執行檔。安裝執行檔不會建立服務；請接著使用下方的服務指令。

<a id="graceful-shutdown"></a>

## 正常關閉

`drain-timeout` 預設為 `2h`。各種關閉訊號具有不同意義：

| 訊號 | 行為 |
| --- | --- |
| `SIGTERM` 或 `SIGQUIT` | 停止擴增、移除閒置 runner，並等待忙碌中的工作完成 |
| 排空期間收到第二次訊號 | 立即停止，並移除其餘 runner |
| 第三次訊號 | 不等待清理，直接結束 |
| `SIGINT` / Ctrl-C | 立即停止；絕不排空 |

在排空或縮減期間，runner 會先要求 GitHub 取消註冊，再移除看似閒置的 runner。如果 GitHub 已經指派工作，即使尚未收到 `JobStarted`，也會保留該 runner，直到工作完成。排空期間若取消註冊或清理失敗，會持續重試，直到成功或達到設定的排空逾時為止。

互動式 Ctrl-C 刻意保留慣常的「立即停止」意義。若要排空前景程序，請明確傳送 TERM：

```bash
kill -TERM <pid>
```

`runner service install` 會產生比所設定的排空時間額度多一分鐘的 systemd `TimeoutStopSec` 或 launchd `ExitTimeOut`，並記錄該值，讓 runner 在之後的 `drain-timeout` 超過此值時發出警告。更新二進位檔不會改寫已安裝的服務定義；請在原處重新產生：

```bash
sudo runner service install --scope system --force --config-path /etc/runner/config.toml   # Linux system service
runner service install --scope user --force --config-path ~/.config/runner/config.toml           # user service
```

`--force` 會先驗證設定與二進位檔，並產生新的定義內容，之後才碰觸已安裝的定義。當定義過時，runner 會在日誌中記錄修正指令，使用它目前執行時的確切設定與二進位檔路徑。若是系統服務但二進位檔不屬於 root，則會先提示複製至 `/usr/local/bin`，再重新安裝。

之後，一般升級流程如下：

```bash
runner update              # add sudo when the binary is in /usr/local/bin
runner service restart     # waits for active jobs automatically
```

<a id="service-layout"></a>

## 服務配置

- **Linux 新系統安裝**由 root 安裝，以專用的 `runscaler` 非登入帳號執行。binary 保持 root 擁有；config／App key 為 `root:runscaler`、`0640`，`/etc/runner` 為 `0750`。HOME 與工作目錄為 `/var/lib/runner`；state 與 log 目錄由服務帳號擁有、權限為 `0750`。`ProtectSystem=strict` 讓設定維持唯讀，只開放 `/tmp`、state 與 `/var/log` 下的有效日誌目錄寫入。安裝會拒絕衝突帳號、連結或共用的憑證檔，以及已有其他擁有者資料的目錄。父目錄（包含日誌目錄的父層）必須由 root 擁有，且不可讓群組或其他使用者寫入；請由管理員先準備主機路徑，安裝器不會更改父目錄權限。
- **Linux user 安裝**使用 `runner service install --scope user`（或 `--user`），沿用使用者的 XDG 路徑、憑證與 Docker socket，不需要 sudo。拒絕建立 root 的 user service。
- **macOS** 僅支援已登入使用者的 LaunchAgent。它會在登入時啟動，因此無人值守的主機請啟用自動登入，並從本機 GUI 工作階段安裝，而非透過 SSH。runner 在 macOS 上會拒絕以 root 身分執行、安裝或啟動。launchd 啟動 agent 時使用精簡的 `PATH`；若 `/opt/homebrew/bin` 與 `/usr/local/bin` 存在，runner 會將其附加至路徑，以便找到透過 Homebrew 安裝的 `tart`。agent 會捨棄 stdout（runner 會自行寫入輪替日誌），並將 stderr 保留在 `~/Library/Logs/runner/stderr.log`；此檔案不會輪替。
- `/tmp/runner.lock` 的執行鎖可避免意外啟動第二個 runner；它不是防範本機使用者的安全邊界。

```bash
# Linux 新系統安裝：從設定產生器複製 TOML，存為 /etc/runner/config.toml，
# 並在主機上填入憑證。
sudo chmod 0600 /etc/runner/config.toml
sudo /usr/local/bin/runner service install --config-path /etc/runner/config.toml

# macOS 或 Linux 使用者安裝：將 TOML 存為 ~/.config/runner/config.toml，
# 並在主機上填入憑證。
chmod 0600 ~/.config/runner/config.toml
runner validate
runner service install --scope user
```

系統憑證必須在設定中明確指定。若使用 `token="env:NAME"` 或 App 的 `private-key-env`，請透過 `--environment-file /etc/runner/credentials.env` 指定受 root 保護的 systemd 環境檔；不會保存安裝 shell 的私人憑證。寫入 unit 前，會以服務身分執行 `runner validate`，用服務實際使用的身分檢查 Docker socket、憑證與 GitHub 存取；離線準備可用 `--no-start` 跳過即時探測。rootful Docker socket 僅透過 unit 的 supplementary groups 授權，仍具有等同 root 的能力，與 rootless daemon 不同；不會接管其他使用者的 rootless socket。

管理指令（`status`、`logs`、`doctor`、`service …`、`update`）會選擇唯一已安裝服務的 binary／config。可用 `--scope user|system` 明確選擇；`--user`、`--user=false` 保留為別名（不列在 help 中）。auto 模式遇到兩份安裝或無法確認的服務資訊會停止；安裝也會拒絕建立第二個範圍。探索只看得到 system 定義和執行帳號自己的 user 定義，所以全新安裝時若有其他 runner 持有執行鎖（例如另一個帳號的 user 服務或前景執行的 `runner run`），也會拒絕安裝。Linux 探索需要支援 JSON 的 systemd `busctl` 及對應的服務管理匯流排。明確 scope 不會退回其他安裝。完全沒有已安裝服務時，仍保留獨立執行的設定搜尋方式。由套件安裝在 `/usr/lib/systemd/system` 或 `/lib/systemd/system` 的 system unit 會像 runner 自己的 unit 一樣被管理，但 `uninstall` 絕不刪除它，而是說明如何停用；`install --force` 會把 runner 的 unit 寫到優先載入的 `/etc/systemd/system`，並重啟服務讓新定義生效。

`runner update` 更新選定服務的 binary，只有加 `--restart` 時才會重啟。系統 binary 更新需要 root，且目前的 updater 必須位於受 root 保護的路徑；可寫入的 user binary 不需要 sudo。若只更新指定 binary（包括 macOS 的 root 擁有共用 binary），可用 `sudo /absolute/path/current-runner update --binary-path /absolute/path/target-runner`，不會選擇或操作 LaunchAgent。系統 `doctor`／`doctor --fix` 請使用錯誤訊息提供的 `sudo systemd-run …`，重現服務 UID、unit 群組與憑證來源；單純 `sudo -u` 可能缺少 Docker 群組。`--fix` 仍要求 manager 已停止並取得共用執行鎖。私人 inline 環境值不會放入命令列，需先移至受保護的 EnvironmentFile。

既有 Linux root 服務是持續支援的安裝模式，在 `update`、`--force` 下保留 root 身分。升級不要求轉換執行身分；非 root 預設適用於新系統安裝。本次規劃不新增 root → 非 root 轉換指令；解除安裝再安裝也不會自動接管既有 root 資料。升級不會搬移或刪除既有設定與備份檔案。

<a id="systemd"></a>

## Systemd

這是 `sudo runner service install --scope system` 為採用預設排空逾時的 Docker 設定所產生的 unit。建議使用該指令，讓 unit 與二進位檔保持同步。

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

## 主機本機 Actions 快取部署

選用的[本機快取操作手冊](local-actions-cache.zh-TW.md) 說明獨立服務／資料 volume、
固定 digest 的 runner 變體、核准後的 canary 與回復。Manager 設定變更需要維護重啟；
不支援熱載入、各 scale set 停用開關，也不可另啟第二個 manager 繞過主機鎖。
保留其他 entry，重啟前確認執行中版本的 drain 行為與服務停止期限。Runner／doctor
清理會保留外部快取資料 volume，不將它視為可清空的 job 快取。
