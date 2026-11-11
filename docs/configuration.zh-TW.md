<a id="configuration"></a>

# 設定

[English](configuration.md) | [繁體中文](configuration.zh-TW.md)

本文件為 [英文版](configuration.md) 的繁體中文版本；若內容有差異，以英文版為準。

可透過 TOML 設定檔（`--config`）或 CLI 旗標提供設定。兩者同時提供時，CLI 旗標的優先順序高於設定檔中的值。

未知的設定鍵（例如拼字錯誤或其他版本的選項）不會被無聲忽略：`runner run` 會逐一記錄警告並繼續啟動，`runner validate` 則會判定失敗。若已有 `[[scaleset]]` 項目，卻仍將單一模式的設定鍵（`url`、`name`、`token`、`github-app`、`labels`、`min-runners`）留在最上層，也會採取相同處理。

<a id="config-file-toml"></a>

## 設定檔（TOML）

**Docker 供應器（預設）：**

```toml
# config.toml
url = "https://github.com/your-org"
name = "my-runners"
token = "ghp_xxx"
max-runners = 10
min-runners = 0
labels = ["self-hosted", "linux"]
runner-image = "ghcr.io/actions/actions-runner:latest"
runner-group = "default"
log-level = "info"
log-format = "text"
concurrent = 10                          # host-wide across all scale sets; 0 = automatic sum
# log-file = "/var/log/runner/runner.log" # default: /var/log/runner/runner.log (root), ~/.local/state/runner/runner.log (Linux), ~/Library/Logs/runner/runner.log (macOS); "" disables
health-address = "127.0.0.1"              # localhost-only by default
health-port = 8080
# drain-timeout = "2h"                    # 0s disables graceful drain

[docker]
socket = "/var/run/docker.sock"
dind = true
# workspace-root = "/home/runscaler/runner-workspaces" # Linux：Docker workspace bind 專用主機目錄
shared-volume = "/shared"
# shared-volume-max-age = "168h"        # delete shared-volume files older than this; unset = never reclaimed
# buildx-cleanup = true                 # opt in only on a runner-dedicated daemon
# buildx-cleanup-ttl = "24h"            # remove buildx builders older than this
# buildx-cleanup-interval = "6h"        # how often the buildx sweep runs
# prune = true                          # opt in only on a runner-dedicated daemon
# prune-interval = "6h"                 # how often the runtime prune sweep runs
# prune-ttl = "24h"                     # remove stopped containers and dangling images older than this
# build-cache-max-age = "168h"          # prune build cache entries not used within this window
# build-cache-budget = 0                # cap daemon build cache to N GB (0 = no cap)
```

在使用主機 Docker socket 的 Linux 主機上，container action 的 workspace bind
原本會由主機解析，無法讀到 runner 容器內的檔案。若要支援這類 action，先建立由管理程式
使用者擁有、權限為 `0700` 的 `workspace-root` 目錄，再於適用的 `[docker]` 或
`[scaleset.docker]` 設定。管理程式會為每個 runner 建立獨立的主機工作目錄與 Unix
socket 代理；代理只轉換 `/home/runner/_work` 下的 Docker 容器 bind 來源，runner
內的路徑仍固定，讓快取用戶端維持相同的版本鍵。runner 的容器停止後才會移除其工作目錄；
管理程式重啟時也會檢查存活的 Docker 掛載，再清理上次異常結束留下的目錄。此選項需
搭配 `dind = true`。

當 runner 使用 `docker buildx` 建置映像檔（例如透過 `docker/setup-buildx-action`）時，每次執行都可能留下 BuildKit builder 容器，以及容量達數 GB 的 `buildx_buildkit_*_state` 磁碟區。在持續運作、共用同一個 Docker daemon 的主機上，這些資源會不斷累積，直到磁碟空間耗盡。若 daemon 專供 runner 使用，可設定 `buildx-cleanup = true` 啟用清理；runner 接著會移除建立時間超過 `buildx-cleanup-ttl` 的 builder。若持續保留的 builder 或其他工作負載也共用該 daemon，請勿啟用。

