// Fields mirror the mapstructure keys in internal/config. Blank optional fields
// are omitted, so the runtime can apply its own defaults.
const field = (key, type, en, zh, ja, extra = {}) => ({ key, type, en, zh, ja, ...extra });

export const globalFields = [
  field('log-level', 'select', 'Log level', '紀錄等級', 'ログレベル', { value: 'info', choices: ['debug', 'info', 'warn', 'error'] }),
  field('log-format', 'select', 'Log format', '紀錄格式', 'ログ形式', { value: 'text', choices: ['text', 'json'] }),
  field('health-port', 'number', 'Health check port (0 disables)', '健康檢查連接埠（0 為停用）', 'ヘルスチェックのポート（0 で無効）', { value: '8080', min: 0, max: 65535 }),
  field('health-address', 'text', 'Health check address', '健康檢查位址', 'ヘルスチェックのアドレス', { value: '127.0.0.1' }),
  field('dry-run', 'bool', 'Dry run', '模擬執行', 'ドライラン'),
  field('log-file', 'text', 'Log file (empty string disables)', '紀錄檔路徑（空字串停用）', 'ログファイル（空文字列で無効）'),
  field('concurrent', 'number', 'Host-wide concurrent limit (0 = automatic)', '主機同時執行上限（0 為自動）', 'ホスト全体の同時実行数（0 で自動）', { min: 0 }),
  field('drain-timeout', 'duration', 'Shutdown drain timeout', '停止服務時等待工作完成的時間', '終了時にジョブの完了を待つ時間', { placeholder: '2h' }),
];

export const setFields = [
  field('url', 'url', 'GitHub organization or repository URL *', 'GitHub 組織或儲存庫網址 *', 'GitHub 組織またはリポジトリの URL *', { placeholder: 'https://github.com/your-org' }),
  field('name', 'text', 'Scale-set name / runs-on label *', '規模集名稱／runs-on 標籤 *', 'スケールセット名／runs-on ラベル *', { placeholder: 'my-runners' }),
  field('provider', 'select', 'Provider', '執行環境', 'プロバイダー', { value: 'docker', choices: ['docker', 'tart'] }),
  field('auth', 'select', 'Authentication', '驗證方式', '認証方式', { value: 'pat', choices: ['pat', 'app'] }),
  field('token-env', 'text', 'PAT environment variable name (optional)', 'PAT 環境變數名稱（選填）', 'PAT の環境変数名（任意）', { visible: 'pat', helpEn: 'Only the variable name is added to TOML. Set its value on the host.', helpZh: 'TOML 只會寫入變數名稱；請在主機上設定其值。', helpJa: 'TOML には変数名だけを書き込みます。値はホスト上で設定してください。' }),
  field('client-id', 'text', 'GitHub App client ID', 'GitHub App 用戶端 ID', 'GitHub App のクライアント ID', { visible: 'app', placeholder: 'Iv1.example' }),
  field('installation-id', 'number', 'GitHub App installation ID', 'GitHub App 安裝 ID', 'GitHub App のインストール ID', { visible: 'app', min: 1 }),
  field('app-key-source', 'select', 'Private-key source', '私鑰來源', '秘密鍵の参照方法', { visible: 'app', value: 'file', choices: ['file', 'env'] }),
  field('key-file', 'text', 'Private-key file path on host', '主機上的私鑰檔案路徑', 'ホスト上の秘密鍵ファイルのパス', { visible: 'app-file', placeholder: '/etc/runner/app.pem' }),
  field('key-env', 'text', 'Private-key environment variable name', '私鑰環境變數名稱', '秘密鍵の環境変数名', { visible: 'app-env', helpEn: 'Enter only the variable name, never the private key.', helpZh: '只輸入變數名稱，不要貼上私鑰。', helpJa: '変数名だけを入力してください。秘密鍵を貼り付けないでください。' }),
  field('max-runners', 'number', 'Maximum runners *', '最多 runner 數量 *', 'ランナー数の上限 *', { value: '10', min: 1 }),
  field('min-runners', 'number', 'Warm runner minimum', '常駐 runner 下限', '待機ランナー数の下限', { min: 0 }),
  field('labels', 'lines', 'Additional labels (one per line)', '其他標籤（每行一個）', '追加ラベル（1 行に 1 つ）'),
  field('runner-group', 'text', 'Runner group', 'Runner 群組', 'ランナーグループ', { placeholder: 'default' }),
  field('runner-image', 'text', 'Runner image *', 'Runner 映像檔 *', 'ランナーイメージ *', { value: 'ghcr.io/actions/actions-runner:latest' }),
  field('registration-sweep-mode', 'select', 'Leaked registration sweep', '清理遺留的 runner 註冊', '残存するランナー登録のクリーンアップ', { choices: ['off', 'log', 'remove'], helpEn: 'Organization scale sets only. Start with log; remove deletes registrations on GitHub. Assumes one runner process per scale set.', helpZh: '僅適用於組織規模集。請先用 log；remove 會刪除 GitHub 上的註冊。假設每個規模集只有一個 runner 程序。', helpJa: '組織のスケールセット専用です。まず log を使用してください。remove は GitHub 上の登録を削除します。スケールセットごとに 1 つのランナープロセスを想定しています。' }),
  field('registration-sweep-interval', 'duration', 'Registration sweep interval', '註冊清理間隔', 'ランナー登録のクリーンアップ間隔', { placeholder: '10m' }),
  field('disable-update', 'bool', 'Disable runner auto-update', '停用 runner 自動更新', 'ランナーの自動更新を無効化', { helpEn: 'Default: enabled. Rebuild the image to update the runner.', helpZh: '預設為啟用；請透過重建映像檔更新 runner。', helpJa: '自動更新の無効化は既定で有効です。ランナーを更新するにはイメージを再ビルドしてください。' }),
];

