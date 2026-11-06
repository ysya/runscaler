# 設計：目錄配置對齊業界慣例（FHS / XDG / Apple）

日期：2026-09-16（2026-09-17 依第二次審查修訂）
狀態：已實作（v0.10.0）
取代：`2026-08-12-single-instance-lock-and-logs-design.md` 的「B. 自有 log 檔」中「預設寫在 config 檔旁」的決定（log 檔本身、輪替、`runner logs` 維持）

## 背景與動機

檢視 service 安裝流程時發現一串互相牽連的問題，其中兩個會讓 service 起不來，一個會讓 runner 直接 panic：

1. **Linux system service 拿不到 lock**：`runner service install --user=false` 產生的 unit 有 `ProtectSystem=strict`，但 `ReadWritePaths` 只有 config 目錄（與 docker socket）。`/tmp` 因此唯讀，`runner run` 開 `/tmp/runner.lock` 失敗並被 systemd 無限重啟。已在 systemd 252 容器以 v0.9.1 實際重現：`open runner lock /tmp/runner.lock: read-only file system`。
2. **macOS launchd 下找不到 tart**：plist 未設 PATH，launchd 啟動的程序 PATH 為 `/usr/bin:/bin:/usr/sbin:/sbin`（本機 launchd 子程序實測），Homebrew 安裝的 tart 位於 `/opt/homebrew/bin`，`exec.LookPath("tart")` 失敗後 KeepAlive 每 10 秒重啟。
3. **停用 log 檔就 panic**：`runManager` 把型別為 `*config.LogFileWriter` 的 nil 傳入 `io.Writer` 參數，`log-file = ""`（文件記載的停用方式）或開檔失敗時，第一行 log 即 nil pointer panic。v0.9.0 與 v0.9.1 皆已實測重現。
4. **log 寫在 `/etc`**：system 模式下 log 預設為 `/etc/runner/runner.log`，unit 為此把 `/etc/runner` 開成可寫，service 等於能改寫自己的 config（含 token）。
5. **root 寫 log 會跟隨 symlink**：`openLogFile` 以路徑開檔、輪替時以 `O_TRUNC` 重開原路徑。root service 的 log 若位於一般使用者可寫的目錄，可被 symlink 導去 append 或截斷 root 的檔案。
6. **launchd 的 stdout 檔無限成長**：plist 把 stdout/stderr 導到 `~/Library/Logs/runner.log`，launchd 不輪替，runner 也沒處理，且與 runner 自己的 log 檔重複。
7. **root service 執行一般使用者可改寫的 binary**：`install.sh` 預設裝到 `~/.local/bin`，system service 以 root 執行它，一般使用者替換 binary 即可取得 root。
8. **lock 跨身分失效**：`lock.Acquire` 每次開檔都 `Fchmod(0666)`，非擁有者得到 EPERM，root 跑過一次後一般使用者就無法啟動；反方向在 Linux `fs.protected_regular` 啟用時，root 以 `O_CREAT` 開啟使用者建立的 `/tmp/runner.lock` 會被拒絕。
9. **service 偵測不準**：`logServiceDrainTimeoutReminder` 以 `XPC_SERVICE_NAME != ""` 判斷 launchd，互動終端機的 `XPC_SERVICE_NAME=0` 被誤判；且只要在 service 下就每次啟動都提醒重裝，即使 unit 是新裝的。
10. **macOS `runner service status` 忽略 `--user`**：只跑 `launchctl list <label>`，legacy 子指令依是否為 root 選 domain，system daemon 不加 sudo 查不到，也無提示。

### 業界做法（已查證）

- **FHS 3.0**：`/etc`「configuration files … must be static」；`/var/lib`「state information … data that programs modify while they run」；`/var/log`「Most logs must be written to this directory」；`/var/lock`「Lock files should be stored within」；`/srv`「site-specific data which is served by this system」，不是服務自身檔案的位置。
- **systemd.exec**：`ConfigurationDirectory=`、`StateDirectory=`、`CacheDirectory=`、`LogsDirectory=`、`RuntimeDirectory=` 分別對應 `/etc`、`/var/lib`、`/var/cache`、`/var/log`、`/run`，並自動排除於 `ProtectSystem=` 的唯讀範圍。`ProtectSystem=strict` 下 `/tmp` 為唯讀（`namespace.c` 的例外表只有 `/proc`、`/sys`、`/dev`、`/home`、`/run/user`、`/root`）。`ReadWritePaths=` 中不存在且未加 `-` 前綴的路徑會使 unit 啟動失敗。
- **systemd.service**：`ExecStart=` 會展開 `%` 指定符與 `$VARIABLE`，字面 `%` 與 `$` 須寫成 `%%`、`$$`；`Environment=` 只展開指定符。
- **XDG Base Directory**：`$XDG_STATE_HOME`（預設 `~/.local/state`）存放「actions history (logs, history, recently used files, …)」；目錄不存在時應以 0700 建立；相對路徑的變數值應忽略。
- **GitLab Runner**：root 時 config 在 `/etc/gitlab-runner/config.toml`，非 root 在 `~/.gitlab-runner/config.toml`；binary 以 sudo 放到 `/usr/local/bin/gitlab-runner`；macOS 只支援 user-mode LaunchAgent（需使用者 keychain 與 UI session），須從本機 GUI 終端機安裝，建議開自動登入。
- **Orchard（Tart 官方編排）**：worker 若以 root 執行，須指定非特權使用者執行 VM 相關工作。
- **Tart**：`TART_HOME` 預設為「目前使用者」的 `~/.tart`；以 root 執行會使用 `/var/root/.tart`，看不到使用者已拉取的 image。
- **launchd**：LaunchAgent 程序的 `XPC_SERVICE_NAME` 等於 plist 的 Label（本機 6 個第三方 agent 實測）。
- **Linux kernel**：`fs.protected_regular=1` 時「don't allow O_CREAT open on regular files that we don't own in world writable sticky directories, unless they are owned by the owner of the directory」。
- **發行版預設權限**：debian:12 與 ubuntu:24.04 的 `/`、`/usr`、`/usr/local`、`/usr/local/bin`、`/opt`、`/var/log` 皆為 `root:root 755`（容器實測）；安裝 rsyslog 等套件後 `/var/log` 可能變成群組可寫，設計不得依賴它維持 root 專屬。
- **x/sys/unix**：`Openat`、`Mkdirat`、`Renameat`、`Unlinkat`、`Fstat` 在 darwin 與 linux 的 amd64、arm64 皆可編譯（實測）。

