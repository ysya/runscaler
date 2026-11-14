// Fields mirror the mapstructure keys in internal/config. Blank optional fields
// are omitted, so the runtime can apply its own defaults.
const field = (key, type, en, zh, extra = {}) => ({ key, type, en, zh, ...extra });

export const globalFields = [
  field('log-level', 'select', 'Log level', '紀錄等級', { value: 'info', choices: ['debug', 'info', 'warn', 'error'] }),
  field('log-format', 'select', 'Log format', '紀錄格式', { value: 'text', choices: ['text', 'json'] }),
  field('health-port', 'number', 'Health check port (0 disables)', '健康檢查連接埠（0 為停用）', { value: '8080', min: 0, max: 65535 }),
  field('health-address', 'text', 'Health check address', '健康檢查位址', { value: '127.0.0.1' }),
  field('dry-run', 'bool', 'Dry run', '模擬執行'),
  field('log-file', 'text', 'Log file (empty string disables)', '紀錄檔路徑（空字串停用）'),
  field('concurrent', 'number', 'Host-wide concurrent limit (0 = automatic)', '主機同時執行上限（0 為自動）', { min: 0 }),
  field('drain-timeout', 'duration', 'Shutdown drain timeout', '停止服務時等待工作完成的時間', { placeholder: '2h' }),
];

export const setFields = [
  field('url', 'url', 'GitHub organization or repository URL *', 'GitHub 組織或儲存庫網址 *', { placeholder: 'https://github.com/your-org' }),
  field('name', 'text', 'Scale-set name / runs-on label *', '規模集名稱／runs-on 標籤 *', { placeholder: 'my-runners' }),
  field('provider', 'select', 'Provider', '執行環境', { value: 'docker', choices: ['docker', 'tart'] }),
  field('auth', 'select', 'Authentication', '驗證方式', { value: 'pat', choices: ['pat', 'app'] }),
  field('token-env', 'text', 'PAT environment variable name (optional)', 'PAT 環境變數名稱（選填）', { visible: 'pat', helpEn: 'Only the variable name is added to TOML. Set its value on the host.', helpZh: 'TOML 只會寫入變數名稱；請在主機上設定其值。' }),
  field('client-id', 'text', 'GitHub App client ID', 'GitHub App 用戶端 ID', { visible: 'app', placeholder: 'Iv1.example' }),
  field('installation-id', 'number', 'GitHub App installation ID', 'GitHub App 安裝 ID', { visible: 'app', min: 1 }),
  field('app-key-source', 'select', 'Private-key source', '私鑰來源', { visible: 'app', value: 'file', choices: ['file', 'env'] }),
  field('key-file', 'text', 'Private-key file path on host', '主機上的私鑰檔案路徑', { visible: 'app-file', placeholder: '/etc/runner/app.pem' }),
  field('key-env', 'text', 'Private-key environment variable name', '私鑰環境變數名稱', { visible: 'app-env', helpEn: 'Enter only the variable name, never the private key.', helpZh: '只輸入變數名稱，不要貼上私鑰。' }),
  field('max-runners', 'number', 'Maximum runners *', '最多 runner 數量 *', { value: '10', min: 1 }),
  field('min-runners', 'number', 'Warm runner minimum', '常駐 runner 下限', { min: 0 }),
  field('labels', 'lines', 'Additional labels (one per line)', '其他標籤（每行一個）'),
  field('runner-group', 'text', 'Runner group', 'Runner 群組', { placeholder: 'default' }),
  field('runner-image', 'text', 'Runner image *', 'Runner 映像檔 *', { value: 'ghcr.io/actions/actions-runner:latest' }),
  field('disable-update', 'bool', 'Disable runner auto-update', '停用 runner 自動更新', { helpEn: 'Default: enabled. Rebuild the image to update the runner.', helpZh: '預設為啟用；請透過重建映像檔更新 runner。' }),
];

