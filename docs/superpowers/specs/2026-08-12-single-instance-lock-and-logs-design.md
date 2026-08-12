# 設計:單一實例鎖 + 自有 log 檔 + `runner logs`

日期:2026-08-12
狀態:設計已確認,待實作

## 背景與動機

runner 目前沒有任何機制阻止同一台機器上跑起第二個實例。唯一的隱含防護是 health server 綁 port(`cmd/runner/main.go` 的 `net.Listen`):第二個實例會因為 port 被佔用而啟動失敗,但這層防護有兩個問題——錯誤訊息是「health server 起不來」,完全看不出真正原因是「已經有一個在跑了」;而 `health-port = 0` 時連這層都沒有。

這不是理論問題。leg host(192.168.66.168)曾出現 systemd unit 因改名而 crash-loop 近 30 天,同時使用者在 tmux 手動跑了另一個實例;兩者並存期間沒有任何警告。並存的實際危害是定期清理的 sweeper 會互相 race——`CleanupSharedDocker` 的註解已明確指出並行呼叫會與容器移除和其他 prune 操作衝突,`startBuildxCleanup` / `startDockerPrune` / `startSharedVolumeCleanup` 也都以「整個程序只有一個 sweeper」為前提設計。

另一個問題是 log 的可及性。`runner service logs` 已存在且在服務模式下正常運作(讀 journald / launchd),但在手動啟動(tmux、前景)時無用——那些情境下 log 只存在於終端機的 scrollback,沒有檔案可讀。Mac Studio 正是這種用法。

## 目標

1. 同一台機器同時只能有一個 runner 實例在 `run`,第二個必須以清楚的錯誤訊息拒絕啟動
2. 不論以何種方式啟動(服務、tmux、前景),runner 都寫一份自己的 log 檔
3. 提供 `runner logs` 讀取該檔,使手動啟動的實例也能查看 log

## 非目標

- **`--detach` / 自我背景化**:與使用者的 tmux 工作流衝突,不做
- **`runner restart` 改動**:現有 `runner service restart` 維持只管服務;tmux 模式下由使用者自行 Ctrl-C 重跑。若 restart 改成殺掉舊程序再另起背景程序,會讓程序與 tmux pane 脫鉤,比現況更難理解
- **自動重啟 / crash 偵測**:systemd 與 launchd 做得更好且更可靠,不在 runner 內重實作
- **跨機器協調**:鎖只保證單機互斥

## 詳細設計

### A. 單一實例鎖(`internal/lock`,新套件)

採用 **`flock(2)`** 而非單純的 PID 檔。關鍵理由:flock 由 kernel 持有,程序以任何方式結束(正常退出、SIGKILL、當機、斷電)時**自動釋放**。PID 檔則必須自行判斷「這個 PID 還活著嗎、是不是已被作業系統重用」,那是一整類容易出錯的邏輯,且無法安全處理斷電後殘留的鎖檔。

介面:

```go
// Acquire 取得單機互斥鎖。成功時回傳的 release 會釋放鎖並清理鎖檔。
// 已被佔用時回傳 ErrAlreadyRunning,並帶有持有者的診斷資訊。
func Acquire(path string, info Info) (release func(), err error)

// Info 是寫入鎖檔供人閱讀的診斷資訊,不作為判斷依據。
type Info struct {
    PID        int
    StartedAt  time.Time
    ConfigPath string
}
```

要點:

- 鎖檔路徑固定為 `/tmp/runner.lock`,**不放 `$HOME`**。理由:root 執行的 system service 與使用者手動執行的程序必須互斥,兩者 `$HOME` 不同。`/tmp` 在支援的平台(darwin / linux)上皆為 world-writable 且開機清空,適合作為機器層級鎖
- 判斷依據**只有 flock 本身**。檔案內容(PID、啟動時間、config 路徑)僅供產生錯誤訊息使用,即使內容過期或損毀也不影響正確性
- 使用 `LOCK_EX | LOCK_NB`(非阻塞),取不到立即失敗,不等待
- 只有 `runner run` 取鎖。`doctor` / `validate` / `status` / `migrate` / `service *` 等不取鎖,不受影響

錯誤訊息需直接說明衝突對象,取代現行難以理解的 health server 錯誤:

```
another runner is already running on this host (PID 4821, started 2026-08-12 14:03:11, config /home/test/runner/config.toml)

  Only one runner may run per machine — the periodic cleanup sweepers
  (docker prune, buildx, shared volume) assume a single process and will
  race each other otherwise.

  To run multiple orgs, use multiple [[scaleset]] entries in one config.
```

當鎖檔無法讀取或內容損毀時,退化為不含詳情的訊息,仍然拒絕啟動。

### B. 自有 log 檔(`internal/config`,調整 `NewLogger` / `NewScaleSetLogger`)

runner 在寫 stdout 的同時,將相同輸出寫入一份 log 檔(等同 tee)。這使得 `runner logs` 在任何啟動方式下都有資料可讀,而服務模式下 journald / launchd 的既有行為不受影響(會有重複紀錄,可接受)。