## 目標

1. runner 的 config、log、備份、binary 位置依執行身分遵循平台慣例
2. Linux system service 在 `ProtectSystem=strict` 下能正常運作，且 `/etc/runner` 對 service 唯讀
3. macOS 只支援一般使用者執行與 LaunchAgent；launchd 下找得到 tart、不再重複寫入完整 log
4. 以 root 執行的 service 只執行 root 擁有、一般使用者無法改寫的 binary；root 寫 log 時，任何上層目錄被替換都無法導走寫入
5. root 與一般使用者輪流執行時，machine-wide lock 仍能運作
6. 提醒過舊的 service 範本與不足的停止逾時，並提供驗證失敗時不動既有服務的重裝方式
7. 所有會安裝 service 的入口（`service install`、`migrate`）產生相同的定義
8. 既有安裝不被自動搬動，升級後以明確訊息引導

## 非目標

- **自動搬移既有檔案**：舊 log、舊備份一律留在原處
- **建立專用系統帳號**：Linux system service 維持 root 執行（Docker provider 需要的 docker 群組本質上等同 root，隔離效益有限）
- **launchctl 改用 `bootstrap`/`bootout`**：行為差異需另外驗證
- **改變 lock 路徑**：維持 `/tmp/runner.lock`（理由見 A.5）
- **把 lock 當成安全邊界**：見 D.2
- **service 定義的全面稽核**：版本標記只代表範本世代
- **使用 `/srv`**
- **發行套件**（deb/rpm/Homebrew formula）

## 詳細設計

### A. 目錄配置

| | Linux root（system service） | Linux 使用者 | macOS 使用者 |
|---|---|---|---|
| config 預設位置 | `/etc/runner/config.toml` | `~/.config/runner/config.toml` | `~/.config/runner/config.toml` |
| runner log | `/var/log/runner/runner.log` | `~/.local/state/runner/runner.log` | `~/Library/Logs/runner/runner.log` |
| migrate 備份 | `/var/lib/runner/backups/` | `~/.local/state/runner/backups/` | `~/.local/state/runner/backups/` |
| binary 建議位置 | `/usr/local/bin/runner`（必須 root 擁有） | `~/.local/bin/runner` | `~/.local/bin/runner` |
| lock | `/tmp/runner.lock` | `/tmp/runner.lock` | `/tmp/runner.lock` |

規則：

1. **身分**：依 effective UID。0 為 root，其餘為使用者；手動 `sudo runner run` 亦為 root。
2. **明確設定優先**：config 的 `log-file`（root 另受 D.1 限制）、`--config`、`--backup-dir`、絕對路徑的 `XDG_CONFIG_HOME` / `XDG_STATE_HOME`。相對路徑的 XDG 變數依規範忽略。
3. **使用者層級一律 XDG**，唯一例外是 macOS 的 log 放 `~/Library/Logs`（Console.app 可見，亦為 launchd 慣例）。
4. **config 搜尋順序不變**：目前目錄 → 使用者 config 目錄 → `/etc/runner` → 舊版位置。任何位置的 config 仍可用 `--config` 指定（例如 `~/runner/config.toml`）。`XDG_CONFIG_HOME` 為絕對路徑時，解析使用者 config 目錄不需要 `$HOME`（維持現況）。
5. **lock 維持 `/tmp/runner.lock`**：root 的 service 與使用者手動執行必須搶同一把鎖；macOS 沒有 `/run` 與 `/var/lock`（本機實測），`/tmp` 是兩平台所有身分皆可寫之處。跨身分的開檔方式見 D.2。
6. **macOS 的 root（effective UID 0，不論 `--user` 為何）**：
   - 拒絕：`runner run`、`init`、`service install|start|restart`、`migrate`（任何 scope）
   - 允許：`service uninstall|stop|status|logs --user=false`，讓既有 LaunchDaemon 可被清除

