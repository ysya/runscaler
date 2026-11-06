# 設計:統一快取架構與磁碟守門員

日期:2026-08-13
狀態:設計已確認,待實作

## 背景與動機

快取相關機制是分四次長出來的,彼此沒有共同語彙,每一個面向都不一致:

| 機制 | 程序退出時 | 容量上限 | 時間淘汰 | 預設 | 可見性 |
| --- | --- | --- | --- | --- | --- |
| Docker layer / build cache | 保留 | `build-cache-budget` | `build-cache-max-age` | prune 關 | 無 |
| `cache-volumes` | 保留 | **無** | **無** | — | 無 |
| `shared-volume` | **刪除** | 無 | `shared-volume-ttl` | — | 無 |
| Tart OCI/IPSW | 保留 | `cache-space-budget` | `cache-max-age` | 開 | 無 |

具體症狀:

- **命名分歧**:同樣是容量上限,叫 `build-cache-budget` 和 `cache-space-budget`;同樣是時間淘汰,有 `build-cache-max-age`、`cache-max-age`、`shared-volume-ttl` 三種說法。
- **生命週期分歧**:只有 shared volume 在程序退出時被刪除。
- **能力缺口**:`cache-volumes` 沒有任何容量控制。這在 knowknow-rn 要放 20GB ccache 時暴露出來——當時唯一的閘門是 ccache 自己的 `max_size`。
- **無可見性**:四者都無法回答「快取現在吃掉多少磁碟」。實例:被問到某台主機磁碟多大時,除了登入該主機沒有其他辦法。
- **語意誤解**:`build-cache-budget` 曾被外部誤認為通用的 volume 配額,實際上只作用於 BuildKit 的層快取。

這些症狀的共同根源是缺少統攝全局的東西:現有機制全是 per-store 的局部控制,而操作者真正在意的是「這台主機的磁碟會不會被塞爆」。

### 業界前例

兩個最成熟的同類實作都以**可用磁碟空間**為主控制,而非 per-store 配額:

- **BuildKit**(`worker.oci.gcpolicy`):`reservedSpace`(保證保留)、`maxUsedSpace`(超過即回收)、`minFreeSpace`(GC 要留下的可用空間),外加 `keepDuration` 做時間淘汰。所有空間參數皆可寫成位元組、帶單位字串(`"512MB"`),**或整顆磁碟的百分比(`"10%"`)**。
- **Nix**(`min-free` / `max-free`):建置期間可用空間掉到 `min-free` 以下即觸發 GC,回收到有 `max-free` 可用為止。

採用此模型的三個理由:

1. 操作者的目標是磁碟性質,不是單一快取的性質。
2. **per-store 配額不會相加**:五個快取各 20GB,在 31GB 的主機上無法從個別數字推出整體是否安全。
3. **百分比單位解決異質機群**:同一份設定要在 31GB、126GB、931GB 三種磁碟上都合理,只有百分比做得到。

### 為何不採用現成方案

自架 `actions/cache` 相容伺服器(falcondev-oss/github-actions-cache-server 等)是成熟生態,但解決的是**不同問題**:keyed、可攜、跨機器的 job 快取。它不會阻止 daemon 用 dangling image 塞爆磁碟,不管 Tart 的 VM 映像,也不管 shared volume 的部署產物。對 ccache 這類大體積本地快取,逐 job 上傳下載反而更差。

主機端儲存治理沒有可重用的函式庫:BuildKit 與 Nix 的實作都內嵌在各自的 daemon 中;Go 生態的 cache library 全是記憶體內 LRU。可以借用的是模型,不是程式碼。

## 設計原則

**依「消失時會發生什麼」分類,而非依「叫不叫快取」。** 這是整份設計的核心判準:

| 類別 | 消失時 | 成員 |
| --- | --- | --- |
| `KindGarbage` | 無影響 | dangling image、stopped container、buildx 孤兒、TTL 過期的 shared 檔案 |
| `KindCache` | 變慢 | docker layer / build cache、cache volumes、Tart OCI |
| `KindScratch` | **進行中的 run 壞掉** | shared volume 在 run 進行中的內容 |