export const dockerFields = [
  field('readiness', 'bool', 'Wait for runner readiness', '等待 runner 就緒'),
  field('ready-timeout', 'duration', 'Readiness timeout', '就緒逾時', { placeholder: '5m' }),
  field('socket', 'text', 'Docker socket', 'Docker socket', { placeholder: '/var/run/docker.sock' }),
  field('dind', 'bool', 'Expose host Docker socket to jobs', '讓工作使用主機 Docker socket', { value: 'true', helpEn: 'Gives jobs root-equivalent host access. Do not run untrusted public-fork PRs.', helpZh: '工作可取得等同主機 root 的權限；勿執行不受信任的公開 fork PR。' }),
  field('workspace-root', 'text', 'Private workspace directory on host', '主機上的私人工作目錄', { helpEn: 'Requires Docker socket access.', helpZh: '需要允許工作使用 Docker socket。' }),
  field('shared-volume', 'text', 'Shared volume mount path', '共用 volume 掛載路徑', { placeholder: '/shared' }),
  field('memory', 'number', 'Runner memory limit (MB; 0 = unlimited)', 'Runner 記憶體上限（MB；0 為無上限）', { min: 0 }),
  field('cpu', 'number', 'Runner CPU cores (0 = unlimited)', 'Runner CPU 核心數（0 為無上限）', { min: 0 }),
  field('pids-limit', 'number', 'Runner PID limit (0 = unlimited)', 'Runner PID 上限（0 為無上限）', { min: 0 }),
  field('platform', 'text', 'Docker platform', 'Docker 平台', { placeholder: 'linux/amd64' }),
  field('network', 'text', 'Existing Docker network', '既有 Docker 網路'),
  field('shared-volume-name', 'text', 'Shared volume name', '共用 volume 名稱'),
  field('shared-volume-max-age', 'duration', 'Shared volume file max age', '共用 volume 檔案最長保留時間', { helpEn: 'Without this setting, shared files are not automatically reclaimed.', helpZh: '未設定時，共用檔案不會自動回收。' }),
  field('shared-volume-cleanup-interval', 'duration', 'Shared volume cleanup interval', '共用 volume 清理間隔'),
  field('buildx-cleanup', 'bool', 'Clean orphaned buildx builders', '清理孤兒 buildx builder', { helpEn: 'Use only on a Docker daemon dedicated to runners.', helpZh: '僅適用於 runner 專用 Docker daemon。' }),
  field('buildx-cleanup-ttl', 'duration', 'Buildx builder age limit', 'Buildx builder 保留時間'),
  field('buildx-cleanup-interval', 'duration', 'Buildx cleanup interval', 'Buildx 清理間隔'),
  field('prune', 'bool', 'Prune Docker runtime objects', '清理 Docker 執行環境物件', { helpEn: 'Affects the entire daemon; use only on a dedicated runner daemon.', helpZh: '影響整個 daemon；僅適用於 runner 專用 daemon。' }),
  field('prune-interval', 'duration', 'Docker prune interval', 'Docker 清理間隔'),
  field('prune-ttl', 'duration', 'Stopped container and image age limit', '已停止容器與映像檔保留時間'),
  field('build-cache-max-age', 'duration', 'Docker build cache max age', 'Docker build cache 最長保留時間'),
  field('build-cache-budget', 'number', 'Docker build cache budget (GB)', 'Docker build cache 容量上限（GB）', { min: 0 }),
  field('actions-cache-url', 'url', 'External Actions cache URL', '外部 Actions 快取網址', { helpEn: 'Requires a cache-capable runner image; URL must end with /.', helpZh: '需要支援快取的 runner 映像檔；網址須以 / 結尾。' }),
  field('native-actions-cache', 'bool', 'Use manager native Actions cache', '使用管理程序內建 Actions 快取', { helpEn: 'Also enable and configure the global native receiver.', helpZh: '還需啟用並設定全域內建接收端。' }),
  field('cache-volumes', 'lines', 'Legacy cache volumes (name:/container/path per line)', '舊版 cache volumes（每行 name:/容器路徑）', { legacy: true }),
  field('cache', 'cache', 'Legacy budgeted cache volumes (name | path | budget | on-exceed)', '舊版有容量上限的 cache volumes（名稱 | 路徑 | 上限 | 超限處理）', { legacy: true, helpEn: 'One entry per line. Example: gradle | /home/runner/.gradle | 20GB | warn (or wipe)', helpZh: '每行一筆。例如：gradle | /home/runner/.gradle | 20GB | warn（或 wipe）' }),
];