### B. `internal/layout`（新套件）

單一來源決定 config、log、備份的預設路徑與 root log 的目錄規則。除 `CurrentIdentity` 外皆為純函式。

```go
const (
	SystemConfigFile = "/etc/runner/config.toml"
	SystemLogRoot    = "/var/log"
	SystemLogFile    = "/var/log/runner/runner.log"
	SystemBackupDir  = "/var/lib/runner/backups"
)

// Identity is everything that decides where runner keeps its files.
type Identity struct {
	GOOS          string // "linux" or "darwin"
	Root          bool   // effective UID is 0
	Home          string // $HOME as seen by this process; "" when unset
	XDGConfigHome string // honored only when absolute
	XDGStateHome  string // honored only when absolute
}

func CurrentIdentity() Identity                 // never fails; Home may be ""
func (id Identity) ConfigHome() (string, error) // error only when Home is needed and unset
func (id Identity) StateHome() (string, error)
func (id Identity) DirPerm() os.FileMode         // 0755 root, 0700 user

type Layout struct {
	ConfigFile string
	LogFile    string
	BackupDir  string
}

func For(id Identity) (Layout, error) // root never fails; users fail only without the bases they need

// SystemLogsDirectory reports the directory of logFile relative to
// /var/log when it is a directory strictly below /var/log whose components
// use only [A-Za-z0-9._-]; both systemd's LogsDirectory= and root's runtime
// log decision use this single rule.
func SystemLogsDirectory(logFile string) (string, bool)
```

lock 路徑不納入 layout，呼叫端直接使用 `lock.DefaultPath`。使用者：`runner run`（log）、`runner logs`、`runner migrate`（備份）、`runner init`（輸出）、`runner service install`（config 預設、log 目錄、plist/unit 環境變數）、`configSearchPaths`。`cmd/runner/config_paths.go` 的 `userConfigPath` 改用 `Identity.ConfigHome()`。

另新增 `internal/pathtrust`：`Problem(uid uint32, mode fs.FileMode) string` 為「只有 root 能改」的唯一判斷（非 symlink、uid 0、group 與 others 皆不可寫），供 C.3 的 binary 檢查與 D.1 的 root log 目錄共用。

### C. service 的產生與安裝

#### C.1 systemd unit（Linux）

system unit：

```ini
[Service]
Type=simple
ExecStart="/usr/local/bin/runner" run --config "/etc/runner/config.toml"
Restart=on-failure
RestartSec=10s
TimeoutStopSec=7260
Environment="RUNNER_SERVICE_VERSION=2"
Environment="RUNNER_SERVICE_STOP_TIMEOUT=7260"
NoNewPrivileges=true
ProtectSystem=strict
LogsDirectory=runner
ReadWritePaths=/tmp
```