export const dockerFields = [
  field('readiness', 'bool', 'Wait for runner readiness', '等待 runner 就緒', 'ランナーの準備完了を待機'),
  field('ready-timeout', 'duration', 'Readiness timeout', '就緒逾時', '準備完了の待機タイムアウト', { placeholder: '5m' }),
  field('socket', 'text', 'Docker socket', 'Docker socket', 'Docker ソケット', { placeholder: '/var/run/docker.sock' }),
  field('dind', 'bool', 'Expose host Docker socket to jobs', '讓工作使用主機 Docker socket', 'ホストの Docker ソケットをジョブに公開', { value: 'true', helpEn: 'Gives jobs root-equivalent host access. Do not run untrusted public-fork PRs.', helpZh: '工作可取得等同主機 root 的權限；勿執行不受信任的公開 fork PR。', helpJa: 'ジョブにホストの root 相当の権限を与えます。信頼できない公開フォークの PR を実行しないでください。' }),
  field('daemon-mode', 'select', 'Job Docker daemon', '工作 Docker daemon', 'ジョブ用 Docker デーモン', { choices: ['host', 'dind', 'none'], helpEn: 'host shares the host socket; dind creates a private daemon per job; none disables Docker access.', helpZh: 'host 共用主機 socket；dind 為每個工作建立私人 daemon；none 停用 Docker 存取。', helpJa: 'host はホストのソケットを共有し、dind はジョブごとに専用デーモンを作成します。none は Docker へのアクセスを無効にします。' }),
  field('dind-image', 'text', 'Private Docker daemon image', '私人 Docker daemon 映像檔', '専用 Docker デーモンのイメージ', { placeholder: 'docker:29-dind' }),
  field('dind-startup-timeout', 'duration', 'Private Docker daemon startup timeout', '私人 Docker daemon 啟動逾時', '専用 Docker デーモンの起動タイムアウト', { placeholder: '2m' }),
  field('dind-memory', 'number', 'Private Docker daemon memory (MB)', '私人 Docker daemon 記憶體（MB）', '専用 Docker デーモンのメモリ（MB）', { min: 0 }),
  field('dind-cpu', 'number', 'Private Docker daemon CPU cores', '私人 Docker daemon CPU 核心數', '専用 Docker デーモンの CPU コア数', { min: 0 }),
  field('dind-pids-limit', 'number', 'Private Docker daemon PID limit', '私人 Docker daemon PID 上限', '専用 Docker デーモンの PID 上限', { min: 0 }),
  field('workspace-root', 'text', 'Private workspace directory on host', '主機上的私人工作目錄', 'ホスト上の専用ワークスペースディレクトリ', { helpEn: 'Requires Docker socket access.', helpZh: '需要允許工作使用 Docker socket。', helpJa: 'Docker ソケットへのアクセスが必要です。' }),
  field('shared-volume', 'text', 'Shared volume mount path', '共用 volume 掛載路徑', '共有ボリュームのマウント先', { placeholder: '/shared' }),
  field('memory', 'number', 'Runner memory limit (MB; 0 = unlimited)', 'Runner 記憶體上限（MB；0 為無上限）', 'ランナーのメモリ上限（MB、0 で無制限）', { min: 0 }),
  field('cpu', 'number', 'Runner CPU cores (0 = unlimited)', 'Runner CPU 核心數（0 為無上限）', 'ランナーの CPU コア数（0 で無制限）', { min: 0 }),
  field('pids-limit', 'number', 'Runner PID limit (0 = unlimited)', 'Runner PID 上限（0 為無上限）', 'ランナーの PID 上限（0 で無制限）', { min: 0 }),
  field('platform', 'text', 'Docker platform', 'Docker 平台', 'Docker プラットフォーム', { placeholder: 'linux/amd64' }),
  field('network', 'text', 'Existing Docker network', '既有 Docker 網路', '既存の Docker ネットワーク'),
  field('shared-volume-name', 'text', 'Shared volume name', '共用 volume 名稱', '共有ボリューム名'),
  field('shared-volume-max-age', 'duration', 'Shared volume file max age', '共用 volume 檔案最長保留時間', '共有ボリューム内のファイルの保持期間', { helpEn: 'Without this setting, shared files are not automatically reclaimed.', helpZh: '未設定時，共用檔案不會自動回收。', helpJa: 'この設定を省略すると、共有ファイルは自動削除されません。' }),
  field('shared-volume-cleanup-interval', 'duration', 'Shared volume cleanup interval', '共用 volume 清理間隔', '共有ボリュームのクリーンアップ間隔'),
  field('buildx-cleanup', 'bool', 'Clean orphaned buildx builders', '清理孤兒 buildx builder', '孤立した buildx ビルダーを削除', { helpEn: 'Use only on a Docker daemon dedicated to runners.', helpZh: '僅適用於 runner 專用 Docker daemon。', helpJa: 'ランナー専用の Docker デーモンでのみ使用してください。' }),
  field('buildx-cleanup-ttl', 'duration', 'Buildx builder age limit', 'Buildx builder 保留時間', 'Buildx ビルダーの保持期間'),
  field('buildx-cleanup-interval', 'duration', 'Buildx cleanup interval', 'Buildx 清理間隔', 'Buildx のクリーンアップ間隔'),
  field('prune', 'bool', 'Prune Docker runtime objects', '清理 Docker 執行環境物件', 'Docker オブジェクトをクリーンアップ', { helpEn: 'Affects the entire daemon; use only on a dedicated runner daemon.', helpZh: '影響整個 daemon；僅適用於 runner 專用 daemon。', helpJa: 'デーモン全体に影響します。ランナー専用のデーモンでのみ使用してください。' }),
  field('prune-interval', 'duration', 'Docker prune interval', 'Docker 清理間隔', 'Docker のクリーンアップ間隔'),
  field('prune-ttl', 'duration', 'Stopped container and image age limit', '已停止容器與映像檔保留時間', '停止したコンテナとイメージの保持期間'),
  field('build-cache-max-age', 'duration', 'Docker build cache max age', 'Docker build cache 最長保留時間', 'Docker ビルドキャッシュの保持期間'),
  field('build-cache-budget', 'number', 'Docker build cache budget (GB)', 'Docker build cache 容量上限（GB）', 'Docker ビルドキャッシュの容量上限（GB）', { min: 0 }),
  field('actions-cache-url', 'url', 'External Actions cache URL', '外部 Actions 快取網址', '外部 Actions キャッシュの URL', { helpEn: 'Requires a cache-capable runner image; URL must end with /.', helpZh: '需要支援快取的 runner 映像檔；網址須以 / 結尾。', helpJa: 'キャッシュ対応のランナーイメージが必要です。URL の末尾は / にしてください。' }),
  field('native-actions-cache', 'bool', 'Use manager native Actions cache', '使用管理程序內建 Actions 快取', '管理プロセス内蔵の Actions キャッシュを使用', { helpEn: 'Also enable and configure the global native receiver.', helpZh: '還需啟用並設定全域內建接收端。', helpJa: 'グローバル設定の内蔵キャッシュ受信機能も有効にして設定してください。' }),
  field('cache-volumes', 'lines', 'Legacy cache volumes (name:/container/path per line)', '舊版 cache volumes（每行 name:/容器路徑）', '旧形式のキャッシュボリューム（1 行に 名前:/コンテナ内のパス）', { legacy: true }),
  field('cache', 'cache', 'Legacy budgeted cache volumes (name | path | budget | on-exceed)', '舊版有容量上限的 cache volumes（名稱 | 路徑 | 上限 | 超限處理）', '容量制限付きの旧形式キャッシュ（名前 | パス | 容量上限 | 超過時の処理）', { legacy: true, helpEn: 'One entry per line. Example: gradle | /home/runner/.gradle | 20GB | warn (or wipe)', helpZh: '每行一筆。例如：gradle | /home/runner/.gradle | 20GB | warn（或 wipe）', helpJa: '1 行に 1 件入力します。例：gradle | /home/runner/.gradle | 20GB | warn（または wipe）' }),
];

