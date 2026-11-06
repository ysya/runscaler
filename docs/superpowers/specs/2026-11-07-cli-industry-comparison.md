# CLI 與常駐服務的業界做法比較

日期：2026-11-07。以下為官方文件／原始碼核對，沒有實機安裝這些產品。

## 分開三個決策

安裝／更新 binary 的權限、常駐程序的 runtime UID，以及 CLI 管理的 service/config/daemon，必須分開。強制 root 不能保證多份 binary 或 config 會選對。

| 工具 | 官方做法 | 對本專案的啟示（推論） |
| --- | --- | --- |
| GitHub Actions Runner / Linux | svc.sh 是需 root 的 systemctl wrapper；安裝時將指定使用者（預設 SUDO_USER）寫入 unit 的 User=，並用安裝目錄的 .service 記錄服務名稱。 | system 安裝與 runtime UID 分開；操作綁定既有服務，而非從當次 PATH 猜。 |
| GitHub Actions Runner / macOS | darwin.svc.sh 拒絕 UID 0，安裝目前使用者的 LaunchAgent，記錄 .service。 | macOS 非 root 是正式部署路徑，不是需要消除的例外。 |
| GitLab Runner | Linux 手動安裝的 manager 服務以 root 執行，install --user 指定的是 job 使用者；root/user 命令的預設 config 不同。macOS 使用 LaunchAgent。 | 保留部署選項，但不可照抄 --user 語意；本專案的 boolean 是 service scope。 |
| Caddy | 官方 systemd 安裝採專用 caddy 帳號、固定設定／資料位置，使用 systemctl 管理。 | system service 不等於 runtime 必須 root；專用帳號可提供可預期的 ownership。 |
| Docker | CLI context 綁定 daemon endpoint；rootless 安裝可設定 rootless context，daemon 由 user systemd 管理。 | 選定 target 後沿用。單主機 runner 暫不需要完整 context database。 |
| Homebrew services | sudo 與非 sudo 選 system/user scope；info/list 支援 JSON。 | scope 應可見；但只靠 sudo 改目標仍可能重現目前混亂，不能原樣照搬。 |

來源：[GitHub Linux script](https://github.com/actions/runner/blob/main/src/Misc/layoutbin/systemd.svc.sh.template)、[unit template](https://github.com/actions/runner/blob/main/src/Misc/layoutbin/actions.runner.service.template)、[macOS script](https://github.com/actions/runner/blob/main/src/Misc/layoutbin/darwin.svc.sh.template)；[GitLab Linux](https://docs.gitlab.com/runner/install/linux-manually/)、[commands](https://docs.gitlab.com/runner/commands/)、[macOS](https://docs.gitlab.com/runner/install/osx/)；[Caddy service](https://caddyserver.com/docs/running)；[Docker contexts](https://docs.docker.com/engine/manage-resources/contexts/)、[rootless](https://docs.docker.com/engine/security/rootless/tips/)；[Homebrew services](https://docs.brew.sh/Manpage#services-subcommand)。

## 建議採用的方向

### 先修正 target 與路徑一致性

已確認的方向：Linux 新安裝預設 system 安裝、專用非 root 帳號執行；macOS 維持 user LaunchAgent；Linux user 安裝仍完整支援。日常管理以唯一既有 service 的有效設定作 binary/config 證據；多份或不明時停止猜測。status 分開顯示 scope、runtime UID、binary/config 路徑與 running version。

這採用「操作綁定既有安裝」與「明確 target」的原則，不代表其他工具都有相同的 auto-discovery。第一步不引入 context database、root control daemon 或 privileged helper。

### Linux runtime UID 的取捨（已確認方向）

| 選項 | 好處 | 代價 |
| --- | --- | --- |
| 新安裝採專用非 root 帳號的 system service（預設） | 類似 Caddy／GitHub Runner 的安裝權限與 runtime-user 分離；不依賴個人登入；固定 config/state/log 位置。 | 安裝／更新需要管理員權限；必須處理帳號、設定讀取、可寫目錄與 Docker socket。 |
| 使用目前使用者的 user service（可選） | user-owned binary/config，更新無需 sudo；可搭配同 UID 的 rootless Docker。 | 無登入開機常駐可能要設定 linger；仍須實機驗證 daemon 權限與限制能力。 |
| 既有 system service root UID（遷移前相容） | 一般更新不強迫既有主機即刻遷移 ownership。 | 不是新安裝目標；需要明確的備份／排空／身分遷移流程。 |

採第一項作 Linux 正式預設，第二項保留完整選用路徑；這次只更新規劃，不建立帳號或 chown。可存取 rootful Docker socket 的 user 仍有 root-equivalent 能力，不能宣稱已成為 rootless Docker；manager UID 與 daemon 模式要分開標示。[Docker 權限說明](https://docs.docker.com/engine/install/linux-postinstall/)

### macOS binary ownership 與服務身分分開

manager 保持登入使用者。binary 可位於管理員擁有的共用目錄，也可位於使用者目錄。GitLab macOS 文件就將 sudo 替換 /usr/local/bin binary 與非 sudo 服務操作分開。因此更新權限不足應提示精確 binary 更新目標，不能順便把服務改為 root。

## 與可靠性 1–4 的對照

| 項目 | 官方前例 | 本專案採用／限制 |
| --- | --- | --- |
| 就緒 | GitLab autoscaler 的 instance_ready_command 失敗可觸發回收。 | 採資源存在／實際可用的區分；GitHub 已派工保護仍優先，不能照搬刪除流程。 |
| 映像 | GitLab Docker executor 有 always/if-not-present/never pull policy。 | inspect 顯示本專案 Docker startup pull、Tart if-not-present 的差異；先加明確 inspect/probe/refresh，不另建背景更新排程。 |
| 重試 | GitLab 對 readiness 反覆失敗採指數退避，對 unhealthy worker 另有低頻探測。 | 分開 provisioning、pool、listener 失敗；其 job-request 預設值不能作為我們的 provisioning 數值證據。 |
| 指標 | GitLab Runner 內建 Prometheus /metrics；官方 Go client 提供 registry/histogram/handler。 | 提案新增官方 client 作一項有明確用途的直接相依套件，避免自製 exposition protocol。 |

來源：[GitLab autoscaler/advanced configuration](https://docs.gitlab.com/runner/configuration/advanced-configuration/)、[Docker executor](https://docs.gitlab.com/runner/executors/docker/)、[monitoring](https://docs.gitlab.com/runner/monitoring/)、[Prometheus Go client](https://github.com/prometheus/client_golang)。

數值預設與本專案 auto-selection 是待檢閱的設計判斷，非業界一致標準；沒有在本次研究中驗證任何 live service 變更。