- `ReadWritePaths` 只剩 `/tmp`（由 `filepath.Dir(lock.DefaultPath)` 推得，供 lock）。移除 docker socket：kernel 檢查 AF_UNIX `connect` 的寫入權限時不受唯讀 bind mount 影響，此點須於 E2E 實測；若不成立，改回並加 `-` 前綴
- `LogsDirectory`：預設 `runner`。config 明確設定的 `log-file` 通過 `layout.SystemLogsDirectory` 時改用其相對目錄；不通過時安裝階段警告並維持 `runner`，runner 執行時也依同一規則改寫預設位置（D.1），兩者一致。config 目錄永遠維持唯讀
- **跳脫**：`ExecStart` 的每個字以雙引號包住。執行檔（第一個字）只把 `%` 寫成 `%%`：systemd 會展開執行檔路徑中的指定符，但不展開 `$`，且拒絕其中的引號、反斜線、控制字元與 glob 字元（`*?[`），因此路徑含這些字元（或不是合法 UTF-8）時拒絕產生 unit。其餘參數跳脫 `\`、`"`，並把 `%`、`$` 寫成 `%%`、`$$`；`Environment=` 的值只跳脫 `\`、`"`、`%`。任何路徑或值含控制字元（含換行）時拒絕產生 unit
- **解析**：migrate 讀取既有 unit 的 `ExecStart` 時，須還原上述引號與跳脫（含舊版未加引號的 unit），render → parse 必須 round-trip
- user unit：新增 `RUNNER_SERVICE_VERSION`、`RUNNER_SERVICE_STOP_TIMEOUT`，以及安裝當下為絕對路徑的 `XDG_CONFIG_HOME` / `XDG_STATE_HOME`（讓 service 與 CLI 算出相同路徑）

#### C.2 launchd plist（macOS，僅 LaunchAgent）

- `StandardOutPath` → `/dev/null`：runner 自己寫有輪替的 `~/Library/Logs/runner/runner.log`，不再重複
- `StandardErrorPath` → `~/Library/Logs/runner/stderr.log`：內容為啟動失敗、panic、`tart pull` 的輸出，以及 D.1 fallback 時的 log；**不輪替**
- `EnvironmentVariables`：`RUNNER_SERVICE_VERSION`、`RUNNER_SERVICE_STOP_TIMEOUT`，以及安裝當下為絕對路徑的 XDG 變數；PATH 不寫入（見 D.1）
- 所有字串值以 XML escape 寫入
- 安裝時建立 `~/Library/Logs/runner/`（0700）

#### C.3 共用的安裝準備與 binary 檢查

所有會安裝 service 的入口（`runner service install`、`runner migrate`）共用：

1. **安裝參數準備**（`buildInstallOpts`）：一次讀取 config，得出 provider、`drain-timeout`、`log-file`，並帶入安裝當下的 XDG 變數。config 存在但無法讀取、語法錯誤或載入失敗時一律中止；config 不存在時，全新安裝只警告，`--force` 與 migrate 則中止
2. **binary 驗證**（`validateServiceBinary`）：binary 路徑一律 `EvalSymlinks` 成真實路徑（解析失敗即中止）。Linux 上以 root 執行的 service（system scope，或由 root 安裝的 user scope）另要求真實路徑為 regular file，且它本身與每一層上層目錄直到 `/` 皆通過 `pathtrust.Problem`。寫進定義的就是檢查過的真實路徑
3. **產生定義**：在寫入任何檔案前完成 render（含 C.1 的跳脫檢查）

以上任一步失敗時不呼叫 service manager，既有定義保持原樣。binary 不符合時訊息列出違規路徑與原因，並提示：

```
sudo install -m 0755 <binary> /usr/local/bin/runner
sudo /usr/local/bin/runner service install <原本的 scope> [--force] [--no-start] --config-path <config> --binary-path /usr/local/bin/runner
```

migrate 時第二行改為以 `/usr/local/bin/runner` 重跑相同選項的 `migrate`。

只在安裝以 root 執行的 service 時檢查 binary：常駐的 root service 才是提權管道，手動 `sudo runner run` 的人本來就有 sudo。

#### C.4 `service install --force` 與其他 service 指令

- **`service install --force`**：service 已安裝時，先完成 C.3 的全部步驟，通過才以原子寫入取代既有定義，再重新載入並重啟（systemd：`daemon-reload` + `restart`，會等待 drain；launchd：`unload` + `load -w`）。未加 `--force` 且已安裝時，錯誤訊息提示 `--force`
- **`service uninstall`**：同時移除同層級的舊版定義（`runscaler.service`、`com.runscaler.agent.plist`），並說明移除了哪些；只找到舊版定義時不算錯誤
- **`service logs`**：Linux 不變（journalctl）；macOS 讀 `~/Library/Logs/runner/stderr.log`，不存在時依層級 fallback 到舊路徑（使用者 `~/Library/Logs/runner.log`、system `/var/log/runner.log`）並加註
- **`service status --user=false`**：macOS 非 root 執行時提示需要 sudo
- **`install.sh`**：以 root 執行時 `INSTALL_DIR` 預設 `/usr/local/bin`；一般使用者維持 `~/.local/bin`；`INSTALL_DIR` 環境變數仍可覆寫
- **`runner update`**：取代 binary 時遇到權限不足（`errors.Is(err, fs.ErrPermission)`），提示改用 `sudo runner update`

### D. 執行時行為

#### D.1 `runner run`

- **macOS root**：一開始即拒絕，exit 1，訊息說明改以登入使用者執行並安裝 LaunchAgent；`/Library/LaunchDaemons/io.github.ysya.runner.plist` 或 `/Library/LaunchDaemons/com.runscaler.agent.plist` 存在時，附上 E.2 的轉換指引
- **log 路徑決策**（純函式）：`log-file = ""` 停用；未設定用 `layout.For` 的預設；root 明確設定時，通過 `layout.SystemLogsDirectory` 才採用（並以預設路徑作為開檔失敗時的 fallback），不通過則警告並改用預設。無法取得預設（使用者缺 `$HOME` 且無對應 XDG 變數）時停用檔案記錄並警告
- **使用者開檔**：建立目錄（0700）、開啟目錄後，log 檔的開啟與輪替皆相對於該目錄 fd，並對 log 檔名加 `O_NOFOLLOW`
- **root 開檔**：從 `/` 逐層以 `O_DIRECTORY|O_NOFOLLOW` 開啟到 log 所在目錄；`/var/log` 之下的每一層若不存在就以 0755 建立，並對開啟後的 fd 做 `pathtrust.Problem` 檢查。log 檔的開啟、輪替時的 rename 與 unlink 全部相對於最後的目錄 fd。由於寫入者持有目錄 fd，事後任何上層被改名或換成 symlink 都無法導走寫入；因此不要求 `/var/log` 本身為 root 專屬（它在部分發行版為群組可寫），能改寫 `/var/log` 的人至多把另一個 root 專屬目錄改名到該位置，runner 仍只會寫入固定檔名的 log
- **開檔失敗**：警告寫到 stderr（原本寫 stdout）；有 fallback 時改開 fallback，否則繼續執行但不寫檔
- **stdout 為 `/dev/null` 時的 fallback**：啟動時以 `os.SameFile` 比對 stdout 與 `/dev/null`。若相同且沒有開啟 log 檔，logger 改寫 stderr
- **不傳型別化 nil**：沒有 log 檔時傳入 logger 的 writer 必須是 nil interface
- **Homebrew PATH**（已實作，未 commit）：`main()` 呼叫 `ensureHomebrewPath()`，macOS 上把存在且缺少的 `/opt/homebrew/bin`、`/usr/local/bin` 附加到 PATH 尾端
- **cobra**：`runner run` 執行期錯誤不再輸出整份 usage
- **舊 log 位置提示**：未設 `log-file`、有使用 config 檔、`<config 目錄>/runner.log` 存在且不等於新路徑時，每次啟動印一行 INFO（新路徑、舊檔留在原處）

#### D.2 lock

路徑不變。開檔流程改為：

1. `open(O_RDWR|O_NOFOLLOW|O_CLOEXEC)`，不帶 `O_CREAT`
2. `ENOENT` 時 `open(O_RDWR|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC, 0666)`；`EEXIST` 時回到步驟 1（避開 `protected_regular` 對 `O_CREAT` 的限制與建立競態）
3. `Fstat`：必須是 regular file，且 hardlink 數為 1
4. 只有檔案擁有者等於自己的 effective UID 時才 `Fchmod(0666)`
5. `flock(LOCK_EX|LOCK_NB)`；成功後才寫診斷資訊

lock 是防止誤啟動第二個實例的安全網，不是安全邊界：本機使用者仍可搶先占鎖，或刪除自己建立的 lock 檔使後續程序鎖到不同 inode。`Info` 不新增欄位。唯讀檔案系統的重裝提示（`explainLockError`）改為 D.3 的修正指令，並帶入實際 config 與 binary 路徑。

#### D.3 過舊 service 提醒（取代 `logServiceDrainTimeoutReminder`）

- **判定為 service**：`INVOCATION_ID` 非空（systemd），或 `XPC_SERVICE_NAME` 為 `io.github.ysya.runner` 或 `com.runscaler.agent`（launchd）；皆不符時不提醒
- **範本世代**：`RUNNER_SERVICE_VERSION` 缺少或無法解析視為 1；小於 2 時 WARN
- **停止逾時**：`RUNNER_SERVICE_STOP_TIMEOUT` 存在且小於 `drain-timeout + 1 分鐘` 時 WARN
- **提醒內容**：附上可直接執行的單一指令，帶入目前實際使用的 config（絕對路徑）與 binary（解析後）：
  - root：`sudo <binary> service install --user=false --force --config-path <config> --binary-path <binary>`
  - 使用者：`<binary> service install --user --force --config-path <config> --binary-path <binary>`
  - 本程序由舊版（runscaler）定義啟動時（launchd label 為 `com.runscaler.agent`，或 `/proc/self/cgroup` 顯示 systemd unit 為 `runscaler.service`），改為提示 `runner migrate`（root 加 `sudo` 與 `--user=false`，使用者加 `--user`）；只殘留舊定義檔而由新定義啟動時，仍提示 `--force` 重裝
  - root 執行且目前的 binary 不是只有 root 能改時，改為一行兩步驟：先 `sudo install -m 0755 <binary> /usr/local/bin/runner`，再以 `/usr/local/bin/runner` 執行上述 `--force` 指令；binary 已是 `/usr/local/bin/runner`（不安全的是上層目錄）時維持一般 `--force` 指令，執行時會列出違規路徑與原因
- 兩者皆通過時不輸出任何提醒

#### D.4 `runner logs`

1. 有 `--config`：該 config 決定的路徑（D.1 的決策）；無 `--config`：搜尋到的 config 或預設
2. 每次開啟（含 `-f` 輪替後重開）都以 `O_RDONLY|O_NONBLOCK` 開啟，再對開啟後的 fd 確認為 regular file；否則拒絕讀取，避免 FIFO 阻塞或讀取裝置
3. 找不到時列出查過的路徑；一般使用者找不到而 `/var/log/runner/runner.log` 存在時，提示 `sudo runner logs`
4. 權限不足時提示 `sudo runner logs`

不讀取 lock 檔內容：lock 檔為 0666，任何本機使用者都能改寫，不能作為路徑來源。

#### D.5 `runner migrate` / `runner init`

- **`migrate`**：
  - macOS 上 root（任何 scope）以 A.6 拒絕；非 root 的 `--user=false` 拒絕並引導 `runner migrate --user`
  - 備份目錄預設依遷移 scope 取 `layout.For`（`--backup-dir` 仍可覆寫）；舊目錄中的備份不動，因此升級後第一次 migrate 會在新目錄建立新備份
  - 安裝新 service 時經過 C.3 的全部步驟（與 `service install` 產生相同定義）
- **`init`**：未給 `--output` 時寫到 `layout.For` 的 config 預設位置；建立上層目錄（使用者層級 0700、`/etc/runner` 0755）；以既有的 `writeFileAtomic` 寫入 0600（不跟隨目的地 symlink、確保新檔權限）；完成後印出路徑。目前目錄已有 `config.toml` 且不同於輸出路徑時，警告它會在 config 搜尋中優先於新檔
- `init` 產生的 config 與 `config.example.toml` 中「log-file 預設在 config 旁」的註解改為依身分的新預設

### E. 既有安裝的過渡

不自動搬移，只提示與重裝。

#### E.1 各情境

| 既有狀態 | 升級新 binary 後 | 操作者要做的事 |
|---|---|---|
| Linux system unit | lock 開檔失敗，錯誤訊息附重裝指令 | 執行印出的指令（binary 不是 root 專屬時，指令會先安裝一份到 `/usr/local/bin`） |
| Linux user unit | 照常執行；log 改寫新位置；D.3 提醒 | 依提醒執行 `--force` 重裝 |
| macOS LaunchAgent | 照常執行；log 改寫新位置；D.3 提醒 | 依提醒執行 `--force` 重裝；舊的無限成長 log 即停止寫入 |
| macOS LaunchDaemon | `runner run` 被拒絕，daemon 重複重啟 | 依 E.2 轉換 |
| tmux／前景執行 | log 改寫新位置；D.1 提示舊位置 | 無（刪除舊 log 可停止提示） |

#### E.2 macOS LaunchDaemon → LaunchAgent（README 提供完整步驟）

1. 保存原設定：`sudo cat /Library/LaunchDaemons/io.github.ysya.runner.plist`（或舊版 `com.runscaler.agent.plist`），記下 `ProgramArguments` 中的 binary 與 config 路徑
2. 移除 system service：`sudo runner service uninstall --user=false`（同時移除舊版 plist）
3. 交接 config：`mkdir -p ~/.config/runner && chmod 700 ~/.config/runner`，再 `sudo install -o "$USER" -m 0600 <原 config> ~/.config/runner/config.toml`
4. Tart image：root 使用的 image 位於 `/var/root/.tart`，登入使用者看不到，需以使用者身分重新 `tart pull`（config 中的 `tart.home` 若指向 root 的目錄須一併修改）
5. binary：放在使用者可執行的位置（`~/.local/bin` 或維持 `/usr/local/bin`；後者 `runner update` 需 sudo）
6. 以登入使用者安裝：`runner service install --config-path ~/.config/runner/config.toml`
7. 開啟自動登入，確保重開機後 LaunchAgent 會啟動

README 新增升級說明涵蓋 E.1、E.2，並同步修正所有仍描述舊預設的段落（Quick Start 的 `--config config.toml`、migrate 備份位置、XDG 與 `init` 預設輸出的說明）。版本號為 **v0.10.0**（預設行為改變）。

## 錯誤處理

| 情境 | 行為 |
|---|---|
| macOS 以 root `run` | 拒絕，exit 1，附 E.2 指引 |
| macOS 以 root 執行 `service install/start/restart` 或 `migrate` | 拒絕，說明只支援以登入使用者安裝 LaunchAgent |
| macOS 非 root `migrate --user=false` | 拒絕，引導 `runner migrate --user` |
| 安裝時 config 存在但無法讀取或載入 | 拒絕，既有定義不變 |
| `--force` 或 migrate 時 config 不存在 | 拒絕，既有定義不變 |
| 全新安裝時 config 不存在 | 警告，照常安裝 |
| Linux 上安裝以 root 執行的 service，binary 解析失敗或路徑不安全 | 拒絕；解析失敗時顯示解析錯誤，路徑不安全時列出違規路徑與原因並提示 `sudo install` 到 `/usr/local/bin`（binary 已在該處時改為提示修正違規路徑的權限） |
| 路徑或值含控制字元 | 拒絕產生 unit |
| `service install` 已安裝且未加 `--force` | 拒絕，提示 `--force` |
| system unit 的 `log-file` 不符合 `SystemLogsDirectory` | 安裝時警告，照常安裝（runner 執行時改寫預設位置） |
| root 的 `log-file` 不符合 `SystemLogsDirectory` | 啟動時警告，改用預設路徑 |
| root 開 log 時遇到 symlink 或不安全的目錄 | 警告，改開 fallback；無 fallback 則不寫檔 |
| log 目錄建立或開檔失敗 | stderr 警告，繼續執行；stdout 為 `/dev/null` 時 logger 改寫 stderr |
| service 範本過舊或停止逾時不足 | WARN 並附 D.3 的修正指令，繼續執行 |
| lock 所在檔案系統唯讀 | 拒絕啟動，附 D.3 的修正指令 |
| lock 檔為 hardlink 或非 regular file | 拒絕啟動，說明 `/tmp/runner.lock` 異常 |
| `runner logs` 權限不足 | 錯誤並提示 `sudo runner logs` |
| `runner logs` 目標（含 `-f` 重開後）不是 regular file | 錯誤，不讀取 |
| `runner logs` 找不到檔案 | 列出查過的路徑；必要時提示 `sudo runner logs` |
| `runner update` 權限不足 | 錯誤並提示 `sudo runner update` |
| macOS `service status --user=false` 非 root | 提示需要 sudo |
| `init` 輸出被目前目錄的 `config.toml` 遮蔽 | 警告，照常寫入 |

## 測試

- **`internal/layout`**：`GOOS` × root/使用者 × XDG（未設、絕對、相對）× `$HOME` 有無的表格測試，期望值為手寫字面路徑；`SystemLogsDirectory` 的接受與拒絕案例
- **`internal/pathtrust`**：擁有者、group 可寫、others 可寫、symlink 的表格測試
- **log 檔**：使用者模式 log 檔名為 symlink 時拒絕；root 模式（以注入的所有權檢查在非 root 下測試）中間目錄為 symlink 時拒絕、目錄權限不安全時拒絕、缺少的目錄被建立；開啟後把目錄改名並在原位置放 symlink，輪替產生的檔案仍留在原目錄
- **log 路徑決策**：純函式表格測試（使用者、root 預設、root 合法明確設定含 fallback、root 不合法設定、停用、缺 `$HOME`）；開檔失敗時改開 fallback
- **service 範本**：
  - unit：`LogsDirectory` 預設與自訂、`ReadWritePaths` 只有 `/tmp`、版本與停止逾時環境變數、user unit 的 XDG 變數、`ExecStart` 參數對 `%`、`$`、`"`、`\`、空白的跳脫、執行檔路徑只跳脫 `%` 並拒絕引號、反斜線、glob 字元與非 UTF-8、控制字元被拒；render → parse round-trip（含舊版未加引號的 unit）
  - plist：stdout 為 `/dev/null`、stderr 路徑、環境變數、含 `&`／`<` 的路徑經 XML escape 後能被解析；macOS 上 `plutil -lint` 通過（不載入）
