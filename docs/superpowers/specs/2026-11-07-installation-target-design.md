# 安裝目標與 root/user 操作設計草案

> Historical record: rename migration and compatibility requirements in this document were superseded by [legacy retirement](../plans/2026-11-07-retire-legacy-compatibility.md). Do not execute the retired migration/compatibility steps. Current runner root/user installation behavior remains supported.


日期：2026-11-07

狀態：安裝模式與 CLI 已實作；歷史更名相容規劃已由退場計畫取代。
使用者要解決的問題：安裝、更新及檔案權限容易混亂；日常指令需要反覆選擇 user/system。

使用者已確認：Linux 預設系統安裝、以專用非 root 帳號執行；macOS 維持 user 模式；Linux 保留 user 安裝選項。以下完整描述此方向，不再採 Linux 新安裝預設 user service 的前一版提案。

官方工具比較與來源見 [CLI industry comparison](2026-11-07-cli-industry-comparison.md)。實作任務見 [installation target plan](../plans/2026-11-07-installation-target.md)。service installation scope、runtime UID 與 CLI target 分開；專用服務帳號現在是 Linux system 安裝的規劃範圍。

## 現況：能安裝兩份，不代表能同時執行

已核對 `cmd/runner/cmd_service.go`、`config_paths.go`、`cmd_update.go`、`platform_guard.go` 與 `internal/lock/lock.go`：

| 問題 | 目前行為 |
| --- | --- |
| Linux root/non-root | 都能執行 manager，但共用 `/tmp/runner.lock` 的 exclusive flock；正常啟動路徑只有一個成功。鎖不是對抗本地惡意使用者的安全邊界。 |
| Linux system/user service | 定義位於不同 scope，可同時存在；安裝/管理只看指定 scope，沒有先找出唯一既有安裝。兩份都啟動時會競爭鎖，失敗者的服務管理器可能反覆重啟。 |
| `--user` | 指的是服務 scope；不是「自動降權」。root 執行 `--user` 仍是 root 的 user manager，不會自動變成 sudo 前的使用者。 |
| macOS | run/init/install/migrate 等路徑已拒絕 root；採登入使用者 LaunchAgent。舊 system 定義保留檢查及清理能力。 |
| 預設 scope | `defaultUserService()` 只看 OS：macOS user，Linux system；不根據已安裝服務自動選擇。 |
| 更新 | `runner update` 替換 `os.Executable()`，不是從 service definition 找 ExecStart。PATH 有兩份 binary 時可能更新錯份。 |
| 設定 | 一般 CLI 搜尋 current dir、user、system 等路徑；服務持有絕對 config path。CLI 不一定使用同一份 config。 |
| 系統安全約束 | Linux root service 要求 binary 與所有祖先目錄只能 root 修改；這個檢查必須保留。 |

## 已確認的部署方向

| 模式 | 安裝／更新 | 常駐程序 | 預設路徑 |
| --- | --- | --- | --- |
| Linux system（預設） | 管理員以 root 安裝／更新 | systemd system service，專用非 root 帳號 | binary `/usr/local/bin/runner`；config `/etc/runner/config.toml`；state `/var/lib/runner`；log `/var/log/runner`。 |
| Linux user（可選） | 目前使用者安裝／更新 | 目前使用者的 systemd user service | binary `~/.local/bin/runner`；config/log/state 沿用 XDG user layout。 |
| macOS user（預設且唯一新服務模式） | 目前使用者安裝／更新 | 登入使用者的 LaunchAgent | binary `~/.local/bin/runner`；user config、`~/Library/Logs/runner` 與 user state。 |

新 Linux system 安裝不因呼叫者是 root 而把 runtime UID 設為 root。管理員權限只用於安裝、更新、帳號／目錄準備及服務管理；manager 不在啟動後自行提權。既有 Linux root service 持續支援，升級、--force 及歷史更名遷移均保留原執行身分，不要求轉為非 root。

