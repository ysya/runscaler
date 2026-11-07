# Runner 可靠性與可觀測性設計草案

日期：2026-11-07

狀態：提案，供檢閱；尚未核准實作
基準：`9a0d388`。以下預設值與新介面是建議，不代表使用者已同意。

## 目的與範圍

針對目前「有工作排隊但 runner 無法接手」的維運缺口，規劃四項改善：

1. Docker runner 就緒確認與失敗診斷。
2. 映像資訊檢查及明確的更新操作。
3. 啟動失敗的退避與冷卻。
4. Prometheus 指標及等待時間量測。

維持單一主機、單一管理程序、Docker/Tart 混合 scale set 的定位。這次不實作、不安裝相依套件、不操作服務、不提交或發佈。

安裝、更新與 root/user 目標混亂的獨立提案見 [installation target design](2026-11-07-installation-target-design.md)。該提案可先落地，但不是四項可靠性功能的必要架構依賴。

已確認部署方向為 Linux 系統安裝、專用非 root 帳號執行，macOS user 模式，另保留 Linux user 安裝。四項功能必須可由目標非 root runtime UID 執行，正確使用其 service scope 與授權目錄；system binary 安裝／更新仍由管理員處理。既有目前 runner 格式的 Linux root 服務持續支援並保留原身分，不要求或規劃轉為非 root；新帳號準備由獨立 CLI 安裝流程處理。rootless Docker 另有相容驗收，不把 manager 非 root 等同 daemon rootless。

業界比較及採用理由見 [CLI industry comparison](2026-11-07-cli-industry-comparison.md)。比照成熟 runner 的就緒、退避、pull policy 與 metrics 分工；不照抄不適用本專案派工契約的資源回收動作。

## 已核對的現況

| 範圍 | 現況與來源 |
| --- | --- |
| Docker | `internal/provider/docker.go` 的 `StartInstance` 在 `ContainerStart` 成功後返回；`WaitInstance` 沒有將容器 exit code 帶回 controller。 |
| Tart | `internal/provider/tart.go` 已等候 `Listening for Jobs` 並觀察首次輪詢後是否存活；`EnsureImage` 在本地已有同名映像時跳過 pull。 |
| 生命週期 | `internal/controller/state.go` 已有 provisioning/idle/busy/removing、capacity lease，以及 provisioning 中收到 job event 的處理。 |
| 派工競態 | `controller.deregister` 透過 GitHub `RemoveRunner` 判斷看似 idle 的 runner 是否實際已被派工；不能只依本地 JobStarted 判斷。 |
| 重試 | controller 啟動失敗目前排程 5 秒後 reconcile；listener 重連已有自己的 5–60 秒退避，兩者不同。Tart warm-pool 建立失敗也有自己的 5 秒重試。 |
| 監控 | `internal/metrics` 已有工作計數、需求量與 runtime 狀態；health server 只有 `/healthz`、`/readyz`。 |
| SDK 時間 | `actions/scaleset@v0.4.0/types.go` 的 JobMessageBase 有 QueueTime、ScaleSetAssignTime、RunnerAssignTime、FinishTime；沒有真正的 job execution start timestamp。 |
| 發佈 | GoReleaser 已設 `CGO_ENABLED=0`；這次不包含更新備份與 rollback。 |

## 共同約束

- 每台主機維持一個管理程序，沿用 `/tmp/runner.lock`；不可改成 root/user 各一把鎖。
- 診斷、映像檢查／probe／refresh、重試及 metrics 不得要求 manager 為 root；沿用其 service scope、runtime UID 可寫的授權目錄與明確 provider socket/home。system scope 的路徑不能由 EUID==0 推定。
- 不阻塞 scaleset listener callbacks。callbacks 只更新記憶體狀態及喚醒 worker。
- provisioning/removing 期間仍保有容量；只有生命週期收尾成功才釋放 lease，且釋放須冪等。
- 不搶占 busy job。就緒逾時不等於允許直接刪除容器。
- 維持 SIGTERM/SIGQUIT drain、SIGINT 立即停止與現有 drain-timeout 邊界。
- `/healthz` 維持 HTTP 200 liveness；`/readyz` 不因正常容量等待、零需求或無關的歷史失敗變成 503。
- health/metrics HTTP request 不得呼叫 Docker、Tart、GitHub API、啟動 helper 或遍歷磁碟。
- health JSON 只增加相容欄位；新 TOML key 必須遵守既有明確零值及 per-scaleset inheritance 規則。
- 兩份 README 同步更新。程式、CLI 文案、測試名稱、agent instructions 與 commit messages 使用英文。
- 第一版僅提案新增 `github.com/prometheus/client_golang` 一項直接 runtime dependency，用維護中的 registry/histogram/handler 避免自行實作 exposition protocol。版本於實作時核對、固定並執行漏洞檢查；規劃階段不安裝。其餘沿用現有 dependencies。