- **安裝準備**：config 不存在（全新安裝警告、必要時拒絕）、語法錯誤、載入失敗、合法 config 的各項事實；`service install` 與 migrate 產生相同的 `installOpts`
- **binary 安全檢查**：注入 stat 的表格測試，涵蓋擁有者非 root、group 可寫、others 可寫、祖先目錄不安全、非 regular file、symlink 解析、解析失敗；真實檔案測試（`/bin/sh` 為 root 專屬時通過；權限設為 0666 的檔案必定被拒）
- **`installService`**：binary 驗證或 render 失敗時不呼叫 service manager
- **lock**：`ENOENT`→`O_EXCL` 建立、`EEXIST` 重試、hardlink 數大於 1 被拒、非擁有者不呼叫 `Fchmod`（以注入的系統呼叫測試）
- **stdout fallback**：注入「stdout 是否為 `/dev/null`」與 log 檔可用性；nil log writer 不 panic
- **過舊 service 提醒**：注入環境變數與身分的表格測試（systemd、兩個 launchd label、`XPC_SERVICE_NAME=0`、版本缺少／無法解析／過舊／最新、停止逾時不足／足夠），並檢查提醒指令帶入實際路徑
- **`runner logs`**：FIFO 與字元裝置被拒且不阻塞、`-f` 期間檔案被換成 FIFO 時返回錯誤、找不到與權限不足的訊息與 sudo 提示
- **`init`**：`writeFileAtomic` 寫出 0600（含覆寫既有 0644 檔）、遮蔽警告；**`migrate`** 備份預設位置、macOS root 與 scope 拒絕、binary 不安全時不動任何服務；**舊 log 位置提示**
- **Homebrew PATH**：非目錄候選、重複候選、非 macOS 不變動
- **端對端（Linux，暫時 systemd 容器，所有等待都有期限與逾時失敗，結束後只移除本次建立的容器與映像）**：
  - 新裝：binary 在 root 擁有的 `/usr/local/bin`，`systemd-analyze verify` 通過，service 越過 lock（以後續的 tart 錯誤證明）、log 寫入 `/var/log/runner/runner.log`、lock 為 root 擁有 0666
  - docker socket：未列入 `ReadWritePaths` 時仍能連線（驗證 C.1 的假設）
  - 升級：舊 unit + 新 binary 出現 `--force` 指令；執行後 unit 更新
  - 拒絕：binary 在一般使用者可寫位置、或 config 語法錯誤時，`--force` 失敗且既有 unit 的雜湊不變
  - 跨身分 lock：root 先建立後一般使用者越過 lock；一般使用者先建立後 root 在 `fs.protected_regular=1` 下越過 lock（修改 sysctl 前保存原值，以 trap 保證還原）
