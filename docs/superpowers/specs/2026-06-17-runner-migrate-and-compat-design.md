# 設計:`runner migrate` + 升級相容(漸進式 deprecation)

- 日期:2026-06-17
- 狀態:草案(待 review)
- 前置:`runscaler`→`runner` 改名 refactor 已完成並 merge 到 main
  (spec: `docs/superpowers/specs/2026-06-16-rename-runscaler-to-runner-design.md`)

## 背景與動機

改名 refactor 把 binary、service 識別碼、config 路徑、docker volume、self-update 資產都從 `runscaler` 換成 `runner`,啟動也從根指令副作用改成 `runner run` 子指令。對**全新安裝**沒問題,但對**既有 runscaler 部署**,升級後會遇到:

1. config 還在 `/etc/runscaler/config.toml`,新版只找 `/etc/runner/`。
2. 舊 systemd/launchd service 的 `ExecStart` 是 `runscaler --config ...`(裸跑),新版裸跑印 help 不啟動 → service 失效。
3. 舊 docker volume `runscaler-shared`、舊 env var `RUNSCALER_TOKEN` 殘留。

本案採**業界主流的漸進式 deprecation**(參考 GitHub CLI config 目錄遷移、AWS CLI v1→v2、Docker Compose v1→v2 drop-in、PostgreSQL binary 改名),而非「拒絕啟動直到遷移」的強制 gate(後者主要用於 DB schema 一致性,對 CLI 改名偏 aggressive 且會讓 service crash-loop)。

## 設計原則(為何不用強制 gate)

| 業界做法 | 對應本案 |
|---------|---------|
| GitHub CLI:新位置沒有就自動 fallback 讀舊 + 警告;env override 時不干擾 | config fallback 讀 `/etc/runscaler` + 警告;尊重 `--config` |
| Docker Compose v2:drop-in replacement,舊呼叫仍可用 | 舊式 `runner --config`(裸跑)→ 警告 + 仍啟動 |
| AWS CLI v2:警告 + 建議,不拒絕執行 | 全程印 deprecation 警告,不中斷 |
| 一次性清理 | opt-in `runner migrate`(不強制) |

**核心取捨(已與使用者確認方向:做完整、可長期使用):** 採漸進 drop-in,既有 service 升級後**繼續運作**(印警告),不 crash-loop。代價是過渡期容忍 `runner --config`(帶 flag 的舊式啟動);但純 `runner`(無啟動 flag)裸跑仍是 help,守住「裸跑不啟動」的主要訴求。相容碼隔離成獨立模組,長期保留供升級者使用,未來要移除時集中且容易。

## 目標
- 既有 runscaler 部署升級後**不中斷**(config 讀得到、service 跑得起來),並被引導遷移。
- 提供 `runner migrate` 一鍵把本機狀態(config / volume / service)轉成新格式,system + user 皆支援。
- 相容機制 first-class:隔離、有測試、可長期維護。

## 非目標
- 不做強制 gate(拒絕啟動)。
- 不自動在啟動時改寫使用者的系統檔(搬 config / 改 service 只由明確的 `migrate` 做)。
- 不改 TOML schema(config 欄位不變)。

## 詳細設計

### A. config 路徑 fallback(`cmd/runner/loadconfig.go`)
- 預設搜尋路徑加入舊路徑,排在新路徑之後:`.` → `/etc/runner` → `/etc/runscaler`(legacy)。
- 當實際從 `/etc/runscaler` 讀到 config 時,印 deprecation 警告(stderr):建議搬到 `/etc/runner` 或執行 `runner migrate`。
- 明確 `--config` 時:照舊只讀指定檔,不 fallback、不警告(尊重使用者意圖,對齊 GitHub CLI 的 env-override 行為)。

### B. 舊式啟動 drop-in 相容(`cmd/runner/main.go` 根指令)
根指令重新獲得一個 RunE,作用是**分流**(預設維持不啟動,只在偵測到舊式呼叫時為相容而啟動):
- `runner`(無啟動 flag)→ 印 help(維持改名 refactor 的「裸跑不啟動」)。
- `runner --config <path>`(裸跑帶 `--config`,即 self-update 後舊 service 的標準呼叫)→ 印 deprecation 警告「直接啟動已 deprecated,請改用 `runner run`」→ 接著執行與 `run` 相同的啟動流程。
- `runner run ...` → 正常啟動(不變)。

實作要點:`--config` 是根的 persistent flag(改名 refactor 已是),所以根 RunE 可偵測它是否被設定。啟動邏輯抽成一個共用函式(例如 `startScaling(cmd)`),`run` 子指令與根 RunE 的 drop-in 分支都呼叫它,避免重複。範圍說明:drop-in 以標準 service 用的 `--config` 為準;若舊呼叫用純 flags(`--url` 等,這些在 `run` 子指令上),會得到 cobra 的 unknown-flag 錯誤並提示改用 `runner run`(可接受,非標準 service 場景)。