## 1. Docker 就緒與失敗診斷

### 1.1 就緒定義

預設在容器成功啟動後繼續停留 provisioning，直到以下任一條件成立：

- 觀察到 Actions runner 的 `Listening for Jobs`，且容器再存活 5 秒。
- 已收到該 runner 的 JobStarted 或 JobCompleted；工作事件是更強的成功證據，不需要再等 stdout。
- GitHub 取消註冊回覆 JobStillRunning，表示該 runner 已被派工，必須保留為 busy。

新增 per-scaleset `[docker] readiness = true`、`ready-timeout = "5m"`。timeout 必須大於 0；`readiness = false` 是特殊自訂映像的相容出口，輸出警告，並清楚顯示其 readiness 未經確認。預設沿用 Tart 的五分鐘及五秒存活觀察，實機驗收後才調整。

Docker non-TTY log 必須經 `stdcopy` 解多工；支援訊息跨 chunk、stdout/stderr、超長單行、context cancellation 與不支援讀取的 logging driver。解析緩衝有界，不能讀取整份 job log。log 不可用時以 job event 作成功證據；deadline 到期則回報 `readiness_unavailable`，不能默認成功。

### 1.2 控制權與生命週期

建議擴充 optional provider capability `InstanceReadiness.WaitReady(ctx, instanceID) error`，Docker 實作；Tart 先維持原本 StartInstance 的等待邏輯。既有 InstanceProvider、InstanceWatcher 的契約保留，WaitInstance 的 error 仍表示「觀察失敗」，不能把 exit code 非零當成重試觀察的 error。

controller 在拿到容器 ID 後，先將 ID 與 GitHub runner ID 附加到 provisioning 記錄，再啟動受追蹤的 readiness worker。等待就緒不占住 reconcile worker，讓其他已完成 runner 能清理。背景 worker 數受已保留的 runner/capacity 數限制，不另建無界 queue。

就緒完成、工作事件、容器退出、縮容與 drain 可能同時發生，須由 instanceState 的原子轉移決定唯一結果。成功只能記一次；晚到的 readiness 結果不能將 busy/removing 改回 idle。drain 取消 readiness workers 並等待它們退出，然後接管清理。

就緒失敗且資源仍存在：先標為「未確認就緒而移除」，使用與 idle removal 相同的 GitHub deregistration 保護。JobStillRunning 必須還原 busy；GitHub 回覆不明或失敗就保留資源與 lease，排程清理重試。不能沿用「StartInstance 回傳錯誤就直接釋放 lease」處理已建立的容器。

已明確退出的容器保留既有 demandStale barrier，但改為 tracked removing 與既有 cleanup retry 收尾，成功移除後才釋放 lease。這是對現有 watcher「先釋放 lease，再做一次 best-effort remove」的刻意調整，讓 cleanup 失敗不會失去追蹤；不要繼續呼叫會立即 detach/release 的舊 markDead 流程。正常完成與退出觀察競態只允許一個清理 owner。

如果 JIT 回覆沒有 runner ID，不能對仍存活的容器執行未受保護的 readiness-timeout 刪除；標為診斷失敗並保留，交給後續工作事件或明確 shutdown deadline 收尾。立即停止及 drain deadline 的既有強制清理行為不變。

### 1.3 診斷資料

新增 optional `InstanceDiagnostics.CollectDiagnostics(ctx, instanceID)`，Docker 讀取 container inspect 的 ExitCode、OOMKilled、FinishedAt；欄位未知使用 optional 值，不能用 false/0 偽裝已確認。診斷取得失敗不得阻止必要清理。

在以下情況、移除前擷取一次：就緒失敗、尚未收到 JobCompleted 就退出。不為每個成功 job 保存完整 logs。

