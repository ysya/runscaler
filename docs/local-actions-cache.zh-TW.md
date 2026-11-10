# 主機本機 Actions 快取

在同一台 Linux 主機上，讓常駐快取服務與可拋棄的 Docker runner 分別運作。
已驗收的 cache-v2 client 保留既有 workflow 步驟、cache key 與 restore-key 比對，
資料留在本機，仍然需要壓縮、解壓及本機 HTTP 傳輸。此 server 允許替換已完成的
同 key 快取，詳見下方「快取語意」。

目前相容性 gate 涵蓋 Linux amd64、釘選的 server／runner、actions/cache v6.1.0、
setup-go、setup-node/npm、Gradle basic、Rust GitHub provider、artifact 轉送及
BuildKit v2 匯出／匯入。請閱讀[固定 revision、三組量測與限制](local-actions-cache-compatibility.md)。
隔離環境真實 job、provider guard 與完整工具鏈映像建置已通過。
Linux amd64 完整映像已隨 v0.17.0 發布；每台主機擴大使用前，仍須完成自己的
provider canary、路由與儲存驗證。Arm64 與其他 client 尚未驗證；標準 runner
映像維持原行為。
另已完成 provider 管理的單一 runner 主機驗收：六種冷／暖 client 與 artifact
傳輸通過，之後將原 scale set 切換並保留原本五個 runner 的容量。日期與範圍
見相容性報告；這不代表所有主機或五個工作同時執行都已驗證。

## 元件與 workflow 調整範圍

快取服務是第三方開源專案
[falcondev-oss/github-actions-cache-server](https://github.com/falcondev-oss/github-actions-cache-server)，
以 Rust 撰寫，並非 GitHub 官方營運的服務。本部署使用 filesystem 與 SQLite，
共同保存在持久 volume。選用的 runner 映像使用已驗收的 GitHub runner fork，
讓 cache-v2 client 連到此服務。Artifact 仍存放 GitHub，但會經過此服務轉送，
因此 artifact 操作也需要服務保持可用。

對已使用 Actions cache v2 且通過驗收的 client，可集中修改 Runscaler 設定，
保留各專案既有的 cache action。此功能不會替沒有 cache 步驟的 workflow 新增
快取、不會改寫 registry／S3 backend，也不代表所有第三方 helper 都已相容。

使用 Runscaler v0.17.0 或更新版本，以及獨立發布的快取映像：

```text
ghcr.io/ysya/actions-runner-local-cache@sha256:ce94d981fbd73f15788bf98af0055d8ccea29a50932484e50c962c67cd45d577
```

一般 `actions-runner` 與 `actions-runner-local-cache` 套件共用 Dockerfile，
但 runner 來源與支援架構不同。版本／source tag 用來辨識建置，部署時固定已審閱的
不可變 digest。目前快取 workflow 發布 source／run／attempt tag，尚未發布
`latest`、`stable-amd64` 或 release 版本別名。

## 資源歸屬與資料保存

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

## 選擇端點與容量

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

## 操作者啟動程序

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

## Opt-in runner 啟動

只在選定的 Docker scale set 設定 `actions-cache-url`，並使用通過驗證、固定 digest 的
快取專用變體。詳見[設定](configuration.zh-TW.md)與
[映像建置及維護](../images/actions-runner/README.md#opt-in-host-local-actions-cache-variant)。
單純 fork base image 或標準映像不會提供已安裝的 capability checker。

Runscaler 先完成 volume 歸屬設定，再執行
`/usr/local/bin/runner-local-cache-check`，成功後才啟動 listener。
檢查會比對 `/usr/local/share/runscaler/` 下 root 擁有的 manifest，以及已驗收的
protocol、source／version、Worker／Listener checksum；缺檔或被修改就不接收工作。
這個檢查不發出網路請求，服務可用性另行驗證。
本機模式只新增 `CUSTOM_ACTIONS_RESULTS_URL`；管理憑證留在服務端。
設定空白時保留原啟動行為。

## Client 使用範圍

僅啟用已驗收的 client 與固定 revision。第一版不支援舊的 v1 client；fork 仍然會
提供 GitHub 的 `ACTIONS_CACHE_URL`。直接執行 shell 工具時需要明確匯出 runtime
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

## 快取語意

固定版本的 v10 server 會在上傳期間保留 key／version／repository／scope 的組合，
拒絕競爭中的相同保留請求。上傳完成後，後續上傳仍可替換同一組合的既有資料。
完成上傳後，會透過資料庫交易更新已發布快取資料的指向；這與 GitHub 的
[不可覆寫 cache key 語意](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching)
不同。多筆同 key save 成功不代表 payload 已混合，也不能假設第一個寫入者永遠勝出。

隔離測試以不同 payload 驗證了替換期間成功讀取的完整性，但不保證所有時序，
也不保證替換後舊 download capability 仍有效：先前解析出的 URL 可能回傳 404。
Cache key 應區分工具鏈、lockfile、平台與相容工作；其他 job 必須取得的輸出請使用
artifact。快取屬於可回收的建置資料，不是必要資料的交接儲存區。

## 下載 capability、備份與回復

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

獲得明確部署核准後，選用已發布的 amd64 映像並驗證 digest／attestation，啟動
獨立快取服務，加入獨立 label、最多一個 runner 的 canary entry。停用重疊的共用
可寫語言快取掛載並保留其資料。擴大前，實測 runner 與旁系 BuildKit 的上傳／下載、
服務重建後全新 runner 的還原及總耗時。隔離環境已通過兩個並行 job，正式容量仍需確認。

回復時停止派發 canary 工作並排空。在 manager 停止期間，只移除 canary 設定 entry，
還原已審閱的舊 binary／設定與相容服務定義，再以原值重啟其他 entry。保留外部
資料 volume 與私有備份；一般回復不刪除 GitHub scale-set 註冊。隔離 JIT 測試
不代表已演練正式 manager 重啟。

直接清空端點後恢復原 cache action，會重新將 payload 傳到 GitHub。除非操作者
明確選擇已審閱、略過所有相關 cache 操作的 workflow，否則維持 canary 工作暫停。
本次未驗證適用於所有 helper 的停用配方，因此不提供自動復跑路徑。