不增加 privileged helper、不自動 sudo、不掃描或代管其他使用者的 home。唯讀命令盡可能在原使用者權限執行；遇到 config/log 不可讀時報告權限限制，不為此放寬機密檔案權限。

### Linux system 的帳號與目錄契約

以下名稱／權限是實作建議值；部署模式本身已確認：

- 專用 system 帳號／群組暫定 `runscaler`，非 root UID、不提供互動登入，home 與 WorkingDirectory 為 `/var/lib/runner`。若同名帳號已存在，檢查其用途、UID、group 與 home；不符合就中止，不重新定義或自動接管既有帳號。
- 新的非 root system unit 設 `User=runscaler`、`Group=runscaler`，保留 NoNewPrivileges、ProtectSystem 與 drain stop timeout。state/log 目錄由新安裝的明確檢查與準備流程處理，unit 只透過 `ReadWritePaths` 開放已核對的 state/log 目錄與共用 lock 所需 `/tmp`。
- 新非 root unit 不使用 `StateDirectory`／`LogsDirectory` 來接管既有 tree，避免 systemd 在 owner 不符時於啟動前自行遞迴 chown。新安裝的預備流程檢查整個待管理子樹；若存在 root-private backups、其他擁有者的資料或用途不明的子項目就中止，不搬移或 chown 既有資料。既有 root 服務繼續使用原 tree。
- binary 及所有父目錄仍只允許 root 修改。`/etc/runner` 建議 root:runscaler 0750；config 及 App key 建議 root:runscaler 0640。manager 只能讀取設定／key，不能替換自身 binary 或修改 system config。
- state/log 目錄由服務帳號寫入，建議 0750；diagnostics 子目錄／檔案仍為 0700/0600。custom log path 必須與 LogsDirectory、runtime 解析結果及實際 UID 存取相符。
- 新 system layout 的管理員備份位置為 `/var/backups/runner`，root-only 0700，檔案 0600；不能放在服務帳號可替換父目錄的 state tree。既有 root 服務的 `/var/lib/runner/backups` 保持原位置與擁有權，不由新安裝接管。
- runtime layout 不能再只以 EUID==0 判斷 system/user。service 需記錄明確 scope，並傳遞相同的有效路徑；metadata 僅決定 layout，不授予 OS 權限。system scope 的非 root manager 仍須使用 `/etc`、`/var/lib`、`/var/log`，不能意外轉到 `/var/lib/runner/.config`。
- config/App key/環境憑證來源必須以實際服務身分檢查可用性；root 安裝 shell 能讀不代表 service 能讀。不自動複製個人 shell 的 token、SSH key 或私人 home 權限。

### 程序非 root 與 Docker daemon 分開呈現

Docker 後端另列 daemon rootless/rootful/unknown；一般使用者存取 rootful socket 不能標示為完整 rootless。macOS Tart 以使用者執行，不套用 Linux Docker rootless 的名詞或需求。

Linux system 服務必須以專用帳號實際驗證設定 socket 的連線。若需要 socket 所屬群組的存取，以 unit 的明確 SupplementaryGroups 授予所選 daemon 必要群組；不得自動把 socket 改成 world-writable、chown socket 或加入 root 群組。rootful Docker socket 的 root-equivalent 能力須在安裝摘要清楚呈現，不能宣稱專用 user 已形成完整隔離。

目前 Docker client 明確使用 config 的 unix socket，會蓋過 FromEnv 的 host；僅執行 `docker context use rootless` 不代表 runner 已切換。Linux user 模式可選同一 UID 的 rootless socket（通常為 `$XDG_RUNTIME_DIR/docker.sock`），在 init/install 確認後記錄絕對路徑。explicit docker.socket 永遠優先，不得在失敗時靜默改用另一個 daemon。

如果 system 服務設定到另一個個人 UID 的 rootless socket，安裝中止並說明改用相同使用者的 user 安裝，或先明確準備專用帳號自己的 daemon；不代為啟用該 daemon、linger 或重寫 socket 權限。system 模式不要求 Docker daemon 本身必須 rootful。