- 自動保存：scale set、provider、runner/instance ID、時間、固定 failure reason、已知 exit/OOM 資訊。
- `[diagnostics] capture-logs = false` 預設不保存任意 stdout/stderr。就緒解析可在記憶體內進行。
- 明確啟用時只保留最多 100 行且最多 64 KiB；遮蔽已知 token/JIT/Authorization/PEM。文件說明不能保證遮蔽 workflow 自行輸出的未知機密。
- `[diagnostics] enabled = true`，允許關閉保存；不影響記憶體 failure counters。
- 存放在 runner 實際有效 log directory 的 `diagnostics/`；log-file 停用時使用該 service scope 的預設 log directory。必須與 systemd 的有效 log-path fallback 及可寫目錄設定採同一個解析結果：既有 root unit 可能用 LogsDirectory，新非 root system unit 使用 manifest 準備目錄及 ReadWritePaths。不能寫入未開放的另一個目錄，也不能為診斷啟用遞迴 ownership 變更。目錄 0700、檔案 0600，拒絕 symlink，原子寫入。
- 最多 20 份、總量 2 MiB、最多 7 天，先以固定上限實作；只清理這個目錄內符合自有命名的檔案。不可啟動外部清理工具。
- HTTP 僅提供固定分類與可選診斷 ID，絕不提供原始日誌或憑證。

## 2. 映像檢查與更新

### 2.1 指令

建議新增：

```text
runner image inspect [--scaleset NAME] [--json]
runner image inspect --probe --scaleset NAME [--json]
runner image refresh --scaleset NAME
```

- `inspect` 預設列出所有設定映像，查本地 metadata，不 pull、不註冊 runner、不啟動容器/VM；設定與 provider location 沿用目標服務（若已採用 installation-target 提案）。
- 結果區分 configured reference、observed local identity、baked runner version、version source、checked_at。無法可靠取得就回報 `unknown`；不能從一般 OCI version label 推定 Actions runner 版本。
- 顯示既有 pull behavior：Docker 是 manager startup pull，Tart 是 if-not-present；這版不新增 pull-policy 設定，也不把兩種行為誤稱一致。
- Docker 可讀 image ID/RepoDigests；Tart 先使用官方 `list --format json` 驗證存在與狀態。沒有穩定、已驗證的 public CLI/API 能取得 digest 時就留空，不讀取私有 cache layout 或猜測 tag 的 digest。
- `--probe` 明確代表會執行映像：建立無 GitHub token/JIT、無 host Docker socket、無 shared/cache volume 的臨時環境，只執行 Actions runner binary 的 `--version`。Tart 使用 clone，絕不啟動或修改 base VM。
- probe 與 refresh 必須取得相同 machine lock，管理程序執行時拒絕，以免爭用兩個 macOS VM 槽位或更新 warm-pool 的 base。這是第一版刻意限制；不承諾線上 image rollout。
- Docker probe timeout 30 秒、Tart probe 5 分鐘；refresh timeout 30 分鐘。取消後以獨立 30 秒清理期限回收只屬於本次 probe 的資源；失敗時印出精確資源識別，不刪除既有 VM/volume。
- Docker probe 固定 argv 執行 `/home/runner/bin/Runner.Listener --version`；Tart 使用設定 runner-dir 下的 `bin/Runner.Listener --version`。自訂映像不符合時回報 unsupported，不下載 runner 補救。
- `refresh` 在多 scale set 時必須指定名稱；單一 scale set 可省略。僅對明確的 remote image ref pull，local Tart VM 名稱回覆「請自行重建」，不改 config、不刪 base image、不重啟服務。
- Docker 使用設定 socket/platform；Tart 使用對應 TART_HOME。相同 ref 在不同 socket/home 必須視為不同目標。

### 2.2 版本判斷

先提供精確的 baked-version 證據。可另查 `actions/runner` 官方 releases，顯示是否落後最新公開版；外網失敗只讓 freshness 為 unknown，不影響本地檢查。GitHub Enterprise 的 runner compatibility 不能以 github.com 的最新版本作結論。

不得以「映像建立超過 30 天」推算 GitHub 必然拒收；公開最新版本不等於 GitHub/GHES 的實際最低可用版本。指令顯示來源與限制，不產生虛構的退休倒數。這版不加入自動定時 pull、不自動 rebuild 自訂映像，也不修改既有 startup pull policy。

`checked_at` 是本次觀察時間，不是假裝知道歷史 pull time。refresh 成功時輸出本次 refreshed_at 與新的可觀察 identity；跨程序的持久 refresh history 留待確有需求再加入。

## 3. 啟動重試退避

新增 per-scaleset `[retry]`，可由 top-level defaults 繼承：

```toml
[retry]
initial = "5s"
max = "1m"
cooldown-after = 5
cooldown = "5m"
```