- **macOS**：單元測試（含 `plutil -lint`）；以隔離的 `HOME` 手動執行，確認 log 寫入 `$HOME/Library/Logs/runner/`，模擬 launchd 環境時出現過舊提醒。不實際載入 LaunchAgent；有其他 runner 持鎖時略過並說明；`sudo runner run` 的拒絕需互動輸入密碼，僅以單元測試涵蓋

## 受影響檔案

| 檔案 | 變更 |
|---|---|
| `internal/layout/layout.go`、`layout_test.go` | 新增 |
| `internal/pathtrust/pathtrust.go`、`pathtrust_test.go` | 新增 |
| `internal/lock/lock.go` | 開檔流程（D.2） |
| `internal/config/logfile.go` | 目錄 fd 的開檔與輪替、root 模式逐層檢查 |
| `internal/config/config.go` | logger 的 console 參數 |
| `cmd/runner/trusted_path.go` | 新增：路徑版的 root 專屬檢查（使用 `pathtrust`） |
| `cmd/runner/logpath.go` | 新增：log 路徑決策、開檔與 fallback、console 選擇、nil writer |
| `cmd/runner/service_env.go` | 新增：過舊 service 提醒與重裝指令 |
| `cmd/runner/platform_guard.go` | 新增：macOS root 拒絕 |
| `cmd/runner/config_paths.go` | `userConfigPath` 改用 `Identity.ConfigHome()` |
| `cmd/runner/loadconfig.go` | 移除 `resolveLogFilePath` |
| `cmd/runner/main.go` | log 設定、macOS root 拒絕、usage 靜音、過舊 service 提醒、舊 log 位置提示；（已實作）`ensureHomebrewPath`、`explainLockError` |
| `cmd/runner/tool_path.go`、`tool_path_test.go` | （已實作）Homebrew PATH；抽出 `homebrewPath` 並補測試 |
| `cmd/runner/cmd_service.go` | 範本、跳脫與解析、`buildInstallOpts`、binary 驗證、`installService`、`--force`、uninstall 舊版、`service logs` fallback、`service status` 提示 |
| `cmd/runner/cmd_logs.go` | 路徑決策、fd 版 regular file 檢查（含 `-f` 重開）、sudo 提示 |
| `cmd/runner/cmd_migrate.go` | 備份預設、macOS 拒絕、共用安裝準備與檢查、`ExecStart` 解析 |
| `cmd/runner/cmd_init.go` | 預設輸出位置、`writeFileAtomic`、遮蔽警告、config 註解 |
| `cmd/runner/cmd_update.go` | 權限不足提示 |
| `install.sh` | root 預設安裝目錄 |
| `config.example.toml` | `log-file` 註解 |
| `README.md` | Quick Start、Logs、Deployment、Service layout、升級說明（E.1、E.2）、migrate 備份與 XDG 說明 |
| 相關 `_test.go` | 見「測試」 |