Docker rootless 相容性另有實機 gate：API/JIT runner、socket-sharing、cache volume/helper 權限、cleanup，以及 CPU/memory/pids 限制。doctor 顯示 manager UID、service scope、socket、daemon 模式與限制能力，不能把加入 docker group 當成 rootless 設定的預設修復。

### Linux 開機啟動

預設 system service 由 systemd 啟動，runtime 仍是專用非 root 帳號，不依賴個人登入或 linger。可選的 user service 若要求無登入開機常駐，需另檢查 linger 並提示依主機政策設定；不自動修改。user manager 不可用時明確回報，不默默改裝 system service。

## 日常指令的目標解析

管理範圍包含 service status/logs/start/stop/restart、manager status/logs、update，以及新 image 指令。互動式 `runner run --config ...` 與 config validate 保留現有顯式使用方式。

新增共用 target resolver，讀取 system service 以及「目前 effective UID」的 user service。結果包括 scope、service definition owner、runtime UID/GID、service identifier、binary absolute path、config absolute path、service state。system service 不能因 runtime UID 非 root 就被分類成 user service。資料來源為 service manager 的有效設定；不能靠任意掃描 PATH 或目前目錄猜測。

| 可見結果 | 預設行為 |
| --- | --- |
| 唯一已安裝 service | 所有管理指令選它，使用它的 binary/config；執行前簡短顯示目標。 |
| 無已安裝 service | service management 回報未安裝；`update` 保留更新目前呼叫 binary 的 standalone 模式，明確顯示路徑。status/logs 可使用明確 config 的前景模式。 |
| system 與目前 UID 的 user service 都存在 | 唯讀總覽列出兩者；變更指令報衝突並要求顯式 scope，不能依「哪個碰巧先搶到 lock」自動決定，也不能自動刪除其中一份。 |
| auto 模式無法確認某個候選 scope | 視為 unknown，不可當作不存在。變更指令中止，列出精確的查詢/提權方式。 |

建議提供 `--scope auto|user|system` 作為管理指令一致的 override，預設 auto；保留 `--user`/`--user=false` 相容 alias。兩者同時指定且矛盾時報錯。macOS system override 僅允許既有 legacy service 的查詢/停止/移除，不能新建或啟動 root manager。

顯式指定 scope 的既有服務管理只要求所選目標可確認且已驗證，不因另一 scope unknown 阻止操作；不得在所選目標不存在時回退另一份。新安裝仍要求所有可見候選 scope 完成檢查，避免用 explicit scope 繞過重複安裝保護。

Linux 非 root 呼叫 `runner service restart` 若唯一目標是 system，不會轉而操作 user service；應列出目標、說明需要 root，再印出包含絕對 binary path 及明確 system scope 的可複製指令。無自動提權或隱藏重啟。

## 更新與 sudo

- 有唯一服務時，update 使用其實際 executable，先解析 symlink 並保留 root-only chain 檢查；不是盲目替換目前 PATH 命中的 binary。
- root 權限不足時，在下載前就提示精確的提權命令；不得先寫入另一個 user binary 作為替代。
- `sudo` 之後不推定使用者想管理 root 的 user service，也不透過 SUDO_USER 自動切換 UID、重建 HOME 或載入任意使用者憑證。
- 若 sudo 後的 scope 與原本提示目標不符、或找不到原本 service，fail closed；使用者以提示中的 explicit scope/binary/config 操作。
- 使用者明確給定 `--config` 且與既有 service 不同時，唯讀指令需顯示 override；會修改 service/installed binary 的指令報衝突，要求先解決目標不一致。
- 保留 update 與 service restart 的分離；更新不代表已重啟套用。

為了涵蓋 macOS「binary 由 root 擁有、服務由一般使用者執行」，提案為 update 增加顯式 `--binary-path PATH` 模式，只替換指定 binary，不解析或操作服務。權限不足的原命令印出包含絕對 updater 路徑與這個 flag 的 sudo 指令；sudo 後不會誤選 root user service。顯式 binary target 與 scope override 互斥；沒有 explicit binary target 時，才依上述 service/standalone 規則解析。root 目標要通過 root-only trust chain，指定路徑不是跳過信任檢查的出口。

