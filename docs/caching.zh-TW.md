<a id="caching"></a>

# 快取

[English](caching.md) | [繁體中文](caching.zh-TW.md)

本文件為 [英文版](caching.md) 的繁體中文版本；若內容有差異，以英文版為準。

短暫存在的 runner 每次都會從冷啟動開始，除非你主動啟用持久保存，否則工作之間不會保留任何資料。三個層次會協同運作：涵蓋整部主機的**磁碟防護機制**在實際空間不足時回收空間；各儲存區自己的**保留設定**負責例行清理；`runner cache` 則讓你**掌握**目前有哪些資料正在占用空間。

<a id="what-can-be-reclaimed"></a>

## 哪些資料可以回收

主機上所有可回收的資料，依照消失後會發生的情況，分為以下三種類型；磁碟防護機制的分層回收階梯就是以此分類為基礎：

| 類型 | 消失後的影響 | 範例 |
| --- | --- | --- |
| **垃圾資料** | 沒有影響，已經沒有任何參照 | 懸空映像檔、已停止的容器、孤立的 buildx 建置器、已過期的共用磁碟區檔案 |
| **快取** | 下次建置或工作稍微變慢，但不會出錯 | Docker 層快取／建置快取、快取磁碟區、Tart 的 OCI/IPSW 映像檔快取 |
| **暫存資料** | 進行中的工作流程執行會失敗 | 共用磁碟區內尚未過期的內容 |

這項區分可避免把建置快取與工作之間傳遞的資料混為一談：只要快取妨礙可用空間，就可以回收；暫存資料在任何層級都不能回收，無論磁碟空間多麼吃緊。共用磁碟區是唯一會隨資料存放時間而兼具兩種類型的儲存區：工作流程進行期間，它是暫存資料（同一次執行中，後續工作可能仍會讀取先前工作寫入的內容）；檔案超過 `shared-volume-max-age` 後，就變成垃圾資料。因此，runner 不再於程序結束時刪除共用磁碟區：同一次工作流程執行的兩個工作之間，即使重新啟動（包括自行更新），也不再導致後續工作失敗；回收完全交由 `shared-volume-max-age` 與防護機制的第 3 層處理。

<a id="the-disk-guard"></a>

## 磁碟防護機制

`[disk]` 會執行防護機制，依各儲存區實際所在的檔案系統分組，監控所有已設定的儲存區，並且只在空間確實吃緊時回收；它不是不論有無需要，都按照固定計時器清理的工具：

```toml
[disk]
guard       = true   # reclaim under disk pressure; false opts this host's disk out of guard management entirely
min-free    = "10%"  # free-space floor that triggers reclamation: a percentage of the filesystem, or a size like "20GB"
target-free = "20%"  # level reclamation aims to restore before stopping; must be greater than min-free
interval    = "1h"   # how often the periodic check runs
max-tier    = 3      # highest tier the guard may reclaim at (1-4)
```

**以檔案系統為單位，而不是主機。** 如果 Mac Studio 的 `TART_HOME` 位於獨立的 931 GB 磁碟區，而 Docker 位於系統磁碟，兩者就會各自檢查、各自套用門檻；在這種情況下，涵蓋整部主機的單一「磁碟已滿」數值並沒有意義。

當檔案系統的可用空間低於 `min-free`，防護機制會依序提高回收層級，每完成一層便重新檢查可用空間，達到 `target-free` 就立即停止：

| 層級 | 回收內容 | 類型 |
| --- | --- | --- |
| 1 | 懸空映像檔、已停止的容器、孤立的 buildx 建置器 | 垃圾資料 |
| 2 | 超過最長存放時間的建置快取與映像檔層 | 快取 |
| 3 | 超過最長存放時間的共用磁碟區檔案 | 暫存資料（僅限已過期部分） |
| 4 | 完整清空快取磁碟區 | 快取 |

`max-tier` 預設為 **3**：會自動回收垃圾資料並依存放時間清理，但清空快取磁碟區（第 4 層）會讓下次建置變成冷啟動，因此必須明確啟用。

防護機制也會在每次清理時，強制執行各儲存區自身設定的容量上限（見下文），無論該儲存區所在的檔案系統是否空間吃緊；此外，每次啟動 runner 前，會立即檢查可用空間，只執行一次低成本的 `statfs`，而非完整清理。如果該檔案系統已低於 `min-free`，會先同步回收空間。這能避免工作在建置途中填滿磁碟，拖垮整部主機；此時回收失敗只會記錄日誌，不會造成致命錯誤，因為拒絕啟動工作比磁碟已滿更糟。

<a id="store-retention-settings"></a>

## 儲存區保留設定

各儲存區也各自保有例行、定時清理的設定；這些機制在磁碟防護機制出現前就已存在，現在仍會依自己的排程執行，不受磁碟空間壓力影響。

