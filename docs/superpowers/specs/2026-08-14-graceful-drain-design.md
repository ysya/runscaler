# 設計:優雅 drain 關閉與啟動時孤兒對帳

日期:2026-08-14
狀態:設計已確認,待實作

## 背景與動機

`Scaler.Shutdown`(`internal/scaler/scaler.go`)在收到訊號時對 idle 與 busy 的 runner **一視同仁地強制移除**。busy 代表正在跑 job,所以任何重啟——升級、設定變更、機器維護——都會殺掉進行中的 build,而使用者看到的是自己的 workflow 失敗,不是「runner 正在升級」。

這使升級變成一件要挑時間做的事。實際後果已經發生過:leg host 的 systemd unit 在改名後指向舊檔名而 crash-loop 了近 30 天,期間使用者改用 tmux 手動跑;真正的原因之一是「重啟有代價」,於是重啟這件事被拖延。

同時,程序若被硬殺(SIGKILL、OOM、斷電、panic),它建立的 runner 容器/VM 會留在原地。目前**沒有任何自動機制**收拾它們——只有操作者主動執行 `runner doctor --fix` 時才會清理。leg host 上曾有一個存活四個月、`docker inspect` 已讀不到、最後必須停掉 daemon 手術移除的 Dead container,即屬此類。

### 業界前例

**GitLab Runner** 把兩種語意分給兩個訊號:`SIGTERM` 立即關閉、**`SIGQUIT` 優雅關閉**(不再接新 job,等現有 build 跑完才退出)。升級流程被文件化為「先送 SIGQUIT 讓它 drain,等它自行退出,再安裝新版」,並提供 systemd drop-in 範本 `KillSignal=SIGQUIT` + `TimeoutStopSec=7200`。

**GitHub 官方的 actions/runner** 走另一條路:runner 在 job 邊界之間自我更新,由伺服器協調時機。這正是本專案 `disable-update` 關閉的機制,但它需要伺服器端配合,不適用於 supervisor 型的程序。

### 為何不能照抄 GitLab 的訊號分法

**launchd 無法指定停止訊號。** macOS 的 `launchd.plist(5)` 明載:launchd 先送 `SIGTERM`,逾時後送 `SIGKILL`,間隔由 `ExitTimeOut` 控制;plist 沒有等同 systemd `KillSignal` 的設定,且 man page 明確要求 daemon「處理 SIGTERM 並收拾善後」。

三台主機中有一台是 Mac(Tart backend,跑 iOS build,單一 job 常達數十分鐘)。若把 drain 綁在 SIGQUIT 上,那台永遠拿不到 drain 能力。因此本設計把 drain 綁在 **SIGTERM**,並接受 SIGQUIT 作為同義詞。

## 設計原則

**分層處理「程序結束」的兩種路徑,不要用一種機制兼顧兩者。**

| 路徑 | 機制 | 涵蓋範圍 |
| --- | --- | --- |
| 優雅結束 | drain | 收到訊號、有機會收尾 |
| 非優雅結束 | 啟動時對帳 | SIGKILL、OOM、斷電、panic——任何沒機會收尾的情況 |

先前的設計草案曾包含「啟動時偵測服務管理器的 stop timeout 並據此夾住 drain 上限」,目的是避免 systemd 在 drain 完成前 SIGKILL 而留下孤兒容器。加入啟動時對帳後這層不再需要:即使 drain 被截斷,下次啟動也會自動收拾。改以單純的啟動警告取代,少一層對服務管理器的耦合。

**啟動時對帳的安全前提已經成立。** 依標籤移除「未被追蹤的 runner 容器」只有在確定沒有第二個 runner 程序共用同一個 daemon 時才安全,否則會刪掉別人正在用的容器。v0.4 加入的單一實例鎖(`internal/lock`,`ErrAlreadyRunning`)提供了這個保證:同一台機器只能有一個 runner 執行 `run`。因此啟動時看到的 `managed-by=runner` 容器必然是前次留下的。**這個依賴必須寫進程式碼註解**——若日後放寬單一實例限制,對帳邏輯必須同時重新檢視。

## 目標

1. 收到停止訊號時不再中斷進行中的 job
2. 不論上次如何結束,啟動時自動清除孤兒 runner 容器與 VM
3. 服務管理器的預設重啟行為變成安全操作
4. 讓「更新了但忘記重啟」變成看得見的狀態

## 非目標