export const tartFields = [
  field('home', 'text', 'Tart home directory', 'Tart 主目錄', 'Tart のホームディレクトリ'),
  field('runner-dir', 'text', 'Runner directory inside VM', 'VM 內 runner 目錄', 'VM 内のランナーディレクトリ', { placeholder: '/Users/admin/actions-runner', helpEn: 'Default: /Users/admin/actions-runner for macOS guests, /home/admin/actions-runner for Linux guests.', helpZh: '預設：macOS guest 為 /Users/admin/actions-runner，Linux guest 為 /home/admin/actions-runner。', helpJa: '既定のパスは macOS ゲストでは /Users/admin/actions-runner、Linux ゲストでは /home/admin/actions-runner です。' }),
  field('cpu', 'number', 'VM CPU cores', 'VM CPU 核心數', 'VM の CPU コア数', { min: 0 }),
  field('memory', 'number', 'VM memory (MB)', 'VM 記憶體（MB）', 'VM のメモリ（MB）', { min: 0 }),
  field('pool-size', 'number', 'Pre-warmed VMs', '預先啟動的 VM 數量', '事前起動する VM 数', { min: 0, helpEn: 'Guest OS is detected from the image. macOS guests run at most two VMs per host; larger max-runners values are capped at two. Linux guests run up to max-runners plus pool-size VMs.', helpZh: '會從映像檔偵測 guest OS。macOS guest 每台主機最多兩台 VM，較大的 max-runners 值會限制為 2；Linux guest 最多執行 max-runners 加 pool-size 台 VM。', helpJa: 'ゲスト OS はイメージから判別します。macOS ゲストはホストあたり最大 2 台で、max-runners が大きくても 2 台に制限されます。Linux ゲストは max-runners と pool-size の合計台数まで起動します。' }),
  field('mtu', 'number', 'Guest MTU (0 = auto, -1 = off)', 'VM MTU（0 自動，-1 停用）', 'ゲストの MTU（0 で自動、-1 で無効）', { min: -1, max: 1500 }),
  field('dns', 'lines', 'Guest DNS IPs (one per line)', 'VM DNS IP（每行一個）', 'ゲストの DNS IP（1 行に 1 つ）'),
  field('net-bridged', 'text', 'Bridged host interface', '橋接的主機網路介面', 'ブリッジ接続するホストのインターフェース', { placeholder: 'en0' }),
  field('net-softnet', 'bool', 'Softnet network isolation', 'Softnet 網路隔離', 'Softnet によるネットワーク分離', { helpEn: 'Runs tart with --net-softnet. Cannot be combined with net-bridged. Softnet admits traffic to the host gateway unless a block rule matches it.', helpZh: '以 --net-softnet 執行 tart。不可與 net-bridged 同時使用。除非封鎖規則相符，Softnet 允許連到主機閘道。', helpJa: 'tart に --net-softnet を指定します。net-bridged とは併用できません。ブロックルールに一致しなければ、ホストのゲートウェイへの通信は許可されます。' }),
  field('net-softnet-allow', 'text', 'Softnet allow rules (comma-separated)', 'Softnet 允許規則（逗號分隔）', 'Softnet の許可ルール（カンマ区切り）', { placeholder: 'out proto udp @host port 53,out proto tcp @host port 53', helpEn: 'Passed to tart run --net-softnet-allow. Needs net-softnet.', helpZh: '傳給 tart run --net-softnet-allow。需要 net-softnet。', helpJa: 'tart run --net-softnet-allow に渡します。net-softnet が必要です。' }),
  field('net-softnet-block', 'text', 'Softnet block rules (comma-separated)', 'Softnet 封鎖規則（逗號分隔）', 'Softnet のブロックルール（カンマ区切り）', { placeholder: 'out @host', helpEn: 'Passed to tart run --net-softnet-block. Needs net-softnet. "out @host" keeps the guest off the host.', helpZh: '傳給 tart run --net-softnet-block。需要 net-softnet。"out @host" 使 VM 無法連到主機。', helpJa: 'tart run --net-softnet-block に渡します。net-softnet が必要です。"out @host" はゲストからホストへの通信を遮断します。' }),
  field('clone-min-free-gb', 'number', 'Minimum free GB before a clone', '複製 VM 前的最低可用空間（GB）', 'VM 複製前に必要な空き容量（GB）', { min: 0, helpEn: 'tart clone is refused when the Tart home filesystem has less free space. 0 disables.', helpZh: 'Tart 主目錄所在檔案系統可用空間不足時拒絕 tart clone。0 為停用。', helpJa: 'Tart のホームディレクトリがあるファイルシステムの空き容量が不足すると、tart clone を拒否します。0 で無効になります。' }),
  field('cache-cleanup', 'bool', 'Clean Tart OCI/IPSW cache', '清理 Tart OCI/IPSW 快取', 'Tart の OCI/IPSW キャッシュをクリーンアップ', { helpEn: 'Enabled by default; local VMs are not deleted.', helpZh: '預設啟用；不會刪除本機 VM。', helpJa: '既定で有効です。ローカルの VM は削除しません。' }),
  field('cache-max-age', 'duration', 'Tart cache max age', 'Tart 快取最長保留時間', 'Tart キャッシュの保持期間', { placeholder: '168h' }),
  field('cache-budget', 'number', 'Tart cache budget (GB)', 'Tart 快取容量上限（GB）', 'Tart キャッシュの容量上限（GB）', { min: 0 }),
  field('cache-cleanup-interval', 'duration', 'Tart cache cleanup interval', 'Tart 快取清理間隔', 'Tart キャッシュのクリーンアップ間隔', { placeholder: '24h' }),
];