`shared-volume` 同時是後兩者:run 進行中是交接區(realmkit 的 build job 寫入 `$SHARED_DIR/deploy/`,後續 job 讀取),run 結束後是垃圾。**TTL 是唯一區隔這兩種狀態的東西**。

**不強行統一四種機制的形狀。** Tart 的 OCI/IPSW 快取由 `tart prune` 管理,不是我們掛載的 volume;硬塞進同一個設定形狀只會製造彆扭的抽象。要統一的是詞彙與語意,不是資料結構。

## 目標

1. 引入主機層級的磁碟守門員,對所有快取種類統一生效
2. 補上 `cache-volumes` 的容量控制
3. 提供快取用量的可見性
4. 統一容量與時間的詞彙,同時完全不破壞既有設定

## 非目標

- **自架 cache server / 跨機器 keyed cache**:不同問題領域,需要時採用現成方案
- **逐檔 LRU 淘汰 cache volume**:pnpm store 是內容定址加 hard link,gradle 的 `modules-2` 有 metadata 索引,外部逐檔刪除會弄壞它們。細粒度淘汰是工具特定的,只有工具自己做得對
- **在 runner 內重實作 BuildKit 的多規則 filter 系統**:目前的需求用單一門檻加階層即可

## 詳細設計

### A. `CacheStore` 抽象(`internal/cachestore`,新套件)

```go
type StoreKind int

const (
    KindGarbage StoreKind = iota // 消失沒有任何影響
    KindCache                    // 消失只是變慢
    KindScratch                  // 消失會弄壞進行中的 run
)

// CacheStore 是主機上一塊可回收的儲存。
type CacheStore interface {
    Name() string
    Kind() StoreKind
    // Path 回傳此 store 的資料實際所在路徑,用於解析它屬於哪個檔案系統。
    Path() string
    // Measure 回傳目前用量。可能昂貴(需掃描 volume),只在守門員實際
    // 觸發或使用者查詢時呼叫,不在便宜的門檻檢查路徑上。
    Measure(ctx context.Context) (bytes uint64, err error)
    // Reclaim 回收此 store 在指定階層可回收的部分,回傳釋出的位元組數。
    Reclaim(ctx context.Context, tier Tier) (freed uint64, err error)
}
```

實作對應現有機制:`dockerGarbageStore`(dangling image、stopped container)、`dockerBuildCacheStore`(BuildKit 層快取)、`buildxStore`、`sharedVolumeStore`、`tartCacheStore`、`cacheVolumeStore`。新增快取種類等於新增一個實作,守門員不需修改。

**一個 store 只能有一個 `Kind`,因此不可橫跨代價等級不同的階層。** Docker daemon 的可回收資料必須拆成兩個 store:垃圾(容器與 dangling image)是 `KindGarbage`→Tier1,build cache 是 `KindCache`→Tier2/Tier4。若合成一個 `KindGarbage` 的 store,`TiersFor` 會使守門員永遠不呼叫它的 Tier2,build cache 這個大宗回收來源就拿不到(leg host 上曾達 59.8GB)。這是「階層由 kind 推導」的直接推論,新增 store 時須一併檢查。

### B. 回收階層

階層**由 `Kind()` 推導**,而非手寫清單。這是型別層面的保證:新增 store 時不可能誤把交接區放進可整體回收的階層。

| Tier | 內容 | 對應 Kind |
| --- | --- | --- |
| 1 | dangling image、stopped container、buildx 孤兒 | `KindGarbage` |
| 2 | 超過 `max-age` 的 build cache、Tart OCI 舊層 | `KindCache` 的時間淘汰部分 |
| 3 | 超過 TTL 的 shared volume 檔案 | `KindScratch` 的已過期部分 |
| 4 | 清空 cache volumes | `KindCache` 的整體回收 |

`KindScratch` 未過期的部分**在任何階層都不可回收**。守門員逐階往下,直到可用空間達標或走完允許的最高階。

預設 `max-tier = 3`:純垃圾與低代價回收預設就做;清空 cache volumes 會讓下一輪 build 變冷,必須明確開啟。走完允許階層仍不足時,記錄警告並說明還差多少、以及提高階層可再回收多少。