除了 buildx builder，每次建置時，掛載主機 Docker socket 的工作也會留下懸空映像檔、已停止的容器與 daemon 建置快取。執行期間的 prune 清理作業（透過 `prune = true` 啟用）會在 runner 運作時定期回收這些資源：建立時間超過 `prune-ttl` 的已停止容器與懸空映像檔、在 `build-cache-max-age` 期間內未使用的建置快取，以及設定 `build-cache-budget` 時超過 GB 上限的建置快取。此功能假設 daemon 專供 runner 使用：只要物件符合條件，無論由誰建立，都會視為工作留下的廢棄資源。在共用的 daemon 上請保持停用。

停用 `prune`／`buildx-cleanup` 只會關閉*這個* daemon 的例行清理；一旦檔案系統實際出現空間壓力，主機層級的磁碟保護機制（請見[快取](caching.zh-TW.md)）仍會透過這兩種方式回收資源，不受這些開關影響。

**Tart 供應器（macOS）：**

```toml
# config.toml
provider = "tart"
url = "https://github.com/your-org"
name = "macos-runners"
token = "ghp_xxx"
max-runners = 2          # Apple limits 2 concurrent macOS VMs per host
runner-image = "ghcr.io/cirruslabs/macos-golden-gate-xcode:latest"
labels = ["self-hosted", "macOS"]
log-level = "info"

[tart]
cpu = 4                  # CPU cores per VM (0 = use image default)
memory = 8192            # Memory in MB per VM (0 = use image default)
runner-dir = "/Users/admin/actions-runner"  # default
pool-size = 2            # pre-warm 2 VMs for instant job pickup (~2s vs ~30s cold boot)
# home = "/Volumes/Data/tart"          # TART_HOME for the tart CLI ("" = ~/.tart)
# cache-budget = 80                    # cap OCI/IPSW cache to N GB via `tart prune` (0 = disabled)
# cache-cleanup-interval = "24h"       # how often the prune sweep runs
# mtu = 0                              # guest MTU: 0 = match a VPN tunnel (default), -1 = off, 1280–1500 = fixed
# dns = ["1.1.1.1", "8.8.8.8"]         # guest resolvers ([] = keep the one Tart's NAT hands out)
# net-bridged = "en0"                  # bridge VMs to this host interface instead of Tart's NAT
```

設定 `home` 後，自己執行 tart 指令時也應指向相同目錄，例如 `TART_HOME=/Volumes/Data/tart tart list`。直接執行 `tart list` 會讀取 `~/.tart`，因此看不到 runner 的映像檔或 VM。

Xcode VM 映像檔體積龐大（每個 50–80 GB），而 `:latest` 標籤會使舊的映像層累積在 `$TART_HOME/cache/` 下；請設定 `cache-budget` 來限制其大小。清理作業只會處理 OCI/IPSW 快取，絕不會動到本機 VM。每次 prune 前，都會將每個 Tart runner 規模集的 `runner-image` 標記為剛使用過，因此即使一週沒有工作，也不會再把下次工作要複製的映像檔清除；在 `cache-budget` 限制下，該映像檔也會最後才被移除。

**透過 VPN 連線的主機（Cloudflare WARP、Tailscale 出口節點、企業 VPN）。** 若通道無法承載 1500 位元組的封包，主機本身仍可正常運作，但 VM 的連線會失效：客體系統宣告完整大小的 TCP MSS，伺服器便以 1500 位元組的封包回應，而通道會丟棄這些封包，導致 VM 的每次 TLS 交握都停滯；runner 雖然啟動，卻始終無法連線，因此工作會一直排隊。預設情況下（`mtu = 0`），runner 會在每個 runner 啟動前查詢主機通往該 runner 規模集 GitHub 主機的路由；若該路由的 MTU 小於 1500，就會將客體系統的 MTU 設為相同值（透過 Cirrus Labs 映像檔允許的 `sudo -n ifconfig`）。若通道僅承載通往部分目的地的流量，請設定固定的 `mtu`；若要讓客體系統維持原狀，請設為 `-1`。Tart 的 NAT 也會透過主機代理 DNS，而該代理可能在主機中斷 VPN 連線後停止回應；可設定 `dns`，讓客體系統使用自己的 DNS 解析伺服器。