duration 必須大於 0、max >= initial、cooldown >= max、cooldown-after >= 1。每次 delay 取 `[base/2, base]` 的隨機值，base 為 initial * 2^(failures-1)，上限 max，避免整批失敗同時重試。第五次連續失敗後改成每次固定 cooldown 的低頻嘗試；成功就 reset，不設需要人手解除的永久熔斷。

重要：不是只將 `AfterFunc(5s)` 換成較長 timer。controller 必須維護 next_retry_at，所有 fresh desired-count callback 都不能提早繞過。維持最多一個可取消 timer；重設、目標供給已滿、drain 及 shutdown 都不留下 timer/goroutine。

「沒有剩餘需求」是目前可供工作的 instance 數已達 `min(max-runners, min-runners + desiredRunners)`，不是只看 job queue 為零。`min-runners = 2` 且 queue 為零時，仍須按退避補足最低兩台；只有 target 已滿（含 target=0）時才取消 active retry deadline。demandStale/pendingCompletions 的 barrier 仍優先，不能憑舊需求新建。

- 失敗包括 JIT 失敗、provider start 失敗、已確認的 readiness 失敗、未接任何工作就意外退出。
- capacity 等待、監看 API 暫時失敗、正常完成、shutdown cancellation 不計入。
- 對非同步 readiness，同一 scale set 同時發生多個結果時採 failure generation：成功只能清除在其啟動前已存在的失敗，不能蓋掉更晚的失敗。已排程的啟動可以完成，但新 scale-up 必須尊重最新 deadline。
- 第一次成功標準是 verified readiness（或 work event）；`ContainerStart` 成功不能 reset。
- 已接工作後中途退出不算 startup failure，另以 unexpected-exit counter 呈現。
- 冷卻不阻塞已完成 runner 的清理、JobStarted/Completed、drain 或其他 scale set。
- health/status 新增 retry_state、next_retry_at；只有失敗且供給未滿需求時 degraded。沒有剩餘需求時清空 active retry deadline，歷史失敗計數保留。
- Tart pool boot 失敗套用相同 delay calculator，但每個 refill worker 保有自己的連續失敗狀態；pool 的失敗計數與 runner startup 分開，不重複計一次失敗。listener reconnect 與 cleanup backoff 維持原本政策。

## 4. 指標與時間量測

### 4.1 HTTP 與資料來源

在既有 health address/port 提供 `GET /metrics`。`health-port = 0` 同時停用三個 endpoint。沿用 localhost 預設，不新增第二個 server、認證系統或對外 listener。採獨立 `prometheus.Registry` 與 `promhttp.HandlerFor`，不使用全域 default registry 或 init-time 註冊；不預設啟用 Go/process collectors。

controller/provider 在事件發生時更新記憶體數據，scrape 僅序列化 snapshot。counter 自程序啟動累計；server 中斷重連不得重設 scale set counter。避免在 request 中做額外 provider I/O。

### 4.2 第一版 metric schema

| Metric | 型別 | Labels / 定義 |
| --- | --- | --- |
| `runner_instances` | gauge | scaleset, provider, phase；四種既有 phase。 |
| `runner_desired_instances` | gauge | scaleset；既有 bounded desired count。 |
| `runner_host_capacity_limit` / `runner_host_capacity_in_use` | gauge | 無；使用既有 broker。 |
| `runner_listener_connected` / `runner_ready` | gauge | scaleset；連線與供給 readiness 分開。 |
| `runner_start_attempts_total` | counter | scaleset, provider。 |
| `runner_start_failures_total` | counter | scaleset, provider, reason；固定分類。 |
| `runner_unexpected_exits_total` | counter | scaleset, provider；與 job result 分開。 |
| `runner_jobs_started_total` / `runner_jobs_completed_total` | counter | scaleset；先沿用既有事件計數語意，不改寫成 exactly-once 承諾。 |
| `runner_capacity_wait_seconds` | histogram | scaleset；一次 broker acquire 的完整等待，只有取得 lease 時記錄。 |
| `runner_startup_duration_seconds` | histogram | scaleset, provider；取得 lease後至 verified-ready/work event；含 pre-start reclaim/JIT/provider。只記成功，失敗另由 counter 呈現。 |
| `runner_cleanup_duration_seconds` | histogram | scaleset, provider；從標記 removing 到實際清理成功，含重試等待；每個 instance 一筆。 |
| `runner_tart_pool_acquisitions_total` | counter | scaleset, outcome；ready/waited/cold，分開立即取得預熱 VM、等候補池及停用池的冷啟動。 |
| `runner_tart_pool_boot_failures_total` | counter | scaleset；與 runner startup failures 分開。 |
| `runner_retry_next_attempt_timestamp_seconds` | gauge | scaleset；有 active deadline 才輸出。 |
| `runner_listener_statistics_timestamp_seconds` | gauge | scaleset；真正 RecordStatistics 的最後時間，不能用混有 job event 的 UpdatedAt 代替。 |