- **`runner update` 自動重啟**:有了安全的 drain 與清楚的版本提示後,自動化一個會中斷服務的動作,價值不足以抵過風險。維持由操作者決定時機
- **伺服器協調的 job 邊界更新**(actions/runner 的模型):需要伺服器端支援
- **修改 `doctor --fix` 的行為**:它對 shared volume 的孤兒判定已失效(見 `2026-08-14-cache-architecture-followups.md`),屬獨立問題

## 詳細設計

### A. Drain 狀態(`internal/scaler`)

`Scaler` 新增 draining 狀態。進入 drain 後:

- `HandleDesiredRunnerCount` 不再擴充,直接回傳目前數量。新 job 不會有 runner 承接,留待下一個程序處理
- 立即移除所有 idle runner(它們沒有工作在身)
- busy runner 不動,等其自然完成。`HandleJobCompleted` 照常移除完成的 runner
- busy 歸零即結束 drain;或達到上限後強制移除剩餘者

```go
// Drain stops taking new work and waits for in-flight jobs to finish.
// Returns when no busy runners remain or the deadline passes, whichever
// comes first. Idle runners are removed immediately — they hold no work.
func (s *Scaler) Drain(ctx context.Context) error
```

`Shutdown` 維持現有語意(強制移除全部),drain 逾時後由呼叫端接著呼叫它。

**drain 期間 listener 必須繼續運作。** runner 是透過 listener 收到的訊息才知道 job 已完成(`HandleJobCompleted`),因此 drain 不能先關掉 listener 再等 busy 歸零——那會等到天荒地老。正確順序是:進入 drain → listener 照常運行、只是不再擴充 → busy 歸零或逾時 → 結束 listen 迴圈 → 既有的 defer 刪除 scale set。這是實作上最容易踩錯的一步。

### B. 訊號處理(`cmd/runner/main.go`)

現行 `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` 使兩者都立即取消 context。改為:

| 事件 | 行為 |
| --- | --- |
| 第一次 SIGTERM / SIGQUIT | 進入 drain |
| 第二次任一訊號 | 立即關閉(現行行為) |
| 第三次 | 強制退出(現行已有) |
| **SIGINT(Ctrl-C)** | **立即關閉,不 drain** |

SIGQUIT 視為 SIGTERM 的同義詞,讓熟悉 GitLab Runner 的操作者的肌肉記憶可用。

**SIGINT 刻意不 drain。** Ctrl-C 的慣例語意是「現在就停」,而互動式執行(本專案的 Mac Studio 目前就在 tmux 裡前景執行)按下 Ctrl-C 後卻要等最長兩小時,是不可接受的意外。GitLab Runner 同樣把 SIGINT 歸為中斷而非優雅關閉。這也表示互動式使用的行為與現況完全相同,沒有回歸。要在互動式情境下 drain,送 `kill -TERM <pid>`——這點須寫進文件。

drain 期間定期記錄剩餘 busy 數量與已等待時間,讓操作者知道它在等什麼、還要多久。

### C. 啟動時孤兒對帳(`cmd/runner/main.go`,在 scale set 啟動之前)

在取得單一實例鎖之後、啟動任何 scale set 之前執行:

- **Docker**:列出所有容器(含已停止),移除 `Labels["managed-by"] == "runner"` 者。`cmd/runner/cmd_doctor.go` 已有相同的查詢邏輯(該處另有以名稱樣式比對的第二條件),抽成共用函式供兩處使用,避免第三份實作
- **Tart**:`tart list` 中名稱符合 `runner-*` 或 `pool-*` 者(對應 `internal/scaler` 的 `runner-<8hex>` 與 `internal/backend/tart.go` 的 `pool-<slot>-<ms>`),先 `tart stop` 再 `tart delete`

對帳結果一律以 Info 記錄(移除了幾個、什麼名字);沒有孤兒時記 Debug。移除失敗只警告不阻擋啟動——啟動失敗比留下一個孤兒更糟。

**不對帳 volume。** shared volume 與 cache volume 依設計本來就長期存在,依「存在即孤兒」判定會刪掉進行中 workflow 的交接資料。

### D. 服務範本(`cmd/runner/cmd_service.go`)

- systemd:加入 `TimeoutStopSec`,值為 drain 上限加一分鐘餘裕。`KillSignal` 維持預設(SIGTERM),不需覆寫
- launchd:加入 `ExitTimeOut`,同上