**選擇 Tart 網路模式（v0.12.0+）。** 共用 NAT 仍是預設模式。若 VM 已能取得位址並連線至 GitHub，請保持 `net-bridged` 未設定或為空；升級不需要變更網路設定。NAT 會讓 VM 留在主機的私有網路中，而不是為每個 VM 配發區域網路上的位址。

**NAT 無法為客體系統提供服務的主機。** Tart 的共用 NAT 仰賴主機的 `bootpd` 為每個客體系統配發租約。若無法配發，客體系統啟動後會使用鏈路本地 `169.254.x.x` 位址，且沒有對外路由，runner 因而始終無法連線至 GitHub，工作也會一直排隊。將 `net-bridged` 設為主機的網路介面（`net-bridged = "en0"`，即 `ifconfig` 顯示的作用中乙太網路或 Wi-Fi 介面的 BSD 裝置名稱），每個 VM 就會以 `tart run --net-bridged=<interface>` 啟動，改由區域網路的 DHCP 伺服器取得位址。橋接會將每個 VM 直接連接至該區域網路，並不會建立私有的 runner 網路。映像檔必須讓客體系統的網路服務保持使用 DHCP。對應的 CLI 指令為 `runner run --config config.toml --tart-net-bridged en0`。在多個 runner 規模集的設定中，`[tart] net-bridged` 會被繼承；若要讓個別 runner 規模集恢復使用 NAT，請設定 `[scaleset.tart] net-bridged = ""`。

橋接的客體系統對外連線時不會經過主機的路由表，因此 `mtu = 0` 不再為其比照主機通道設定 MTU；固定的 `mtu` 與 `dns` 仍然適用。橋接 VM 的 MAC 位址是根據主機介面的硬體位址與共用主機名額推導，而不是在每台主機上重複使用相同的固定名額位址。同一個介面硬體位址與名額會維持相同位址，讓 DHCP 租約得以重複使用；變更主機介面的硬體位址也會改變 VM 位址。若無法安全地讀取或寫入橋接 MAC 位址，啟動會失敗，複製的 VM 也會被清除。IP 配發仍由 DHCP 伺服器控制：此 MAC 政策不保證固定 IP，也無法解決複製映像檔內重複的 DHCP 用戶端識別碼。在正式依賴橋接部署之前，請確認兩個同時執行的 VM 能取得不同位址，且在反覆建立與移除 VM 後仍可完成工作；若多台 Mac 主機共用該區域網路，也應納入驗證。

**Docker 網路。** 這些 Tart 設定不適用於 Docker runner。除非透過 `[docker] network` 選擇另一個既有網路，否則它們會使用 Docker 的預設橋接網路。位址配發由 Docker 管理；透過該設定即可選擇專用的 Docker 橋接網路（請見[安全性與隔離](security.zh-TW.md)）。

**Runner 啟動檢查。** 若 runner 終止或始終無法連線至 GitHub，其 VM 仍會持續執行，因此 runner 只會在 VM 內的 runner 印出 `Listening for Jobs`，且幾秒後仍然存活時，才交出該 VM；GitHub 正是在這個階段拒絕已淘汰的 runner 版本。若 runner 在此之前結束，或在 5 分鐘內仍未連線，runner 就會刪除 VM，記錄 `Failed to start runner` 與該 runner 本身的輸出（或其 `_diag` 記錄檔的結尾），然後重試。

**自訂 VM 映像檔。** `runner-image` 除了接受 OCI 參照，也接受本機 VM 名稱：啟動時會先檢查 `tart list`，只有找不到該名稱時才會拉取映像檔。若要將專案相依項目預先放入映像檔（模擬器執行環境、CocoaPods/SPM 快取、額外的工具鏈），請複製 Cirrus Labs 映像檔、完成環境配置並將其停止，再將 `runner-image` 指向配置好的 VM：