export const tartFields = [
  field('home', 'text', 'Tart home directory', 'Tart 主目錄'),
  field('runner-dir', 'text', 'Runner directory inside VM', 'VM 內 runner 目錄', { placeholder: '/Users/admin/actions-runner', helpEn: 'Default: /Users/admin/actions-runner for macOS guests, /home/admin/actions-runner for Linux guests.', helpZh: '預設：macOS guest 為 /Users/admin/actions-runner，Linux guest 為 /home/admin/actions-runner。' }),
  field('cpu', 'number', 'VM CPU cores', 'VM CPU 核心數', { min: 0 }),
  field('memory', 'number', 'VM memory (MB)', 'VM 記憶體（MB）', { min: 0 }),
  field('pool-size', 'number', 'Pre-warmed VMs', '預先啟動的 VM 數量', { min: 0, helpEn: 'Guest OS is detected from the image. macOS guests run at most two VMs per host; larger max-runners values are capped at two. Linux guests run up to max-runners plus pool-size VMs.', helpZh: '會從映像檔偵測 guest OS。macOS guest 每台主機最多兩台 VM，較大的 max-runners 值會限制為 2；Linux guest 最多執行 max-runners 加 pool-size 台 VM。' }),
  field('mtu', 'number', 'Guest MTU (0 = auto, -1 = off)', 'VM MTU（0 自動，-1 停用）', { min: -1, max: 1500 }),
  field('dns', 'lines', 'Guest DNS IPs (one per line)', 'VM DNS IP（每行一個）'),
  field('net-bridged', 'text', 'Bridged host interface', '橋接的主機網路介面', { placeholder: 'en0' }),
  field('cache-cleanup', 'bool', 'Clean Tart OCI/IPSW cache', '清理 Tart OCI/IPSW 快取', { helpEn: 'Enabled by default; local VMs are not deleted.', helpZh: '預設啟用；不會刪除本機 VM。' }),
  field('cache-max-age', 'duration', 'Tart cache max age', 'Tart 快取最長保留時間', { placeholder: '168h' }),
  field('cache-budget', 'number', 'Tart cache budget (GB)', 'Tart 快取容量上限（GB）', { min: 0 }),
  field('cache-cleanup-interval', 'duration', 'Tart cache cleanup interval', 'Tart 快取清理間隔', { placeholder: '24h' }),
];

export const retryFields = [
  field('initial', 'duration', 'Initial retry delay', '初次重試間隔', { placeholder: '5s' }),
  field('max', 'duration', 'Maximum retry delay', '最大重試間隔', { placeholder: '1m' }),
  field('cooldown-after', 'number', 'Failures before cooldown', '進入冷卻前的失敗次數', { min: 1 }),
  field('cooldown', 'duration', 'Cooldown duration', '冷卻時間', { placeholder: '5m' }),
];

export const actionsCacheFields = [
  field('enabled', 'bool', 'Enable native Actions cache receiver', '啟用內建 Actions 快取接收端'),
  field('listen-address', 'text', 'Private listen IP and port', '私人監聽 IP 與連接埠', { placeholder: '172.18.0.1:32500' }),
  field('advertise-url', 'url', 'URL reachable by runner containers', 'Runner 容器可連線的網址', { placeholder: 'http://cache.internal:32500/' }),
  field('data-dir', 'text', 'Private host data directory', '主機上的私人資料目錄', { placeholder: '/var/lib/runner/native-actions-cache' }),
  field('max-size-bytes', 'number', 'Maximum stored bytes', '最大儲存位元組數', { min: 1, placeholder: '53687091200' }),
  field('max-idle-days', 'number', 'Maximum idle days', '最長閒置天數', { min: 0 }),
  field('token-issuer', 'url', 'Custom token issuer (advanced)', '自訂權杖簽發者（進階）'),
  field('jwks-url', 'url', 'Custom JWKS URL (advanced)', '自訂 JWKS 網址（進階）'),
  field('audiences', 'lines', 'Accepted audiences (one per line)', '接受的 audience（每行一個）'),
  field('upstream-results-url', 'url', 'Upstream results URL (advanced)', '上游 results 網址（進階）'),
];

export const diskFields = [
  field('guard', 'bool', 'Enable disk-pressure guard', '啟用磁碟空間保護', { helpEn: 'Enabled by default.', helpZh: '預設啟用。' }),
  field('min-free', 'text', 'Start cleanup below free space', '低於此剩餘空間時開始清理', { placeholder: '10%' }),
  field('target-free', 'text', 'Cleanup target free space', '清理目標剩餘空間', { placeholder: '20%' }),
  field('interval', 'duration', 'Disk check interval', '磁碟檢查間隔', { placeholder: '1h' }),
  field('max-tier', 'number', 'Maximum cleanup tier (1–4)', '最高清理等級（1–4）', { min: 1, max: 4, helpEn: 'Tier 4 can wipe legacy cache volumes.', helpZh: '第 4 級可能清空舊版 cache volume。' }),
];

export const diagnosticsFields = [
  field('enabled', 'bool', 'Enable diagnostics', '啟用診斷'),
  field('capture-logs', 'bool', 'Capture runner logs', '擷取 runner 紀錄'),
];

export const groups = { global: globalFields, set: setFields, docker: dockerFields, tart: tartFields, retry: retryFields, 'actions-cache': actionsCacheFields, disk: diskFields, diagnostics: diagnosticsFields };