路徑決定順序:

1. config 的 `log-file` 明確指定 → 使用該路徑
2. `log-file = ""`(明確設為空字串) → 停用檔案輸出
3. 未設定且有使用 config 檔 → `<config 檔所在目錄>/runner.log`
4. 未設定且無 config 檔(純 CLI flag) → 當前工作目錄的 `runner.log`

選擇「config 檔旁邊」而非固定的 XDG / `~/Library/Logs` 路徑,是因為它在服務模式下同樣明確——systemd / launchd 未設 `WorkingDirectory` 時 cwd 為 `/`,以「執行目錄」為準會讓 log 落在意料之外的位置。在現有的兩台主機上,config 皆位於 `~/runner/`,因此 log 會落在 `~/runner/runner.log`,符合既有習慣。

輪替:單一檔案上限 **10 MB**,超過時輪替為 `runner.log.1`(僅保留一份備份,舊的直接覆蓋)。長跑的 runner 不能讓 log 無限成長——這兩台主機已因磁碟問題處理過兩次。輪替以自行實作的 size-check-on-write 完成,不引入外部相依。

寫檔失敗(權限不足、磁碟滿)**不可導致 runner 無法啟動**:降級為僅輸出 stdout,並在 stdout 印一則警告。

檔案 writer 必須是**併發安全**的。每個 scale set 各自跑在一個 goroutine 並持有自己的 logger(`NewScaleSetLogger`),它們會同時寫入同一個檔案;寫入與大小檢查、輪替之間若無互斥,會造成紀錄交錯或在輪替瞬間遺失寫入。以單一 mutex 保護 write + rotate 即可。

### C. `runner logs`(新的頂層指令,`cmd/runner/cmd_logs.go`)

```
runner logs [-f] [-n N] [--config PATH]
```

讀取依上述規則解析出的 log 檔路徑。`-n` 預設 100 行,`-f` 持續跟隨(輪替發生時重新開檔)。檔案不存在時給出明確訊息,說明可能是尚未啟動過、或 `log-file` 被停用。

與既有 `runner service logs` 的分工需在 help 文字中寫明:

- `runner logs` — runner 自己的 log(任何啟動方式皆可用)
- `runner service logs` — 服務管理器的 log(journald / launchd),包含 runner 自己寫不到的事件,例如啟動失敗、crash-loop、被 OOM killer 終止

### 既有程式碼調整

- `cmd/runner/main.go`:`run()` 在載入 config 之後、建立 Docker client 之前取鎖;`defer release()`。health server 綁 port 的檢查保留(縱深防禦)
- `internal/config/config.go`:`Config` 新增 `LogFile string`;`NewLogger` / `NewScaleSetLogger` 接受選用的檔案 writer
- `config.example.toml` / `README.md`:記錄 `log-file` 與 `runner logs`

## 錯誤處理

| 情境 | 行為 |
| --- | --- |
| 鎖已被佔用 | 拒絕啟動,錯誤訊息含持有者 PID / 啟動時間 / config 路徑 |
| 鎖檔內容損毀或讀不到 | 仍拒絕啟動,訊息退化為不含詳情 |
| 鎖檔所在目錄不可寫 | 啟動失敗並明確指出路徑(這代表環境異常,不應靜默忽略) |
| log 檔開啟失敗 | 降級為僅 stdout + 警告,**不影響啟動** |
| log 輪替失敗 | 記錄警告,繼續寫入原檔 |
| `runner logs` 找不到檔案 | 說明可能原因(未啟動過 / 已停用 / config 路徑不同) |

## 測試

- `internal/lock`:第二次 `Acquire` 回傳 `ErrAlreadyRunning`;`release` 之後可再次取得;**跨程序**測試(啟動子程序持鎖,驗證父程序取不到)以確認 flock 語義而非僅測到同程序行為;鎖檔內容損毀時仍正確拒絕
- log 檔:tee 行為(stdout 與檔案內容一致);達上限時正確輪替且不遺失新寫入;開檔失敗時 runner 仍能啟動
- 路徑解析:上述四種情況各一,含 `log-file = ""` 停用
- `runner logs`:`-n` 行數、檔案不存在的訊息
- `cmd/runner`:`run` 取鎖而其他子指令不取鎖

## 受影響檔案

| 檔案 | 變更 |
| --- | --- |
| `internal/lock/lock.go` | 新增 |
| `internal/lock/lock_test.go` | 新增 |
| `cmd/runner/cmd_logs.go` | 新增 |
| `cmd/runner/main.go` | `run()` 取鎖;註冊 `logs` 子指令 |
| `internal/config/config.go` | `LogFile` 欄位;logger 支援檔案 writer |
| `internal/config/defaults.go` | log 檔名與大小上限常數 |
| `config.example.toml` | 記錄 `log-file` |
| `README.md` | 記錄 `runner logs` 與單一實例限制 |