**守門員與現有 sweeper 的關係:兩者並存,職責不同。** 現有的四個 sweeper(`startSharedVolumeCleanup`、`startBuildxCleanup`、`startDockerPrune`、`startTartCacheCleanup`)維持不變,它們是「第 2 層:各 store 依自己的保留策略定期整理」。守門員是額外的一層,只在**磁碟壓力**下才動作。實作時不刪除既有 sweeper,兩者透過同一組 `CacheStore` 實作共用回收邏輯即可。

**各 store 的啟用開關只約束例行清理,不約束守門員。**(2026-08-14 修訂;原設計為「守門員尊重停用開關」。)

`prune = false`、`buildx-cleanup = false`、`cache-cleanup = false` 的語意是「不要定期主動清理」,而不是「磁碟燒起來也別碰」。原設計讓守門員一併尊重這些開關,結果是:v0.4 起 `prune` 與 `buildx-cleanup` 預設為 false,`max-tier` 預設 3 又碰不到只在 Tier4 出現的 cache volume——**預設設定下守門員是開著的,卻一件東西都回收不了**,功能等於惰性出貨。這不是「操作者選擇退出」,而是「操作者從未選擇加入」,兩者被混為一談。

修訂後:sweeper 讀 `Enabled()` 決定要不要定期跑;守門員在磁碟壓力下對所有 store 一律可回收。

代價與退路:共用 daemon(非 runner 專用)的主機,可能在壓力下被清到非 runner 的物件。這種主機的正確做法是 `[disk] guard = false` 整個關掉守門員——那是一個明確表達「這台的磁碟不歸 runner 管」的開關,語意比借用 `prune` 精確。

### C. 磁碟守門員(`internal/diskguard`,新套件)

```toml
[disk]
guard       = true      # 預設開
min-free    = "10%"     # 可用空間低於此值觸發回收
target-free = "20%"     # 回收到此值為止
interval    = "1h"      # 週期檢查間隔
max-tier    = 3         # 允許回收到的最高階層
```

**per-filesystem,不是 per-host。** 門檻分別套用在每個 store 所在的檔案系統上。Mac Studio 的 `TART_HOME` 位於 `/Volumes/FrankData`(931GB),Docker 資料在系統碟——只看「主機可用空間」會完全失準。實作上以 `syscall.Statfs` 取得每個 `Path()` 所屬檔案系統的 `f_fsid`(或裝置編號)分組,同一檔案系統的 store 共用一次判斷與一輪回收。

**兩個觸發點:**

1. **週期檢查**:每 `interval` 一次,沿用現有 sweeper 的 goroutine 模式(`startSharedVolumeCleanup` 等)。
2. **啟動 runner 前檢查**:在 `Scaler.startRunner` 呼叫 backend 之前做一次 `statfs`。這只是一次系統呼叫(微秒級,不掃描任何 store),低於 `min-free` 才同步執行回收再開 job。這防的是「job 起來、把磁碟寫爆、整台失能」——leg host 曾因此磁碟達 98%。

**單位**:百分比(`"10%"`,相對於該檔案系統總容量)或絕對值(`"20GB"`)。解析後一律轉為位元組再比較。

**昂貴與便宜路徑分離**:門檻判斷只用 `statfs`;`Measure()` 只在守門員實際要回收(需要知道各 store 多大以決定順序)或使用者執行 `runner cache` 時呼叫。

### D. 詞彙統一與相容

新的規範詞彙:容量上限一律 `budget`,時間淘汰一律 `max-age`。

| 既有 key | 新 key | 處理 |
| --- | --- | --- |
| `build-cache-budget` | `build-cache-budget` | 不變(已符合) |
| `build-cache-max-age` | `build-cache-max-age` | 不變(已符合) |
| `cache-space-budget`(tart) | `cache-budget` | 舊 key 保留為 alias |
| `cache-max-age`(tart) | `cache-max-age` | 不變 |
| `shared-volume-ttl` | `shared-volume-max-age` | 舊 key 保留為 alias |

**既有設定零改動是硬性要求**:leg host、Mac Studio、tower-git-worker 都經由 self-update 取得新版,設定檔不會同步更新。alias 由 `internal/config/load.go` 的 map 合併層處理——舊 key 存在時映射到新 key,兩者同時存在則新 key 勝出並發出警告。alias 不觸發 unknown-key 警告。