服務資料必須以 systemd properties 或 property-list parser 讀取；不把 service 檔案當 shell 執行，不以簡單空白分割 ExecStart。路徑包含空白、引號、symlink 需有測試。提權命令完整引用，root 更新目標必須仍滿足 root-only trust chain。

## 新安裝與衝突處理

新安裝前檢查可見的另一 scope；發現既有安裝就拒絕重複安裝，列出現有 binary/config 與 scope。`--force` 只重建同一份 service，不能繞過跨 scope 衝突或變更 runtime UID。

沒有既有 service 時，Linux 預設 system、macOS 預設 user。Linux 一般使用者要求預設 system 安裝時，提示精確 sudo 指令與 `--user` 選項，不自動提權，也不自動退回 user。Linux user 可用既有 `--user` 或新 `--scope user` 明確選一次；root 執行 user 安裝時拒絕建立 root 的 user service。已安裝後的管理依 target resolver，不要求每次重複選模式。

installer/help/README 必須區分「只下載 binary」與「服務模式」。目前 install.sh 依 EUID 決定 binary 位置，因此 Linux system 指引需明確使用 root-owned binary 安裝路徑；不能讓使用者先安裝 user binary，再以 system install 意外混用。user 模式文件保留不需 sudo 的完整路徑。

不能保證發現其他 UID 的私有 user service；文件及錯誤需明說 discovery 僅涵蓋 system 與目前 UID。runtime machine lock 仍是避免正常程序同時執行的最後保護，不宣稱可無權限掃描整台主機。

遷移既有雙重安裝是獨立維護動作：列出兩份服務、辨識執行中的工作、備份、排空、停用選定舊服務、驗證保留服務。須使用者明確選擇，不納入自動安裝/更新副作用。

### 既有 Linux root system service 的相容性

既有 root 執行是持續支援的模式；非 root 預設適用於新 Linux system 安裝。一般升級不改 UID/GID、不接管既有資料，也不要求使用者執行身分轉換。

先前的 `runner service migrate` 身分轉換提案撤回，不列為未完成項目或下一階段必做功能。未來只有使用者明確要求時才另行設計；既有 `runner migrate` 維持歷史更名與設定遷移語意。

一般 update 只換 root-owned binary，不做 UID 遷移；--force 遇到既有 root runtime 保留 UID，無須提示轉換指令。狀態顯示反映實際 runtime：既有 root 仍顯示 root，新安裝才顯示專用非 root 帳號。

離線 image probe/refresh 及需要讀取服務私有資料的診斷，必須重現 target 的 runtime UID/GID、unit 專屬 SupplementaryGroups、HOME、WorkingDirectory、scope/path metadata 及該操作必要的設定環境。單純 `sudo -u runscaler` 不足，不能用它作通用修復提示。

system 模式改為產生可檢閱的 `sudo systemd-run` transient service 指令，以固定絕對 binary/config、明確 User/Group/SupplementaryGroups、受限可寫路徑執行；使用 wait/pipe/collect 取得結果及回收 transient unit，不啟用新常駐服務、不使用繼承呼叫者環境的 transient --scope。不得帶入 StateDirectory/LogsDirectory 的遞迴 ownership 副作用。

只轉移 allowlist 中、由有效 service definition 確認的必要環境。機密必須由已設定且可驗證的 EnvironmentFile／credential source 提供，不展開到命令列、提示、manifest 或 logs；無法在不暴露機密下重現就中止，不回退 sudo -u。禁止複製任意 ExecStartPre 或其他 unit 動作。指令產生器須處理 shell quoting 及 systemd 的 $ expansion，先檢查主機支援所需 systemd-run 功能。CLI 不隱藏 sudo/UID 切換，不讀另一個個人帳號的 Docker/Tart 檔案，也不為維護命令永久修改帳號群組。