### C. `runner migrate [--user]` 指令(`cmd/runner/cmd_migrate.go`,新)
opt-in 一鍵遷移,逐步驟、冪等(每步先偵測,無舊狀態就跳過):
1. **config**:`/etc/runscaler/config.toml` 存在且 `/etc/runner/config.toml` 不存在 → 建立 `/etc/runner/` 並 `mv` 搬移。目標已存在 → 跳過 + 警告(不覆蓋)。
2. **service**:偵測舊 unit/plist(system + user 兩層,見 D 的偵測)→ stop/disable/unload + 移除舊檔 → 用既有 service 安裝邏輯裝新 service(`runner.service` / `io.github.ysya.runner`,ExecStart = `runner run --config /etc/runner/config.toml`)。
3. **volume**:清舊 `runscaler-shared`(重用 doctor 的 volume 清理)。
4. **env var**:偵測到 `RUNSCALER_TOKEN` → 印提示(改用 `RUNNER_TOKEN`;shell/外部 env 由使用者改)。
5. **權限**:system 需 root(重用 `checkPrivileges`);`--user` 不需。
6. 全部無舊狀態 → 印 "nothing to migrate"。逐步回報;某步失敗則停在該步(service uninstall 失敗就不接 install),可重跑。

### D. legacy 隔離模組(`cmd/runner/legacy.go`,新)
集中所有 legacy 識別碼與偵測,讓相容碼可長期維護、未來一次移除:
- 常數:`legacyConfigPath = /etc/runscaler/config.toml`、`legacySystemdUnit = runscaler.service`、`legacyLaunchdLabel = com.runscaler.agent`、`legacyLaunchdPlist`、`legacySharedVolume = runscaler-shared`、`legacyTokenEnv = RUNSCALER_TOKEN`。
- 偵測函式:`legacyConfigExists()`、`legacyServiceInstalled(user bool)`(檢查 system/user 的舊 unit/plist 路徑)。
- 共用 deprecation 警告 helper。
- A(config fallback warning)、B(drop-in warning)、C(migrate 偵測)都引用此模組,避免散落。

### 既有程式碼調整
- `cmd_service.go`:把 uninstall 參數化(接受 unit file / label / service name),讓 `migrate` 能移除 **legacy 識別碼** 的 service,既有 `service uninstall` 仍用新識別碼。
- `cmd_doctor.go`:volume 清理邏輯抽成可被 `migrate` 呼叫(目前 `checkDockerVolume` 已能處理 `runscaler-shared`)。

## 相容 / 冪等
- 冪等:遷移後舊狀態消失 → 警告不再出現、重跑 migrate = no-op、全新安裝完全無感。
- drop-in 與 fallback 長期保留(隔離在 `legacy.go`),有 deprecation 警告引導;未來移除時只動 `legacy.go` 與少數呼叫點。

## 錯誤處理
- migrate 每步驟獨立回報成功/失敗;失敗即停(不做半套破壞),使用者修正後可重跑(冪等保證安全)。
- config `mv` 前確認目標不存在(不覆蓋)。
- service install 前先成功 uninstall 舊的;任一失敗則保留現狀並回報。

## 測試
- `legacy.go` 偵測:給定 temp dir 的舊 config / 舊 unit 檔 → 偵測正確;無 → false。
- `migrate` config 搬移:temp dir 模擬 `/etc/runscaler`→`/etc/runner`(注入路徑);目標已存在 → 跳過。
- `migrate` service:用既有 serviceManager 的 fake / 參數化 uninstall 驗證舊識別碼被移除、新識別碼被安裝。
- drop-in(root RunE):`runner`(無 flag)→ help、不啟動;`runner --config X` → 走啟動分支(以可注入的 startScaling 驗證,不真的連 GitHub)。
- config fallback:從 legacy 路徑讀到 → 有警告;`--config` 指定 → 無 fallback / 無警告。
- 冪等:migrate 後偵測回 false。

## 受影響檔案
- 新增:`cmd/runner/cmd_migrate.go`、`cmd/runner/legacy.go`、`cmd/runner/cmd_migrate_test.go`、`cmd/runner/legacy_test.go`
- 修改:`cmd/runner/main.go`(root RunE drop-in + 抽 `startScaling` + AddCommand)、`cmd/runner/loadconfig.go`(fallback + 警告)、`cmd/runner/cmd_service.go`(參數化 uninstall)、`cmd/runner/cmd_doctor.go`(volume 清理可重用)
- 文件:`README.md` 的 "Upgrading from runscaler" 改成「跑 `runner migrate`」為主,手動步驟作為備選

## 推出順序建議
1. `legacy.go`(常數 + 偵測 + 警告)+ 測試
2. config fallback(A)+ 測試
3. 抽 `startScaling` + root RunE drop-in(B)+ 測試
4. 參數化 service uninstall
5. `runner migrate`(C)+ 測試
6. README migration 段更新
7. 全量驗證