### E. cache volume 的選配 budget

沿用 docker-compose 的 short/long syntax 模式:簡式維持現狀(完全向後相容),需要控制時改用完整式。

```toml
[docker]
# 簡式:多數快取不需要容量控制
cache-volumes = [
  "pnpm-store:/home/runner/.local/share/pnpm/store",
  "gradle-cache:/home/runner/.gradle",
]

# 完整式:需要 budget 或超標策略時
[[docker.cache]]
name      = "ccache"
path      = "/home/runner/.ccache"
budget    = "20GB"
on-exceed = "warn"    # warn(預設) | wipe
```

`on-exceed` 只有兩個值,兩者都安全且可預期:`warn` 記錄警告不動資料(細粒度淘汰交給工具自己的 LRU,例如 ccache 的 `max_size`);`wipe` 整個清空重建。**不提供逐檔淘汰**,理由見非目標。

**budget 與守門員階層是獨立的兩件事。** budget 屬於第 2 層(各 store 自己的保留策略):只要用量超過 `budget`,`on-exceed` 就生效,**與磁碟是否有壓力、與 `max-tier` 設到多少都無關**。守門員的第 4 階則是磁碟壓力下的緊急手段,會清空 cache volume 而不管它有沒有超過自己的 budget。兩者可能都想清空同一個 volume,實作上以同一個 `Reclaim` 路徑處理,先到先做即可。

超標檢查在守門員的週期沉澱中順帶進行(需要 `Measure()`),不在啟動 runner 的便宜路徑上。

### F. 可見性(`runner cache`,新指令)

```
$ runner cache
STORE               FILESYSTEM     SIZE      FS FREE    POLICY
docker-build-cache  /              12.4 GB   38% (48G)  max-age=168h
ccache              /              18.1 GB   38% (48G)  budget=20GB on-exceed=warn
runner-shared       /              3.3 GB    38% (48G)  max-age=72h
tart-oci            /Volumes/…     142 GB    58% (540G) budget=150GB
```

`--json` 供程式取用。同時在 health endpoint 的 `/healthz` 加入 `disk` 區塊(各檔案系統的可用比例與守門員狀態),讓遠端也能看到——目前遠端唯一能知道磁碟狀況的方式是登入該主機。

### G. shared volume 退出時不再刪除(行為變更)

目前 `run()` 結束時會呼叫 `CleanupSharedDocker` 移除 shared volume(`cmd/runner/main.go` 約 440–453 行)。改為不刪除,回收完全交由 TTL 與守門員。

理由:

1. **會弄壞進行中的 workflow**。realmkit 的 build job 寫入 `$SHARED_DIR`,後續 job 讀取;runner 在兩者之間重啟(升級、當機、手動重啟)會使 deploy job 失敗,而錯誤訊息指向 workflow 而非 runner。
2. **與其他所有 store 的生命週期不一致**,是「哪個機制在退出時會刪東西」這類記憶負擔的來源。
3. 退出時刪除**不是**任何已知情境真正想要的行為。

現行的 TTL 實作以 `find -mtime` 為之,粒度為整天且下限 1 天(`internal/backend/docker.go` 約 544–547 行)。這意外地保護了交接區——只要 workflow 不超過 24 小時就不會被掃到。但這是巧合:該處註解的意圖是「讓小於一天的 TTL 仍會掃」,而非保護 in-flight 資料。spec 明訂此為**設計約束**:shared volume 的 `max-age` 下限必須大於最長 workflow 執行時間,並在程式碼註解與文件中寫明原因,避免日後有人「改進」粒度時引入資料遺失。

## 錯誤處理