## 驗收條件與程式範圍

建議集中在 `cmd/runner/installation.go` / `installation_test.go`，與現有 service manager 的讀取方法整合；不要把 discovery 邏輯複製到每個 command。

- Linux 只有 user service：省略 flag 的 status/logs/restart/update 全部選同一份，且不誤用 `/etc/runner` config。
- Linux 新 system 安裝：管理員安裝／更新 root-owned binary，unit 有非 root User=/Group=，state/log 可寫、config/key 可讀但不可改，日常 manager 不需提權。
- Linux 一般使用者省略 scope 的全新安裝：要求 system 安裝權限，提示精確命令及 user 選項；沒有隱藏提權或 scope fallback。
- Linux --user/macOS 預設安裝：binary/config/logs 使用 user layout，更新不需 sudo；root 不得建立 root user service。
- Linux user manager 或 linger 不符合無登入啟動需求時，明確說明缺少條件，不自動改裝 system service或修改主機政策。
- Linux 只有 system service：一般使用者可看到目標；需要寫入時得到精確提權提示，沒有任何變更。
- 兩個 scope 都存在：列出雙方路徑；任何自動選擇的 mutation 都失敗。
- macOS 維持非 root；sudo 不會建立 root 的 LaunchAgent 或讀 `/var/root/.tart`。
- `--user` 舊用法仍可用；矛盾 flags、explicit config 與 service config 衝突會明確報錯。
- 兩份不同 binary 的情境：update 修改 service 所指向者，另一份保持原樣。
- discovery 讀取失敗、其他 UID service、unit override、複雜 ExecStart、symlink、含空白路徑及 root-only chain 均有測試。
- 非 root system scope 的 runtime 及 CLI path resolution 不會誤用 user XDG layout；root-only binary trust 檢查不因 User=runscaler 被取消。
- state/log tree 有未列入 manifest 的子項目時中止；新 unit 與維護 transient unit 不以自動 directory directives 遞迴改 ownership。正常啟動與 rollback 都不得觸碰排除項目。
- systemd unit 的專屬 Docker supplementary group 會在離線維護重現，HOME/scope/credentials 一致；僅 sudo -u 無法存取的 fixture 必須仍能經產生的 transient-service 指令正確存取，且不永久修改群組。
- 既有 Linux root system unit 的 update/--force 及歷史更名遷移保留 UID 與資料擁有權，升級不要求轉為非 root。
- 不執行隱藏 scope/UID migration、不更動 lock policy。
- rootless Docker 與 rootful Docker 的 endpoint 不會自動互相回退；manager 非 root 與 daemon rootless 狀態分開驗收、呈現。

此規劃作可靠性四項之前的獨立改善；部署方向已確認，帳號名稱、檔案權限與遷移指令等詳細契約仍供檢閱。本次只更新文件，不建立帳號、chown、停止服務或執行遷移。

## 官方參考

- [Apple root and login sessions](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPMultipleUsers/Concepts/SystemContexts.html)：root session 與登入 session 是不同執行環境。
- [systemd loginctl](https://www.freedesktop.org/software/systemd/man/255/loginctl.html)：user service 的 linger 與 runtime UID 是不同問題。
- [Docker post-installation](https://docs.docker.com/engine/install/linux-postinstall/)：可存取 rootful Docker socket 的 docker group 具有 root-level privileges；non-root manager 不等於 rootless Docker。
- [Docker rootless](https://docs.docker.com/engine/security/rootless/) 與 [rootless tips](https://docs.docker.com/engine/security/rootless/tips/)：user service、socket 路徑與 cgroup 限制條件；部分主機準備可能需要管理員權限。
- [systemd.exec](https://www.freedesktop.org/software/systemd/man/255/systemd.exec.html)：User/Group、StateDirectory/LogsDirectory、執行環境與目錄權限。
- [systemd-run](https://www.freedesktop.org/software/systemd/man/latest/systemd-run.html)：transient service、properties 與明確執行環境；實作時仍須檢查主機功能版本。
