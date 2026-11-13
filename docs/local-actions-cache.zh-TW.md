# 主機本機 Actions 快取

Runscaler 在 Linux 主機上為短生命週期 Docker runner 提供兩種選用的
cache-v2 接收器模式：

- **原生 Go 接收器：** `runner` 管理程序自行啟動私有 HTTP listener，並將快取資料
  存於 `[actions-cache].data-dir`。先啟用全域接收器，再對選定的 Docker 規模集設定
  `native-actions-cache = true`；不需要另一個快取服務程序或容器。詳見下方
  [原生接收器的設定與回復](#原生-go-接收器)。
- **外部接收器：** 另行管理服務，例如下方的 Rust／Compose 部署，再對選定規模集
  設定明確的 `docker.actions-cache-url`。服務程序、資料及管理權限均與 runner 分開。

已驗收的 cache-v2 client 維持標準 Actions cache key 與還原行為。兩種模式都需要
壓縮、解壓及本機 HTTP 傳輸，並非零複製快取。

[外部 Rust 服務相容性報告](local-actions-cache-compatibility.md)記錄原先釘選的
client 與 runtime gate。原生 Go 接收器已通過真實簽名工作驗收，涵蓋失效保護
runner 映像、client、重啟與斷線結果；詳細執行證據保存在私有操作紀錄。兩種模式
在每台新主機都需驗證私有端點、`actions-runner` 映像及實際工作流程。統一映像
同時支援官方與本機快取，由 scale set 設定選擇端點。

## 外部接收器：資源歸屬與資料保存

`runscaler-actions-cache` 由操作者獨立管理，不隨每個 job 建立或刪除。
它使用 `managed-by=runscaler-cache` label，不可設定為 `managed-by=runner`，
也不可加入 `runscaler.runner-id`。外部 named volume
`runscaler-actions-cache-data` 將 SQLite 與檔案 payload 一起保存在 `/app/.data`。

不可掛入 job，也不可登記在 `cache-volumes` 或 `[[docker.cache]]`；這些儲存區
有各自的回收／清空規則。讓 server 自行管理：閒置 30 天、明確的絕對容量預算。
固定版本中，絕對容量預算會優先於檔案系統百分比設定，兩者不會同時套用。
清理門檻不是硬性磁碟配額，暫存上傳可能超過預算；主機必須保留空間，
需要硬性上限時應使用獨立檔案系統或 quota。不要分開刪 SQLite 或 payload 檔案。

已停止的服務容器可能被 stopped-container prune 移除；資料保存承諾在外部 volume，
Compose 應能使用同一份 volume 重建服務。一般 runner 清理不應刪除此 volume。

## 外部接收器：端點與容量

將 `deploy/local-cache/.env.example` 複製為同目錄下 Git 忽略的 `.env`，填好所有必填欄位：

- `CACHE_BIND_IP`：主機的私有介面 IP，不可使用 `0.0.0.0` 或 `::`。
- `CACHE_PORT`：主機連接埠，預設 3000。
- `CACHE_ADVERTISE_URL`：有結尾斜線的完整 HTTP(S) URL，必須能從實際 runner
  及旁系 BuildKit 容器的網路命名空間連線。
- `CACHE_MAX_SIZE_BYTES`：根據主機剩餘磁碟空間決定的正整數 byte 容量。
- `CACHE_MANAGEMENT_KEY`：私有管理憑證，只提供給服務，不提供給 job。

Linux 容器不一定能透過 `localhost`、Compose DNS 名稱或 `host.docker.internal`
找到主機。本次 pilot 在隔離環境使用經過實測的私有 bridge 位址；正式環境要自行
驗證路由。Compose 要求明確輸入，但不會驗證 IP 是否私有，也不會自動調整防火牆。
容器內部會監聽所有介面，主機端只發布到明確選定的私有介面。

## 外部接收器：啟動程序

先檢閱 canary 證據，確認實際主機、映像、端點與容量後才部署。已存在且歸屬不明的
同名 volume 不可直接覆寫；若確認是新 volume，明確建立：

```sh
docker volume create --label managed-by=runscaler-cache runscaler-actions-cache-data
```

準備私有設定，使用不列印解析後憑證的方式驗證，再獨立啟動服務：

```sh
cp deploy/local-cache/.env.example deploy/local-cache/.env
# 先在 Git 忽略的 .env 填妥所有必填輸入。
docker compose --env-file deploy/local-cache/.env -f deploy/local-cache/compose.yaml config --quiet
docker compose --env-file deploy/local-cache/.env -f deploy/local-cache/compose.yaml up -d
```

保留 token 驗證，不可在實際服務設定 `SKIP_TOKEN_VALIDATION`。測試 fixture 的
受控 issuer 只能用於隔離驗證。服務設定與範例不應包含 GitHub runtime token
或 runner 註冊憑證。

使用外部 HTTP client，分別從實際 runner 及共用實際 BuildKit 網路命名空間的 client
探測 `/health`。上游 distroless 映像沒有可假設存在的 shell／curl。
主機自己能連通還不夠，兩條路徑也都要實際上傳、下載 payload 並核對 checksum。

## Runner 選用與映像

使用原生接收器時，只在選定的 Docker scale set 設定 `native-actions-cache = true`，
並設定全域 `[actions-cache].advertise-url`。使用已驗證的統一 `actions-runner`
映像；canary 驗證時固定 digest。詳見[設定](configuration.zh-TW.md)與
[映像建置及維護](../images/actions-runner/README.md#build-and-verification)。
單純 fork base image 不會提供已安裝的 capability checker。

Runscaler 先完成 volume 歸屬設定，再執行
`/usr/local/bin/runner-local-cache-check`，成功後才啟動 listener。
檢查會比對 `/usr/local/share/runscaler/` 下 root 擁有的 manifest，以及已驗收的
protocol、source／version、Worker／Listener checksum；缺檔或被修改就不接收工作。
這個檢查不發出網路請求，服務可用性另行驗證。
Docker provider 對選定的接收器提供 `CUSTOM_ACTIONS_RESULTS_URL`。外部服務的管理
憑證不會注入 job；原生模式沒有獨立的管理金鑰。設定空白時保留原啟動行為。

## Client 使用範圍

僅啟用已驗收的 client 與固定 revision。舊的 v1 client 不受支援。原本已驗收的
映像在未選用本機端點時保留 GitHub 的 `ACTIONS_CACHE_URL`；選用本機快取時會將它指向
本機 receiver，由本機拒絕 v1 請求，不回退到雲端。直接執行 shell 工具時需要明確匯出 runtime
環境；container handler 未設定 v2 flag 時，也需明確啟用 v2。
已驗收的 setup-go、setup-node/npm、Gradle basic 與 Rust GitHub provider revision
列於證據報告；pnpm 與其他 provider 尚未驗證。Rust 還原 crate 後仍可能取得 registry
metadata，並非完整離線 Cargo 支援。工具鏈及套件來源下載與快取 payload 傳輸分別計算。

BuildKit 使用 `type=gha,version=2`，每個 image／platform 分別設 scope。
本機端點不會自動導向既有 registry／S3 cache，也不會自動保存
`RUN --mount=type=cache` 的可變快取內容。Canary 先停用重疊的共用可寫語言快取掛載，
保留原資料。

Artifact 仍儲存在 GitHub，經過本機 Results 端點轉送；服務故障因此也可能影響
artifact action。不可保證所有 cache／action 錯誤都不會造成 job 失敗，也不可自動
將 cache payload 改存 GitHub。實際六種 client 斷線驗證中，五種 Node cache helper
發出警告後繼續建置，artifact action 則失敗。Docker 選擇性匯出需要
`ignore-error=true`；測試停用匯入以單獨驗證匯出，並確認錯誤 Dockerfile 仍會失敗。
這不代表任意 cache import 或其他 client 都能容忍斷線；沒有自動雲端 fallback。

## 外部接收器：下載 capability、備份與回復

Filesystem 模式回傳的 `/download/<UUIDv4>` 是 bearer capability，沒有獨立到期時間；
取得該 URL 者可在資料刪除前下載。不可列印或分享 payload URL。Scoped lookup 的
驗證不能撤銷已洩漏的 URL；正式部署前應檢閱此特性。

先 drain／暫停受影響的本機快取工作，再停止服務，整份備份資料 volume，確保 SQLite
與 payload 一致。備份應放在 repository 外並限制存取；還原時用相容的釘選映像，
同時還原兩者。

v10 是單向遷移：比 v9.10 更舊的資料庫會被刪除並清空 storage；v9.10 會被遷移。
不可直接對已遷移 volume 降版，應使用相容的完整快照，或另一份新 volume。

## Provider 管理的 canary 與回復

變更現有 manager 前，先保存 binary、私有設定與服務定義，確認目前版本實際的關閉
行為，並安排維護時段。Manager 使用單一主機鎖，不支援熱載入或各 scale set 的
`enabled` 開關；`max-runners=0` 無效。不可另啟第二個 manager 當作 canary 隔離。
新增／移除 scale set 都需重啟 manager，會短暫中斷它管理的所有 listener；
保留其他 scale set 的設定值。

先停止派發 canary 工作並排空現有工作，再替換檔案。舊版 manager 的 drain 行為
若未確認，應先安排所有現有工作完成；換新 binary 不會改變仍在執行的舊程序。
停止前檢查並調整服務的停止期限，使其涵蓋已審閱的 drain 額度：舊 systemd 的
90 秒上限無法容納新版預設兩小時 drain。詳見[服務期限處理](deployment.zh-TW.md#graceful-shutdown)。
不可為了部署而強制終止業務 job。

獲得明確部署核准後，使用已驗證 digest 的失效保護映像。在另一個私有連接埠
與全新私有資料目錄啟動原生 Go listener。若從既有 Rust 部署遷移，保留其服務
與資料供回復使用。新增只帶獨立 label、最多一個 runner 的 canary scale set，
避免一般組織工作被派入。
在 provider 管理的 runner 上驗證簽名保存／還原、artifact 轉送及旁系 BuildKit
上傳／下載後，再排空並切換選定的現有 scale set。停用重疊的共用可寫語言快取
掛載並保留其資料。正式容量與磁碟效能仍需另行觀察。

回復時停止派發 canary 工作並排空。若從既有外部接收器遷移，在 manager 停止期間
還原已審閱的舊 binary／設定與相容服務定義，包括選定 scale set 的外部端點與舊 runner
映像。保留每份既有快取資料與私有備份；一般回復不刪除 GitHub scale-set 註冊。
隔離 JIT 測試不代表已演練正式 manager 重啟。

直接清空端點後恢復原 cache action，會重新將 payload 傳到 GitHub。除非操作者
明確選擇已審閱、略過所有相關 cache 操作的 workflow，否則維持 canary 工作暫停。
本次未驗證適用於所有 helper 的停用配方，因此不提供自動復跑路徑。

## 原生 Go 接收器

可選用的原生接收器位於 manager 程序內，使用獨立私有連接埠與全新資料目錄。
它根據已驗證 token 的 repository／scope 授權，完成後的 key 不可覆蓋，
依序提交 Azure block，並發出有期限的本機 payload URL。SQLite 與私有檔案
一起保存資料；非 cache 的 Results 請求仍轉送至固定 GitHub 上游供 artifact
使用。正常 drain 期間 listener 保持運作。備份或還原時，須先停止 manager，
並把整個原生目錄中的資料庫、WAL、物件及 capability key 一起處理。
`runner status` 回報就緒狀態與用量；`runner cache` 唯讀查詢已完成的項目數與容量。

本機協定驗證需安裝 Node.js，然後執行：

```sh
RUNSCALER_NATIVE_FIXTURE=1 go test -race ./internal/actionscache -run '^TestNativeProtocolFixture$' -count=1
```

此測試只在 loopback 啟動一次性的 issuer 與 cache HTTP listener，檢查授權後的
分塊上傳、逐位元組還原、取消、重新開啟 store，以及受控的 artifact 轉送。
它不註冊 runner，也不證明真實 GitHub 簽名 job 的相容性。

若從既有外部部署遷移，先保留其服務與資料作為回復端點，資料保留期限另行決定；
全新的原生部署不需要 Rust 服務。原本 runner 映像對舊版 v1 client
仍提供 GitHub 的 `ACTIONS_CACHE_URL`；原生 v2 應使用已驗證的失效保護映像，
讓舊版請求在本機遭拒。切換前應檢查私有的真實簽名工作驗收紀錄；本機協定測試
不能取代真實簽名 client 驗收。