## 審查紀錄

### 2026-09-16 Codex 審查初版設計

逐條對照程式碼與文件查證後：

- **採用**：H1（stdout fallback）、H2／M1（自訂 log 限定 `/var/log/<子目錄>`）、H3（取消 gid 0 例外）、H4／M7（共用檢查入口、真實路徑）、H5（`runner logs` 不讀 lock 檔）、H6（root log 限制）、H7（lock 開檔流程）、H8／M10（LaunchDaemon 轉換步驟、舊版定義清除與 log fallback）、H9（`service install --force`）、M2、M3、M4、M6（舊 label `com.runscaler.agent`，git 歷史查證）、M9、M11、M12、L2
- **修正說法後採用**：M5（`RunStreaming` 只用於 `tart pull`，非持續寫入；改為如實描述 stderr 內容且不承諾輪替）、L1（移除 docker socket 例外，待 E2E 實測）、M8（改為明文界定 lock 非安全邊界，補 hardlink 檢查）、L3（刪除「精準偵測」措辭，另補停止逾時比對）

### 2026-09-17 Codex 審查實作計畫

- **採用**：
  - `--force` 與 migrate 須驗證 config 可讀與可載入（既有 `detectDrainTimeout` 會吞掉讀檔錯誤）→ C.3 `buildInstallOpts`
  - migrate 須與 `service install` 共用安裝參數（原本漏傳自訂 log 與 XDG）→ C.3、D.5
  - `ExecStart` 解析改為還原引號與跳脫（原本 `strings.Fields` 會截斷含空白的路徑）→ C.1
  - `ExecStart` 須跳脫 `$`，並拒絕控制字元 → C.1
  - `LogsDirectory` 與執行時使用同一路徑規則，並在開檔失敗時改開 fallback → B、C.1、D.1
  - `runner logs` 改為對開啟後的 fd 檢查 regular file，`-f` 重開時重做 → D.4
  - macOS root 可經由 `migrate --user` 繞過安裝限制 → A.6、D.5
  - `XDG_CONFIG_HOME` 為絕對路徑時不應要求 `$HOME` → A.4、B
  - E2E：安裝 `procps`、sysctl 以 trap 還原、所有等待有期限、以後續錯誤證明越過 lock、只清除本次建立的映像、macOS 使用隔離 `HOME`
  - README 仍有描述舊預設的段落、LaunchDaemon 轉換缺少建立目的目錄
  - 計畫層級的編譯缺口、錯誤的測試期望、真實路徑測試對主機的假設
- **部分採用**：root log 的祖先鏈。改為從 `/` 逐層 `O_NOFOLLOW` 開啟並對 `/var/log` 之下的目錄 fd 檢查，開檔與輪替全部相對於目錄 fd；不要求 `/var/log` 本身為 root 專屬，理由見 D.1
- **待主機確認**：leg host 的 systemd 版本、實際 unit 與 drop-in、`fs.protected_regular`／`fs.protected_hardlinks`、`/tmp/runner.lock` 擁有者、binary 擁有者與權限