histogram buckets 固定為 `0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800` 秒，加 `+Inf`、sum、count。輸出 HELP/TYPE，秒與 bytes 使用 base units。第一版不手算或保存 p95/p99，由 Prometheus 查詢 histogram。

不把 job ID、runner UUID、repo、workflow、image tag/digest、錯誤原文放入 labels。scale set/provider/phase/reason/outcome 都必須有界。配置值的引號、反斜線、換行要正確 escape。

### 4.3 排隊時間的命名限制

SDK 沒有 execution start timestamp。可在後續增補 `ScaleSetAssignTime - QueueTime` 與 `RunnerAssignTime - ScaleSetAssignTime`，但不能把「本地收到 JobStarted 的時間減 QueueTime」標為精準 execution queue latency。第一版先提供可確定的 host capacity/startup/cleanup 三段時間；上游 timestamp 的語意以官方實例核對後才加入，缺值/負值跳過。

## 交付順序與驗收

建議先完成就緒、診斷與重試，再完成映像指令，最後把穩定的 lifecycle observations 接成 metrics。每階段可獨立 review，不以全部完成才驗收。

| 階段 | 可交付結果 | 必要驗收 |
| --- | --- | --- |
| A | Docker 真正 ready 才計成功；失敗有 metadata | 早退、OOM、stdout 不可讀、跨 chunk、晚到 job event、deregistration busy、drain 競態、診斷失敗仍能清理。 |
| B | 單一 scale set 退避與冷卻 | fresh demand 不繞過；無需求不重試；其他 scale set 不受影響；cleanup 不延遲；pool 與 runner 不重複計數。 |
| C | image inspect/probe/refresh | inspect 零資源建立；probe 無憑證/共享掛載；live manager 時拒絕維護；remote/local 與不同 provider location 不混淆。 |
| D | `/metrics` 與等待時間 | 正確 wire format、bucket 累計及 +Inf；scrape 零外部 I/O；series 有界；現有 health contract 不變。 |

每階段先跑對應單元測試，再跑 `go test -race ./...`、`go vet ./...` 及現有 lint/CI。測試使用可控時間與 channel，不以長時間 sleep 驗證退避。無功能實作前不新增測試或執行會啟動服務的指令。

實機驗收獨立列為 gate：Docker 真實 job 完成與清理；刻意使用無法連線的 runner；Tart 一次冷啟動及預熱池 job；SIGTERM drain 期間不得中斷 job；兩個連續 metrics scrape 的增量。mock/CI 通過不代表實機 gate 通過。實機維護、重新啟動與映像 refresh 要另依使用者授權執行。

## 可檢閱的選擇

以下是本草案建議，實作前一起確認：

1. Docker readiness 預設開啟，提供顯式相容出口。
2. 任意 runner 日誌預設不保存，metadata 預設保存。
3. 映像 probe/refresh 第一版要求 manager 停止，不做背景自動更新。
4. 五次失敗後進入五分鐘冷卻，持續自動探測恢復。
5. 提案新增官方 Prometheus Go client 作唯一新直接相依套件，使用固定有限 schema；不自製 exporter protocol。

## 核對來源

- [GitHub self-hosted runners reference](https://docs.github.com/en/actions/reference/runners/self-hosted-runners)：ephemeral log preservation、版本更新要求。
- [Tart list implementation](https://github.com/openai/tart/blob/main/Sources/tart/Commands/List.swift) 與 [quick start](https://github.com/openai/tart/blob/main/docs/quick-start.md)：public list/pull/clone 行為，透過 Context7 核對。
- [Prometheus naming](https://prometheus.io/docs/practices/naming/) 與 [exposition format](https://prometheus.io/docs/instrumenting/exposition_formats/)：base units、有限 labels 及 text format。
- 本地固定版本 `github.com/moby/moby/client@v0.6.1` 的 container_logs.go/container_inspect.go/image_inspect.go；`github.com/actions/scaleset@v0.4.0/types.go`。