export const retryFields = [
  field('initial', 'duration', 'Initial retry delay', '初次重試間隔', '最初の再試行までの待機時間', { placeholder: '5s' }),
  field('max', 'duration', 'Maximum retry delay', '最大重試間隔', '再試行の最大待機時間', { placeholder: '1m' }),
  field('cooldown-after', 'number', 'Failures before cooldown', '進入冷卻前的失敗次數', 'クールダウンに入るまでの失敗回数', { min: 1 }),
  field('cooldown', 'duration', 'Cooldown duration', '冷卻時間', 'クールダウンの時間', { placeholder: '5m' }),
];

export const actionsCacheFields = [
  field('enabled', 'bool', 'Enable native Actions cache receiver', '啟用內建 Actions 快取接收端', '内蔵 Actions キャッシュ受信機能を有効化'),
  field('listen-address', 'text', 'Private listen IP and port', '私人監聽 IP 與連接埠', 'プライベート IP と待受ポート', { placeholder: '172.18.0.1:32500' }),
  field('advertise-url', 'url', 'URL reachable by runner containers', 'Runner 容器可連線的網址', 'ランナーコンテナから接続できる URL', { placeholder: 'http://cache.internal:32500/' }),
  field('data-dir', 'text', 'Private host data directory', '主機上的私人資料目錄', 'ホスト上の非公開データディレクトリ', { placeholder: '/var/lib/runner/native-actions-cache' }),
  field('max-size-bytes', 'number', 'Maximum stored bytes', '最大儲存位元組數', '保存容量の上限（バイト）', { min: 1, placeholder: '53687091200' }),
  field('max-idle-days', 'number', 'Maximum idle days', '最長閒置天數', '未使用データの保持日数', { min: 0 }),
  field('token-issuer', 'url', 'Custom token issuer (advanced)', '自訂權杖簽發者（進階）', 'カスタムトークン発行者（詳細設定）'),
  field('jwks-url', 'url', 'Custom JWKS URL (advanced)', '自訂 JWKS 網址（進階）', 'カスタム JWKS URL（詳細設定）'),
  field('audiences', 'lines', 'Accepted audiences (one per line)', '接受的 audience（每行一個）', '許可する audience（1 行に 1 つ）'),
  field('upstream-results-url', 'url', 'Upstream results URL (advanced)', '上游 results 網址（進階）', '上流の results URL（詳細設定）'),
];