```bash
tart clone ghcr.io/cirruslabs/macos-golden-gate-xcode:latest ios-golden
tart run ios-golden --no-graphics &
tart exec -it ios-golden zsh      # runs as admin; install tools, warm caches
tart stop ios-golden
```

```toml
runner-image = "ios-golden"       # local VM — no registry needed
```

每個工作仍會啟動全新的寫入時複製 VM；runner 絕不會直接啟動黃金範本 VM。若要跨主機共用，可先執行 `tart login ghcr.io`，再執行 `tart push ios-golden ghcr.io/<org>/ios-golden:latest`；也可使用 [Packer tart builder](https://github.com/openai/tart/blob/main/docs/integrations/packer.md) 與 [macos-image-templates](https://github.com/cirruslabs/macos-image-templates) 讓建置流程可重現。不論採用哪個基底，映像檔都必須保持 [Tart Guest Agent](https://github.com/openai/tart/blob/main/docs/blog/posts/2025-06-01-tart-guest-agent.md) 運作；runner 透過 `tart exec` 而非 SSH 操作 VM，且每個非 vanilla 的 Cirrus Labs 映像檔都已內建此代理程式。映像檔也必須在 `runner-dir` 下解壓縮好 GitHub Actions runner，並具備可執行的 `run.sh`。配置黃金範本 VM 會占用主機兩個 macOS VM 名額中的一個，因此請在 runner 尚未滿載時進行。

**Runner 版本淘汰。** GitHub 會在伺服器端淘汰 runner 版本：每個版本都會在較新版本發布數週後迎來其 `runtime_deprecates_at` 日期；仍使用已淘汰版本的 runner 在首次輪詢訊息時，會收到 `Runner version vX.Y.Z is deprecated and cannot receive messages` 拒絕訊息並結束。runner 規模集仍會顯示 Online，但它啟動的每個 runner 都會終止，因此工作會一直排隊；在 Tart 上，上述啟動檢查會記錄此拒絕訊息。每個短暫存在的容器或 VM 啟動時，都會使用映像檔內建的 runner 版本，`disable-update` 無法改變這點：更新指示會隨 GitHub 拒絕的同一次訊息輪詢傳回（請見[社群討論 #206494](https://github.com/orgs/community/discussions/206494)），所以 `disable-update = false` 只會讓仍受支援的 runner 在執行第一個工作前下載最新版本，無法挽救已淘汰的版本。因此 runner 預設使用 `disable-update = true`，並改為透過更新映像檔來更新 runner：重新建置容器映像檔，或替換黃金範本 VM 中的 `actions-runner`。若要查看某個版本距離淘汰還有多久：

```bash
gh api repos/<owner>/<repo>/actions/runners/deprecations/<version>   # runtime_deprecates_at
```

組織層級的端點（`orgs/<org>/actions/runners/deprecations/<version>`）需要 `admin:org` 範圍。

<a id="github-app-authentication"></a>

## 主機本機 Actions 快取（opt-in）

使用內建 Go 接收器時，在 `[scaleset.docker]` 設定 `native-actions-cache = true`；
管理程序會自動採用 `[actions-cache].advertise-url`。預設為 false，也可以在 scale set
用 false 覆寫繼承的 true。既有的 `actions-cache-url` 仍可指定外部接收器等明確端點；
若與 `native-actions-cache = true` 同時設定或繼承，URL 必須與全域 advertise URL 相同。
Linux amd64 環境建議逐一 scale set 啟用，並搭配固定 digest、通過驗收的快取專用
runner 變體。標準 runner 或單純 fork base image 都不夠；最終映像必須提供
Runscaler 的 capability checker，先驗證能力再接收工作。

端點必須是有 host 與結尾實際斜線的完整 HTTP(S) URL，不能有帳密、query 或 fragment。
驗證錯誤不會回顯輸入的敏感值，也不會自動修正或正規化 URL。實際 runner 與旁系
BuildKit 容器都必須能連線至此端點。

```toml
[[scaleset]]
url = "https://github.com/<owner>"
name = "runscaler-local-cache-canary"
labels = ["runscaler-local-cache-canary"]
token = "env:RUNNER_TOKEN"
max-runners = 1
runner-image = "<verified-local-cache-image>@sha256:<accepted-digest>"

[scaleset.docker]
platform = "linux/amd64"
native-actions-cache = true
# cache-volumes = [] # 只有需要關閉繼承的掛載時才設定
# cache = []         # 只有需要關閉繼承的掛載時才設定
```

服務與資料不可歸屬於 runner，也不可放進一般可清空的快取掛載。
詳見[服務設定、client 範圍與回復方式](local-actions-cache.zh-TW.md)。
回復時先 drain 並停用受影響的 opt-in scale set，再還原設定；直接關閉原生快取開關並恢復
原 cache action 會重新使用 GitHub cache。空字串覆寫是設定語意，不是保證資料只留
本機的回復程序。

`cache-volumes` 與 `[[docker.cache]]` 已開始 sunset；現有設定仍照常掛載並執行
原本的容量政策，直到從設定移除為止。切換 workflow 或處理舊 Docker volume 前，
請先依照[遷移步驟](caching.zh-TW.md#遷移至-actions-cache)驗收。

<a id="native-go-receiver-opt-in"></a>

### 原生 Go 接收器（opt-in）

Manager 可選擇在另一個私有 listener 提供 cache-v2 接收器。已有實際簽名 job
與 provider-managed canary 的驗收紀錄；新主機仍須驗證自己的 runner 映像、端點
及 workflow。以下是全域設定，不會繼承至各 scale set：

```toml
[actions-cache]
enabled = true
listen-address = "172.18.0.1:32500"
advertise-url = "http://cache.internal:32500/"
data-dir = "/var/lib/runner/native-actions-cache"
max-size-bytes = 53687091200
max-idle-days = 30
# audiences = ["an-exact-verified-runtime-audience"]
```

`listen-address` 應使用實際私有主機 IP；`advertise-url` 須能從 runner 與旁系
BuildKit 容器連通。資料目錄須為全新、私有，且與 Rust volume 分開。預設使用
GitHub token issuer／JWKS 與 Results 上游；可信任的 HTTPS issuer 可同時設定
`token-issuer` 與 `jwks-url`，`upstream-results-url` 則是固定 HTTPS 來源。
沒有略過 token 驗證的開關。`native-actions-cache = true` 必須搭配已啟用的全域
接收器；scale set 會自動取得 advertise URL。其他 scale set 保留原有端點。
啟用後，文字版與 JSON 版 `runner status` 會回報原生快取的就緒狀態、項目數與容量；
文字版也會顯示相對於共用保留目標的用量。`runner cache` 可查看更詳細的儲存資訊。
只有需要關閉繼承的掛載時，才需設定空的 `cache-volumes = []` 與 `cache = []`。

## GitHub App 驗證

組織與儲存庫 runner 支援 GitHub App 驗證。請將 App 安裝至擁有這些 runner 的組織或儲存庫。組織 runner 需授予組織層級的 **Self-hosted runners: Read and write** 權限。儲存庫 runner 需授予儲存庫層級的 **Administration: Read and write** 權限；儲存庫的 **Metadata: Read-only** 權限會一併包含。企業層級的 runner 需要 PAT。App 註冊與權限細節請見 GitHub 的 [runner 驗證指南](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/authenticate-to-the-api)。

請設定 App 的 Client ID（建議使用；以字串表示的數字 App ID 也適用）、Installation ID，以及恰好一個私密金鑰來源。這些 ID 並不相同；Installation ID 用來識別此 App 在目標擁有者帳戶上的安裝。請見 [GitHub 的 JWT 指南](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app)。

```toml
url = "https://github.com/your-org"
name = "app-runners"
max-runners = 2

github-app.client-id = "Iv1.example"
github-app.installation-id = 123456
github-app.private-key-file = "/absolute/path/to/runner-app.pem"
# Alternatively, replace private-key-file with an environment variable name:
# github-app.private-key-env = "RUNNER_GITHUB_APP_KEY"
```

金鑰必須是未加密的 RSA PEM 金鑰（PKCS#1 或 PKCS#8），大小不得超過 64 KiB。請使用絕對檔案路徑，將檔案存放在儲存庫外，並設定僅限擁有者存取的權限，例如 `0600`。若使用環境變數來源，指定的變數必須包含完整的 PEM 值，且使用實際的換行字元；設定檔只會儲存變數名稱。金鑰會在建立用戶端時讀取，因此輪替金鑰後請重新啟動管理程序。SDK 會視需要簽署 JWT 並交換憑證。

產生的服務定義不會從執行安裝的 shell 複製 App 金鑰環境變數。這些服務請使用金鑰檔案，或明確透過服務管理程式提供該變數。

存在 App 設定時，`RUNNER_TOKEN` 備援變數會被忽略。若明確設定 `token` 或 `--token`，又同時提供 App 設定，便會發生錯誤。App 設定不完整時也會失敗，不會無聲地改用 PAT 驗證。

與 PAT 憑證一樣，App 憑證絕不會由 `[[scaleset]]` 項目繼承。請個別設定每個安裝；使用 App 與 PAT 的 runner 規模集可以在同一個程序中執行：

```toml
[[scaleset]]
url = "https://github.com/org-a"
name = "app-runners"
github-app.client-id = "Iv1.example"
github-app.installation-id = 123456
github-app.private-key-env = "ORG_A_APP_KEY"

[[scaleset]]
url = "https://github.com/org-b"
name = "pat-runners"
token = "env:ORG_B_TOKEN"
```

若要以非互動方式設定，`runner init` 接受與 `runner run` 相同的 App 參照旗標，並會直接寫入參照，不會提示輸入 PAT：

```bash
runner init --url https://github.com/your-org --name app-runners \
  --provider docker --max-runners 2 --dind=false --shared-volume="" \
  --github-app-client-id Iv1.example --github-app-installation-id 123456 \
  --github-app-private-key-file /absolute/path/to/runner-app.pem
```

`run`、`validate`、`doctor` 與 `scaleset delete` 使用相同的驗證選擇邏輯。`doctor` 會對每個已設定的 runner 規模集執行唯讀 API 探測；`validate` 也會檢查 供應器連線與設定。

<a id="token-security"></a>

## 權杖安全性

避免透過 CLI 旗標傳入權杖（會出現在 `ps` 輸出中）。可改用以下兩種方式：

**方式 1：`RUNNER_TOKEN` 環境變數** — 未設定 `--token` 旗標或設定檔值時，會自動使用此變數：

```bash
export RUNNER_TOKEN=ghp_xxx
runner run --url https://github.com/org --name my-runners
```

**方式 2：設定檔中的 `env:` 語法** — 以名稱參照任意環境變數：

```toml
token = "env:GITHUB_TOKEN"  # reads from $GITHUB_TOKEN at startup
```

優先順序：`--token` 旗標 > `RUNNER_TOKEN` 環境變數 > 設定檔值（包含 `env:` 解析）。

**多個 runner 規模集（混合使用 Docker + Tart）：**

```toml
# Global defaults (inherited by all scale sets)
runner-image = "ghcr.io/actions/actions-runner:latest"
runner-group = "default"
max-runners = 10
log-level = "info"

[docker]
socket = "/var/run/docker.sock"
dind = true

# Each [[scaleset]] runs independently.
# Inherits global settings if omitted.

[[scaleset]]
url = "https://github.com/your-org"
name = "linux-runners"
token = "ghp_aaa"

[[scaleset]]
provider = "tart"
url = "https://github.com/your-org"
name = "macos-runners"
token = "ghp_bbb"
max-runners = 2
runner-image = "ghcr.io/cirruslabs/macos-golden-gate-xcode:latest"
labels = ["self-hosted", "macOS"]
[scaleset.tart]
pool-size = 2
```

<a id="cli-flags"></a>

## CLI 旗標

| 旗標 | TOML 設定鍵 | 預設值 | 說明 |
| ------------------- | -------------------- | --------------------------------------- | ------------------------------------------------- |
| `--config` | | | TOML 設定檔路徑 |
| `--url` | `url` | （必填） | 註冊 URL（組織或儲存庫） |
| `--name` | `name` | （必填） | runner 規模集名稱（用作 `runs-on` 標籤） |
| `--token` | `token` | （PAT 模式必填） | GitHub 個人存取權杖 |
| `--github-app-client-id` | `github-app.client-id` | （未設定） | GitHub App Client ID，或以字串表示的數字 App ID |
| `--github-app-installation-id` | `github-app.installation-id` | （未設定） | GitHub App 安裝 ID |
| `--github-app-private-key-file` | `github-app.private-key-file` | （未設定） | RSA PEM 金鑰的絕對路徑 |
| `--github-app-private-key-env` | `github-app.private-key-env` | （未設定） | 包含 PEM 金鑰的環境變數名稱 |
| `--provider` | `provider` | `docker` | 執行個體供應器（`docker` 或 `tart`） |
| `--max-runners` | `max-runners` | `10` | 同時執行的 runner 數量上限 |
| `--concurrent` | `concurrent` | `0`（`max-runners` 總和） | 主機上所有 runner 規模集合計的數量上限 |
| `--min-runners` | `min-runners` | `0` | 保持預備狀態的 runner 數量下限 |
| `--labels` | `labels` | `<name>` | runner 標籤（以逗號分隔） |
| `--runner-group` | `runner-group` | `default` | runner 群組名稱 |
| `--runner-image` | `runner-image` | `ghcr.io/actions/actions-runner:latest` | runner 映像檔（Docker 映像檔或 Tart VM 映像檔） |
| `--docker-socket` | `[docker] socket` | `/var/run/docker.sock` | Docker socket 路徑（Docker 供應器） |
| `--dind` | `[docker] dind` | `true` | 將 Docker socket 掛載至 runner（Docker 供應器） |
| — | `[docker] workspace-root` | 未設定 | Linux 主機上的私人目錄，供各 runner 轉換 workspace bind 路徑 |
| `--shared-volume` | `[docker] shared-volume` | | Docker 共用磁碟區路徑（Docker 供應器） |
| `--tart-cpu` | `[tart] cpu` | `0`（映像檔預設值） | 每個 VM 的 CPU 核心數（Tart 供應器） |
| `--tart-memory` | `[tart] memory` | `0`（映像檔預設值） | 每個 VM 的記憶體大小，單位為 MB（Tart 供應器） |
| `--tart-runner-dir` | `[tart] runner-dir` | `/Users/admin/actions-runner` | Tart VM 內的 runner 安裝目錄 |
| `--tart-pool-size` | `[tart] pool-size` | `0` | 預先暖機、可立即接收工作的 VM 數量 |
| `--tart-net-bridged` | `[tart] net-bridged` | `""`（Tart 的 NAT） | 每個 VM 要橋接至的主機介面 |
| `--log-level` | `log-level` | `info` | 記錄層級（debug/info/warn/error） |
| `--log-format` | `log-format` | `text` | 記錄格式（text/json） |
| `--dry-run` | `dry-run` | `false` | 驗證所有項目，但不啟動接聽程序 |
| `--health-address` | `health-address` | `127.0.0.1` | 健康檢查的接聽位址 |
| `--health-port` | `health-port` | `8080` | 健康檢查的 HTTP 連接埠（0 表示停用） |

適用於整個程序的 `drain-timeout` 與進階調校設定鍵（清理、快取磁碟區、隔離）刻意設計為只能透過設定檔設定；完整清單請見 `config.example.toml`。

與 GitLab Runner 相同，`concurrent` 是整個管理程序的上限，而各 runner 規模集的 `max-runners` 則是其個別上限。控制器會在 JIT／供應器 啟動前預留全域容量，因此多個組織，以及混合使用 Docker/Tart 的 runner 規模集，合計都無法超額占用主機容量。系統會先為每個 runner 規模集預留 `min-runners` 下限所需的容量，再以先進先出（FIFO）順序分配剩餘容量。將 `concurrent` 保持為 `0` 時，會以所有已設定的 `max-runners` 值總和作為上限，以維持向下相容的行為。

使用 `provider` TOML 設定鍵與 `--provider` 旗標選擇 Docker 或 Tart。