**Docker 層快取／建置快取（自動）。** 設定 `dind = true` 時，工作會直接與主機的常駐程式通訊，因此 `docker pull`／`docker build` 的層快取由所有工作共用，重新啟動後仍會保留。在 runner 專用的常駐程式上，可以主動啟用執行期間的 prune 清理來限制容量成長；關於 `prune`、`prune-ttl`、`build-cache-max-age`、`build-cache-budget`，請參閱上方的[設定檔（TOML）](configuration.zh-TW.md#config-file-toml)。

> **BuildKit 提示：** `docker/setup-buildx-action` 會為每個工作建立用完即丟的建置器，其快取會隨工作結束而消失（`buildx-cleanup` 會回收殘留資料）。若要真正跨工作重複使用建置快取，請使用常駐程式內建的 BuildKit 建置（單純使用 `docker build`，或搭配 `driver: docker` 的 buildx），讓快取存放在 `build-cache-*` 保留設定管理的位置。

**快取磁碟區（`cache-volumes`、`[[docker.cache]]`）。** 具名磁碟區會掛載至每個 runner 容器中工具的預設快取路徑，工作流程無須任何修改即可使用已備妥的快取：

```toml
[docker]
cache-volumes = [
  "gradle-cache:/home/runner/.gradle",
  "pnpm-store:/home/runner/.local/share/pnpm/store",
]

# Long form, for an entry that needs a size budget. A name in both forms
# resolves entirely from its [[docker.cache]] entry, not merged field by field.
[[docker.cache]]
name      = "ccache"
path      = "/home/runner/.ccache"
budget    = "20GB"
on-exceed = "warn"   # warn (default): log only, leave eviction to the tool's own LRU | wipe: delete everything over budget
```

簡短格式已足以應付大多數快取：首次使用時建立、持久保存，且永遠不會因任何 TTL 而被清理。不同 runner 規模集使用相同磁碟區名稱即可共用快取；使用不同名稱則可隔離。完整格式另外提供選填的 `budget`，其限制不受磁碟空間壓力影響；防護機制每次清理都會檢查，無論 `max-tier` 為何，或檔案系統是否真的空間吃緊。這裡不會局部淘汰資料：一旦超出容量上限，`wipe` 就會刪除磁碟區的全部內容。細緻的 LRU 淘汰刻意交由工具本身處理（例如 ccache 自己的 `max_size`）；無論是以內容定址的 pnpm 儲存區，還是 Gradle 的 `modules-2` 中繼資料索引，都可能因通用的逐檔清理而損壞。

**共用磁碟區（`shared-volume`、`shared-volume-max-age`）。** 這是掛載至每個 runner 相同路徑的一般用途磁碟區（透過 `$SHARED_DIR` 提供），供工作流程明確在同一次執行的工作之間傳遞檔案：

```toml
[docker]
shared-volume                  = "/shared"
shared-volume-max-age          = "168h"  # delete files older than this
shared-volume-cleanup-interval = "6h"
```

檔案只有在超過 `shared-volume-max-age` 後才會刪除；絕不會在程序結束時刪除，也絕不會在期限內刪除，無論磁碟空間多麼吃緊（即上方的暫存資料分類，防護機制的第 3 層也會強制遵守）。請將此值設為比最長工作流程執行時間更長。

`shared-volume-max-age` 是此磁碟區*唯一*的保留策略，因此不設定它就表示永遠不會有任何機制回收此磁碟區：程序結束時不會刪除，而防護機制在任何層級都不會碰觸尚未過期的暫存資料。runner 仍會測量其大小（會出現在 `runner cache` 中），並在啟動時警告，但沒有設定最長存放時間的共用磁碟區會無限制成長。

**Tart OCI/IPSW 快取（`cache-cleanup`、`cache-max-age`、`cache-budget`）。** 每個 Xcode VM 映像檔大小為 50–80 GB，而 `:latest` 標籤會讓舊層累積在 `$TART_HOME/cache/` 下；完整的 `[tart]` 範例請參閱上方的[設定檔（TOML）](configuration.zh-TW.md#config-file-toml)。清理程序只會處理 OCI/IPSW 快取，絕不會碰觸你的本機 VM。

**啟用開關只控制例行清理。** `prune`（涵蓋懸空映像檔／已停止容器儲存區與建置快取儲存區）、`buildx-cleanup` 和 `cache-cleanup` 各自開啟或關閉該儲存區自己的*週期性、定時*清理，但不會讓儲存區免受磁碟防護機制管理。`prune` 與 `buildx-cleanup` 預設關閉，因為共用常駐程式可能執行不屬於 runner 的工作負載；但實際磁碟空間吃緊時，防護機制仍會回收這些儲存區，不受開關影響。這是必要的：`prune` 與 `buildx-cleanup` 都預設關閉，而 `max-tier` 預設為 3；如果防護機制也遵守這些開關，那麼在只使用 Docker 的主機上，它雖然預設啟用，卻完全無法回收任何資料，直到管理者另外明確啟用例行清理為止。如果某顆磁碟確實不該由 runner 管理，例如常駐程式與無關的工作負載共用，正確的開關是 `[disk] guard = false`，可將該主機的磁碟完全排除於防護機制的管理之外。個別儲存區的開關從來就不是為了表達這個意思而設計的。

<a id="visibility"></a>

## 檢視空間使用情況

`runner cache` 會測量每個已設定的儲存區，並列出其大小、所在的檔案系統、該檔案系統目前的可用空間百分比，以及保留策略：

```
$ runner cache
STORE               FILESYSTEM  SIZE     FS FREE  POLICY
docker-build-cache  /           12.4 GB  38%
ccache              /           18.1 GB  38%      budget=20.0 GiB on-exceed=warn
runner-shared       /           3.3 GB   38%
tart-cache          /Volumes/…  142 GB   58%
```

加上 `--json` 即可供指令碼使用。測量可能耗費較多資源，部分儲存區會透過短暫執行的輔助容器遍歷磁碟區，因此絕不會在頻繁執行的關鍵路徑上進行。若需要可安全輪詢的低成本資訊，`/healthz` 的 `disk` 區段會以單純的 `statfs` 回報各檔案系統的可用空間百分比，絕不會實際測量儲存區大小。