| 情境 | 行為 |
| --- | --- |
| `statfs` 失敗(路徑不存在、權限不足) | 記錄警告,跳過該檔案系統,不阻擋 job 啟動 |
| 某個 store 的 `Measure()` 失敗 | 記錄警告,該 store 視為未知大小、排在回收順序最後,其餘照常 |
| 某個 store 的 `Reclaim()` 失敗 | 記錄警告,繼續下一個 store 與下一階(不中止整輪) |
| 走完 `max-tier` 仍未達 `target-free` | 警告,說明差額與提高階層可再回收的量 |
| `min-free` 大於等於 `target-free` | 設定驗證失敗(`runner validate` 拒絕),否則會造成每次檢查都觸發回收 |
| 百分比或容量字串無法解析 | 設定驗證失敗,錯誤訊息含合法格式範例 |
| 啟動 runner 前的同步回收超時 | 以 timeout 包住(比照 `cleanupSharedDockerTimeout` 的做法);超時則警告後照常啟動 job,不因回收失敗而拒絕服務 |

## 測試

- **`internal/diskguard`**:門檻判斷(百分比與絕對值、邊界值);per-filesystem 分組(以 fake `Path()` 分屬不同裝置);逐階回收在達標時**提前停止**(不多回收);走完階層仍不足時的警告;`min-free >= target-free` 的驗證。
- **階層由 kind 推導**:針對每個 `StoreKind` 斷言其可被回收的階層;特別釘住 **`KindScratch` 未過期部分在任何階層都不被回收**——這是防止資料遺失的關鍵不變量。
- **`internal/cachestore`**:各 store 實作的 `Measure` 與 `Reclaim`,沿用現有的 `mockDocker` 與 `mockCommandRunner`。
- **便宜路徑**:斷言啟動 runner 前的檢查**不會**呼叫任何 `Measure()`(以計數用的 fake store 驗證),避免日後有人不慎把昂貴呼叫放進熱路徑。
- **設定相容**:舊 key(`cache-space-budget`、`shared-volume-ttl`)仍生效且不產生 unknown-key 警告;新舊並存時新 key 勝出並警告;三台主機的現行設定原樣載入無警告(以實際設定內容為測試案例,token 以佔位符替換)。
- **守門員尊重停用開關**:`prune = false` 時守門員不對該 daemon 執行 prune,且警告中說明原因。
- **budget 獨立於磁碟壓力**:磁碟充裕時,超過 `budget` 且 `on-exceed = "wipe"` 的 volume 仍會被清空。
- **cache volume 完整式**:簡式與完整式混用;`on-exceed = "wipe"` 觸發整體清空、`"warn"` 不動資料。
- **`runner cache`**:表格與 `--json` 輸出;某 store 量測失敗時仍列出其餘。

## 受影響檔案

| 檔案 | 變更 |
| --- | --- |
| `internal/cachestore/store.go` | 新增:介面、`StoreKind`、`Tier` |
| `internal/cachestore/docker.go` | 新增:docker daemon / buildx / shared volume / cache volume 的 store 實作 |
| `internal/cachestore/tart.go` | 新增:Tart OCI 快取的 store 實作 |
| `internal/diskguard/guard.go` | 新增:門檻判斷、per-filesystem 分組、階層回收迴圈 |
| `internal/diskguard/fs_unix.go` | 新增:`statfs` 封裝 |
| `cmd/runner/cmd_cache.go` | 新增:`runner cache` 指令 |
| `cmd/runner/main.go` | 建構 store 集合;啟動守門員;移除 shared volume 的退出刪除 |
| `internal/config/config.go` | 新增 `DiskConfig`;`[[docker.cache]]` 完整式;alias 處理 |
| `internal/config/load.go` | 舊 key → 新 key 的 alias 映射 |
| `internal/health/health.go` | `/healthz` 加入 `disk` 區塊 |
| `internal/scaler/scaler.go` | `startRunner` 前的便宜檢查掛載點 |
| `config.example.toml`、`README.md` | 記錄 `[disk]`、`runner cache`、分類與階層 |

## 推出順序建議

1. `cachestore` 抽象 + 各 store 實作(純重構,行為不變,既有測試須全綠)
2. `diskguard` + 週期檢查(此時已可保護磁碟)
3. `runner cache` + health 的 disk 區塊(可見性)
4. 詞彙 alias + cache volume 完整式
5. 啟動 runner 前的便宜檢查
6. shared volume 退出不再刪除(行為變更,獨立一個 commit 以便單獨回退)

前三步已涵蓋主要價值;4–6 可視情況分批發布。