**既有安裝不會自動更新 unit 檔。** 因此 runner 啟動時若偵測到自己在服務管理器底下執行(systemd 的 `INVOCATION_ID` 或 launchd 的環境特徵)且 unit 缺少對應的 timeout 設定,發出一則警告說明重啟時 drain 可能被截斷,並指出重跑 `runner service install` 可修正。**警告而已,不讀取也不修改服務管理器的狀態**——這是刻意的取捨,見設計原則。

即使警告被忽略,最壞情況也只是 drain 被 SIGKILL 截斷,而 C 節的對帳會在下次啟動時收拾。

### E. 設定

```toml
drain-timeout = "2h"   # 0 停用 drain,回到立即關閉
```

頂層全域設定(與 `health-port` 同層),因為它是程序層級行為而非 per scale set。預設 2 小時,與 GitLab Runner 的建議值一致——涵蓋實際 job 長度(realmkit 的 `timeout-minutes: 50`、Mac Studio 的 iOS build 數十分鐘)且留有餘裕。

### F. `runner update` 的版本提示

更新完成後查詢本機 health endpoint,若執行中版本與剛安裝的 binary 版本不同,明確說明:服務仍在跑舊版、要套用需執行 `runner service restart`。現行訊息只有一句籠統的 "Restart runner to use the new version",不會告訴操作者服務是否在跑、跑的是哪一版。

這正是 leg host 那次「更新了、忘了重啟、binary 顯示新版但實際跑舊版」的無聲失敗。有了 drain,建議操作者立即重啟才是負責任的建議。

## 錯誤處理

| 情境 | 行為 |
| --- | --- |
| drain 期間 backend 移除 idle runner 失敗 | 警告,繼續 drain |
| drain 達到上限仍有 busy | 記錄剩餘數量與名稱,強制移除並退出 |
| `drain-timeout` 為 0 或負值 | 停用 drain,維持現行立即關閉;`runner validate` 不視為錯誤 |
| 啟動對帳時 Docker 不可用 | 警告並跳過,不阻擋啟動 |
| 啟動對帳移除個別容器失敗 | 警告該容器,繼續處理其餘 |
| `tart list` 失敗 | 警告並跳過 Tart 對帳 |
| 服務 unit 缺少 timeout 設定 | 啟動時警告一次,不阻擋 |

## 測試

- **drain 狀態機**:idle 立即移除、busy 保留;drain 期間 `HandleDesiredRunnerCount` 不擴充;busy 歸零即返回;逾時後強制移除剩餘者。以假 backend 驅動,不依賴計時器精度
- **訊號序列**:第一次進入 drain、第二次立即關閉。以可注入的訊號來源測試,避免對程序真正送訊號
- **啟動對帳**:含 `managed-by=runner` 標籤的容器被移除;不含標籤者不被碰;移除失敗不阻擋啟動;**volume 在任何情況下都不被移除**(此為防資料遺失的關鍵不變量,須有專屬測試)
- **共用的孤兒查詢函式**:`doctor` 與啟動對帳走同一條路徑,兩處行為一致
- **服務範本**:systemd 產生的 unit 含 `TimeoutStopSec` 且大於 drain 上限;launchd plist 含 `ExitTimeOut`
- **既有設定相容**:未設 `drain-timeout` 的設定載入後取得預設值且無警告

## 受影響檔案

| 檔案 | 變更 |
| --- | --- |
| `internal/scaler/scaler.go` | 新增 `Drain`、draining 狀態、`HandleDesiredRunnerCount` 的 drain 分支 |
| `internal/scaler/scaler_test.go` | drain 狀態機測試 |
| `cmd/runner/main.go` | 訊號序列;啟動時對帳;drain 後才 `Shutdown` |
| `cmd/runner/orphans.go` | 新增:共用的孤兒查詢與移除(Docker 與 Tart) |
| `cmd/runner/cmd_doctor.go` | 改用共用函式 |
| `cmd/runner/cmd_service.go` | 範本加入 `TimeoutStopSec` / `ExitTimeOut` |
| `cmd/runner/cmd_update.go` | 版本不一致提示 |
| `internal/config/config.go`、`defaults.go` | `DrainTimeout` 與預設值 |
| `config.example.toml`、`README.md` | 記錄 drain 行為、訊號語意與升級流程 |

## 推出順序建議

1. 啟動時孤兒對帳(獨立有價值,且是後續步驟的安全網)
2. drain 狀態機與訊號處理
3. 服務範本與啟動警告
4. `runner update` 版本提示

第 1 步先行的理由:它讓第 2、3 步即使有缺陷也不會留下孤兒容器。