export const diskFields = [
  field('guard', 'bool', 'Enable disk-pressure guard', '啟用磁碟空間保護', 'ディスク空き容量の監視を有効化', { helpEn: 'Enabled by default.', helpZh: '預設啟用。', helpJa: '既定で有効です。' }),
  field('min-free', 'text', 'Start cleanup below free space', '低於此剩餘空間時開始清理', 'クリーンアップを開始する空き容量', { placeholder: '10%' }),
  field('target-free', 'text', 'Cleanup target free space', '清理目標剩餘空間', 'クリーンアップ後の目標空き容量', { placeholder: '20%' }),
  field('interval', 'duration', 'Disk check interval', '磁碟檢查間隔', 'ディスクの確認間隔', { placeholder: '1h' }),
  field('max-tier', 'number', 'Maximum cleanup tier (1–4)', '最高清理等級（1–4）', 'クリーンアップの最大レベル（1～4）', { min: 1, max: 4, helpEn: 'Tier 4 can wipe legacy cache volumes.', helpZh: '第 4 級可能清空舊版 cache volume。', helpJa: 'レベル 4 では旧形式のキャッシュボリュームを空にする場合があります。' }),
];

export const diagnosticsFields = [
  field('enabled', 'bool', 'Enable diagnostics', '啟用診斷', '診断を有効化'),
  field('capture-logs', 'bool', 'Capture runner logs', '擷取 runner 紀錄', 'ランナーログを収集'),
];

export const groups = { global: globalFields, set: setFields, docker: dockerFields, tart: tartFields, retry: retryFields, 'actions-cache': actionsCacheFields, disk: diskFields, diagnostics: diagnosticsFields };
