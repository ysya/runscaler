# 指令

[English](commands.md) | [繁體中文](commands.zh-TW.md)

本文件為 [英文版](commands.md) 的繁體中文版本；若內容有差異，以英文版為準。

所有指令的一行摘要請見 README 的[指令總覽](../README.zh-TW.md#commands)。

<a id="status-dashboard"></a>

## 狀態儀表板

`runner status` 會將本機 `/healthz` 快照轉為便於維運人員閱讀的儀表板。
摘要會彙整就緒的 runner 規模集、runner 生命週期（`provisioning`、`idle`、`busy`、`removing`）、
期望的 runner 數量、佇列深度，以及程序執行期間累計的工作計數。
此外，也會以 `in_use / limit` 顯示整台主機的 `concurrent` 容量。
接著，各 runner 規模集會分別顯示連線狀態、上次成功連線時間與最近一次錯誤。
磁碟區段會顯示可快速取得的檔案系統剩餘空間；若需要較耗時的各儲存區用量測量，請使用 `runner cache`。

```bash
# Human-readable dashboard; color is enabled automatically in a terminal
runner status

# Stable machine-readable health response
runner status --json

# Plain dashboard for logs or terminals where color is undesirable
runner status --no-color
```

`/healthz`（以及 `status --json`）中各 runner 規模集的選用 `runtime` 物件，
會回報啟動嘗試次數、失敗次數、連續失敗次數、上次嘗試與成功啟動時間、
等待全域容量的狀態，以及排空狀態。計數器的有效期間為管理程序的生命週期。
若缺少啟動時間戳記，表示尚未記錄過該事件。失敗原因會使用安全的分類
（`jit_config_failed` 或 `provider_start_failed`），不會包含供應器的原始錯誤或憑證值。

`/healthz` 仍是存活檢查端點，會回傳 HTTP 200。當監聽器中斷連線、
控制器正在排空，或啟動失敗後仍有未滿足的需求時，其 JSON `status` 會變成 `degraded`，
而 `/readyz` 會回傳 HTTP 503。尚未完成的重試不會恢復就緒狀態；成功啟動後才會恢復。
正常等待主機全域容量、縮減至零，以及需求消失後留下的歷史失敗紀錄，都不視為故障。
取得執行階段快照不會向供應器或 GitHub 發出請求。儀表板會以 `ERROR`、
`WAITING FOR CAPACITY` 或 `DRAINING` 說明這些狀態，並附上啟動計數與時間戳記。

<a id="updating"></a>

## 更新

替換現有安裝前，請先閱讀[升級前準備](../README.zh-TW.md#upgrade-requirements)與目標版本的發行說明。
舊指令、設定名稱與憑證環境變數可能需要先手動調整。

```bash
# Check for a release and review its upgrade requirements without installing
runner update --check

# After preparing the installation, update its selected binary
runner update

# Or update, validate the config with the new binary and restart the service
runner update --restart
```

`runner update` 會顯示選定服務的安裝範圍、執行身分、binary 與設定檔，下載適合平台的
封存檔、驗證 `checksums.txt` 中的 SHA-256，再以不可分割的操作替換 binary。
沒有加 `--restart` 時，這個流程不會驗證設定／provider 相容性或重啟服務，也不會重寫服務定義；既有 system／user
範圍與執行身分會保留，原本以 root 執行的服務不會被轉成非 root 帳號。

安裝後會列出完成升級的步驟：先用新 binary 驗證選定的設定檔，再重啟服務，並依設定的
`drain-timeout` 說明重啟最多會等執行中的工作多久。非 root 的 system 服務會跟 `doctor`
一樣，透過 `systemd-run` 以服務的身分、群組與環境檔執行驗證指令。提示會寫出選定的
binary 與 `--scope user|system`，但會省略多餘的部分：不需要 sudo 的提示，若 PATH 上
找到的就是這支 binary，會直接寫 `runner`；選定的安裝是唯一一個時，則省略 `--scope`。
system 服務管理需使用管理員 shell。健康端點回報相同版本，不代表設定相容性或就緒狀態已驗證。
健康端點停用或無法連線時，會明示執行版本未確認，不會假定服務已停止。
binary 已是最新版、但服務仍回報舊版本時，`runner update` 會再列一次這些步驟。
使用 `--binary-path` 或沒有安裝服務時，不會選取服務；驗證後請手動重啟使用該 binary 的程序。

`runner update --restart` 會自己執行這些步驟：先用新 binary 依服務的執行方式驗證設定，
通過後才重啟服務，驗證失敗時不會動到正在執行的服務。健康端點已回報新版本時不會重啟；
健康端點無法回報版本時，只有剛裝了新 binary 才會重啟。`--restart` 不能和 `--check` 或
`--binary-path` 一起使用。

SIGTERM 通常會開始排空並等待工作完成，但仍受 `drain-timeout` 與服務管理器停止期限限制。
停用排空或任一期限到期時，工作可能被終止；重啟前請先確認這些設定。

上述提示來自目前已安裝的更新器；舊更新器在第一次升級時無法顯示新加入的警告。
首次跨越這次變更時，請先備份原 binary 與設定檔，將新版下載並解壓至另一個路徑，
在預定執行環境中用新 binary 驗證現有設定，再替換服務使用的 binary：

```bash
/path/to/new/runner validate --config /absolute/path/config.toml
```

runner 規模集在重新啟動期間會維持註冊，因此當 runner 因升級、重新開機或當機而停止時，
排入佇列的工作會在 runner 重新連線後立即被接手。
runner 不會自行刪除 runner 規模集：當你停用某個 `[[scaleset]]` 時，
請先停止 runner，再使用 `runner scaleset delete <name>` 從 GitHub 移除它。

listener 斷線時，runner 會在五秒後重新連線，之後每次等待加倍，最長一分鐘。
若被 GitHub 速率限制，會依 GitHub 要求的時間等待；GitHub 沒有給時間時等一分鐘，最長不超過一小時。

<a id="troubleshooting-with-doctor"></a>

## 使用 `doctor` 疑難排解

若 runner 意外終止（例如 `kill -9`、當機或斷電），可能會遺留 Docker 容器或 Tart VM。
下次執行 `runner run` 時，會在取得整台機器共用的鎖定後自動清理這些資源。
可以用 `doctor` 手動檢查。有 runner 持有這個鎖時，doctor 會把它擁有的容器和 VM
標成使用中，不算孤立資源：

```bash
# Check GitHub API connectivity and orphaned resources
runner doctor

# Auto-remove orphaned containers and VMs
runner doctor --fix
```

Doctor 會使用各個已設定 runner 規模集的 App 或 PAT 憑證探測 GitHub API、
解析其 runner 群組，並查詢其註冊狀態。每次探測的逾時時間為五秒；
即使失敗，也不會阻止後續探測或本機資源檢查。取消操作會停止後續探測。
若 runner 規模集尚未註冊，僅會回報狀態，不會建立或修改它。

接著，Doctor 會列出每個已連上的 runner 群組，回報其中已註冊、
但這份設定中沒有任何相同註冊 URL 的 `[[scaleset]]` 使用的 runner 規模集。
這些規模集可能由其他主機執行；若沒有主機執行，指定它的工作會一直排隊，
因為 runner 不會自行刪除 runner 規模集：請把它加回設定成為 `[[scaleset]]`，
停止 runner，再執行 `runner scaleset delete <name>`。
找到這類規模集不會讓 doctor 失敗，但列出失敗會。

找不到設定檔時，doctor 會跳過 GitHub API 檢查並列出找過的路徑；`runner validate`
則會列出同樣的路徑並以失敗結束。即使沒有孤立資源，API 檢查失敗仍會讓指令以失敗狀態結束。
報告僅包含安全的取消、逾時、DNS、TLS、速率限制或數字 HTTP 狀態分類，
速率限制還會附上 GitHub 要求的等待時間；不會輸出原始 SDK 回應本文。

`--fix` 旗標會取得與 `runner run` 相同的全機鎖定，因此只要目前仍有 runner 執行個體
正在執行，就會拒絕移除資源。
啟動時的資源清理或 `doctor --fix` 都不會移除共用磁碟區與快取磁碟區：
這些磁碟區原本就會保留至程序結束之後，而且可能仍包含執行中工作流程的後續工作所需的資料。

啟動時的資源清理與 `doctor` 採用相同的辨識規則：容器須帶有 `managed-by=runner` 標籤，或名稱正好是 `runner-<8 位十六進位>`；本機 Tart VM 的名稱須正好符合 runner 產生的 `runner-`、`pool-` 或 `runner-keep-` 格式。其餘資源無論叫什麼名字都不會被移除。

<a id="logs"></a>

## 日誌

`runner run` 會將日誌寫入 stdout 與輪替檔案（10 MB，另保留一份備份）。
檔案的預設位置取決於執行身分：

| 執行身分 | 日誌檔 |
| --- | --- |
| Linux 系統服務（專用帳號或 root），或 `sudo runner run` | `/var/log/runner/runner.log` |
| Linux 使用者 | `$XDG_STATE_HOME/runner/runner.log`（`~/.local/state/runner/runner.log`） |
| macOS 使用者 | `~/Library/Logs/runner/runner.log` |

可將 `log-file` 設為明確的路徑，或使用 `log-file = ""` 停用檔案日誌。
以 root 執行時，`log-file` 必須位於 `/var/log` 下的目錄，且目錄名稱僅能使用
英文字母、數字、`.`、`_` 或 `-`；若不符合，會改用預設位置並發出警告。
v0.10 之前的版本會將 `runner.log` 寫在設定檔旁，該檔案會保留在原處。

只有 stdout 是終端機時，stdout 上的日誌輸出才會使用色彩；在 systemd、launchd、
未加 `-t` 的 `docker run`、CI 或管線中，則維持純文字。日誌檔一律不包含色彩控制碼。
可設定 `NO_COLOR=1` 關閉色彩，或設定 `CLICOLOR_FORCE=1` 強制啟用色彩。

```bash
runner logs                 # last 100 lines (add sudo for a system service)
runner logs -n 500 -f
```

每台主機只能有一個 `runner run` 程序在執行。請在該程序中使用多個 `[[scaleset]]`
項目，納入所有組織或儲存庫。
## Runner 啟動與失敗復原

Docker runner 會保持 provisioning，直到日誌出現 `Listening for Jobs`，且容器再存活五秒。
GitHub 的工作指派或完成事件也能證明啟動成功。預設等待五分鐘；可用
`docker.ready-timeout` 調整。自訂 entrypoint 無法輸出該訊息時，可明確設定
`docker.readiness = false`；狀態會顯示 `unverified` 並記錄警告。
Docker 用戶端以非 root 執行，不代表 Docker daemon 本身是 rootless。

啟動失敗後，第一次等待 2.5–5 秒，之後加倍並限制於 `retry.max`（預設一分鐘）；
連續失敗五次後，進入五分鐘冷卻。可在全域預設或個別規模集的 `[retry]` 設定
`initial`、`max`、`cooldown-after`、`cooldown`。新的需求回呼無法繞過期限，
佇列為空時仍會補足最低 runner 數。確認復原只會重設對應的失敗 generation。
Tart 暖池各 slot 分別記錄重試；工作執行期間退出與清理重試不會累加啟動失敗。
`runner status` 與健康 JSON 會顯示就緒狀態、重試期限及安全的失敗資訊。

失敗診斷預設啟用，放在實際開啟的日誌檔旁的 `diagnostics/`，包含 fallback 路徑。
停用檔案日誌時使用該安裝範圍的預設日誌目錄。目錄與檔案權限分別為 0700、0600，
最多保留 20 份、2 MiB、七天，記錄已知的退出碼、OOM 與結束時間；未知值保持未知。
清理失敗時保留容量，直到成功移除。無法確認 GitHub 身分的 runner，會保留到工作事件
允許清理或明確強制關機。

原始日誌預設不收集。設定 `diagnostics.capture-logs = true` 才會收集節錄
（最多 100 行、64 KiB），並遮罩已知 PAT／JIT、Authorization 標頭與 PEM 區塊。
這無法辨識所有工作流程機密；工作可能輸出敏感資料時請維持關閉。
`diagnostics.enabled = false` 可停用診斷報告。健康 JSON 不會包含原始日誌，
診斷收集或寫入失敗也不會阻止清理。
